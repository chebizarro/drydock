package contextvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"drydock/internal/auditengine"
	"drydock/internal/metrics"
	"drydock/internal/repoconfig"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
)

const (
	MethodSecurityAudit         = "security/audit"
	MethodSecurityAuditSARIF    = "security/audit/sarif"
	MethodSecurityAuditProgress = "security/audit/progress"
)

type SecurityAuditParams struct {
	RepoAddr    string `json:"repo_addr"`
	Subtree     string `json:"subtree,omitempty"`
	Ref         string `json:"ref,omitempty"`
	Depth       string `json:"depth,omitempty"`
	SinceCommit string `json:"since_commit,omitempty"`
}

type SecurityAuditAccepted struct {
	Accepted       bool   `json:"accepted"`
	RequestEventID string `json:"request_event_id"`
}

type SecurityAuditSARIFParams struct {
	AuditID int64 `json:"audit_id"`
}

type SecurityAuditSARIFResult struct {
	AuditID int64           `json:"audit_id"`
	SHA256  string          `json:"sha256"`
	SARIF   json.RawMessage `json:"sarif"`
}

// SecurityAuditProgress is the notification payload published as an audit
// advances. Event IDs deduplicate exact repeats; OccurredAt and then event ID
// define a deterministic relay-delivery order.
type SecurityAuditProgress struct {
	AuditID        int64  `json:"audit_id"`
	RequestEventID string `json:"request_event_id"`
	Phase          string `json:"phase"`
	Status         string `json:"status"`
	Message        string `json:"message,omitempty"`
	OccurredAt     int64  `json:"occurred_at"`
}

// ShouldApplySecurityAuditProgress applies the consumer ordering contract.
// Terminal states never regress to processing, even if a processing event is
// delivered later by a relay.
func ShouldApplySecurityAuditProgress(current SecurityAuditProgress, currentEventID string, candidate SecurityAuditProgress, candidateEventID string) bool {
	if candidateEventID == "" || candidateEventID == currentEventID {
		return false
	}
	currentTerminal := isTerminalAuditStatus(current.Status)
	candidateTerminal := isTerminalAuditStatus(candidate.Status)
	if currentTerminal && !candidateTerminal {
		return false
	}
	if !currentTerminal && candidateTerminal {
		return true
	}
	if currentEventID == "" {
		return true
	}
	if candidate.OccurredAt != current.OccurredAt {
		return candidate.OccurredAt > current.OccurredAt
	}
	return candidateEventID > currentEventID
}

func isTerminalAuditStatus(status string) bool {
	return status == "success" || status == "error"
}

type SecurityAuditRunner interface {
	Run(context.Context, auditengine.Request) (auditengine.Result, error)
}

type SecurityAuditStore interface {
	GetRepositoryAnnouncement(context.Context, string) (nostr.Event, error)
	GetRepositoryCloneURLs(context.Context, string) ([]string, error)
	SecurityAuditSARIFForRequester(context.Context, int64, string) ([]byte, string, error)
}

type SecurityAuditConfigLoader interface {
	LoadBaseRepoConfig(context.Context, string) ([]byte, error)
}

type auditFeedbackTarget struct {
	RequestEventID string
	Requester      nostr.PubKey
	Relays         []string
}

type auditFeedbackContextKey struct{}

type AuditProgressNotifier interface {
	Notify(context.Context, Notification) (string, error)
}

// AuditFeedbackReporter publishes one-way ContextVM audit progress notifications.
type AuditFeedbackReporter struct {
	notifier AuditProgressNotifier
	relays   []string
}

func NewAuditFeedbackReporter(notifier AuditProgressNotifier, relays []string) *AuditFeedbackReporter {
	return &AuditFeedbackReporter{notifier: notifier, relays: append([]string(nil), relays...)}
}

func (r *AuditFeedbackReporter) ReportAuditProgress(ctx context.Context, auditID int64, phase string) error {
	status := "processing"
	if phase == "published" {
		status = "success"
	}
	return r.publish(ctx, auditID, phase, status, "")
}

func (r *AuditFeedbackReporter) ReportAuditFailure(ctx context.Context, auditID int64, _ error) error {
	// Detailed failures remain in server logs and durable audit state; avoid
	// exposing repository/security internals in relay-visible notification content.
	return r.publish(ctx, auditID, "failed", "error", "security audit failed")
}

func (r *AuditFeedbackReporter) publish(ctx context.Context, auditID int64, phase, status, message string) error {
	if r == nil || r.notifier == nil {
		return errors.New("audit progress notifier is not configured")
	}
	target, ok := ctx.Value(auditFeedbackContextKey{}).(auditFeedbackTarget)
	if !ok || target.RequestEventID == "" || target.Requester == nostr.ZeroPK {
		return errors.New("audit feedback target is missing")
	}
	relays := append([]string(nil), target.Relays...)
	if len(relays) == 0 {
		relays = append(relays, r.relays...)
	}
	progress := SecurityAuditProgress{
		AuditID: auditID, RequestEventID: target.RequestEventID, Phase: phase,
		Status: status, Message: message, OccurredAt: time.Now().Unix(),
	}
	if _, err := r.notifier.Notify(ctx, Notification{
		Method: MethodSecurityAuditProgress, Params: progress,
		Recipients: []nostr.PubKey{target.Requester}, RelatedEventID: target.RequestEventID,
		Relays: relays,
	}); err != nil {
		metrics.SecurityAuditProgressNotificationFailures.Inc()
		return fmt.Errorf("publish audit progress notification: %w", err)
	}
	return nil
}

// SecurityAuditHandler registers and executes the security/audit ContextVM method.
type SecurityAuditHandler struct {
	runner        SecurityAuditRunner
	store         SecurityAuditStore
	configLoader  SecurityAuditConfigLoader
	feedback      *AuditFeedbackReporter
	defaultRelays []string
	logger        *slog.Logger
	start         func(func())
}

func NewSecurityAuditHandler(runner SecurityAuditRunner, store SecurityAuditStore, configLoader SecurityAuditConfigLoader, feedback *AuditFeedbackReporter, defaultRelays []string, logger *slog.Logger) *SecurityAuditHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &SecurityAuditHandler{
		runner: runner, store: store, configLoader: configLoader, feedback: feedback,
		defaultRelays: append([]string(nil), defaultRelays...), logger: logger,
		start: func(run func()) { go run() },
	}
}

func (h *SecurityAuditHandler) RegisterContextVMMethods(router *Router) error {
	if router == nil {
		return errors.New("contextvm router is required")
	}
	if err := router.Register(MethodSecurityAudit, h.HandleSecurityAudit); err != nil {
		return err
	}
	return router.Register(MethodSecurityAuditSARIF, h.HandleSecurityAuditSARIF)
}

func (h *SecurityAuditHandler) HandleSecurityAudit(ctx context.Context, req Request) (any, *Error) {
	if h == nil || h.runner == nil || h.store == nil {
		return nil, &Error{Code: ErrorInternal, Message: "security audit is not configured"}
	}
	params, rpcErr := ParamsAs[SecurityAuditParams](req)
	if rpcErr != nil {
		return nil, rpcErr
	}
	repository, err := scope.ParseRepositoryRef(params.RepoAddr)
	if err != nil {
		message := "repo_addr must be a 30617:<pubkey>:<identifier> address"
		if errors.Is(err, scope.ErrInvalidRepositoryPubkey) {
			message = "repo_addr contains an invalid pubkey"
		}
		return nil, &Error{Code: ErrorInvalidParams, Message: message}
	}
	repoID := repository.RepositoryID
	depth, repoCfg, err := h.auditConfig(ctx, repoID, params.Depth)
	if err != nil {
		return nil, &Error{Code: ErrorInvalidParams, Message: err.Error()}
	}
	announcement, err := h.store.GetRepositoryAnnouncement(ctx, repoID)
	if err != nil {
		return nil, &Error{Code: ErrorNotFound, Message: err.Error()}
	}
	cloneURLs, err := h.store.GetRepositoryCloneURLs(ctx, repoID)
	if err != nil || len(cloneURLs) == 0 {
		if err == nil {
			err = errors.New("repository has no clone URLs")
		}
		return nil, &Error{Code: ErrorNotFound, Message: err.Error()}
	}
	requestEventID := req.Event.ID.Hex()
	if req.Event.ID == nostr.ZeroID {
		return nil, &Error{Code: ErrorInvalidRequest, Message: "request event id is required"}
	}
	relays := dedupeStrings(append(append([]string{}, req.Relay), h.defaultRelays...))
	target := auditFeedbackTarget{RequestEventID: requestEventID, Requester: req.Sender, Relays: relays}
	runCtx := context.WithValue(ctx, auditFeedbackContextKey{}, target)
	auditReq := auditengine.Request{
		RepoID: repoID, CloneURLs: cloneURLs, Ref: strings.TrimSpace(params.Ref),
		Depth: depth, RequestedBy: req.Sender.Hex(), Subtree: strings.TrimSpace(params.Subtree),
		SinceCommit: strings.TrimSpace(params.SinceCommit), EnableSCA: repoCfg.Security.SCA,
		EnableSecrets: repoCfg.Security.SecretScan, Localizer: repoCfg.Security.Audit.Localizer,
		Nostr:        repoCfg.Security.Nostr,
		Announcement: announcement, Requester: req.Sender, Relays: relays,
	}
	h.start(func() {
		result, runErr := h.runner.Run(runCtx, auditReq)
		if runErr == nil {
			return
		}
		h.logger.Error("security audit failed", "request_event_id", requestEventID, "audit_id", result.AuditID, "error", runErr)
		if h.feedback != nil {
			if err := h.feedback.ReportAuditFailure(context.WithoutCancel(runCtx), result.AuditID, runErr); err != nil {
				h.logger.Warn("security audit failure feedback publish failed", "request_event_id", requestEventID, "error", err)
			}
		}
	})
	return SecurityAuditAccepted{Accepted: true, RequestEventID: requestEventID}, nil
}

func (h *SecurityAuditHandler) HandleSecurityAuditSARIF(ctx context.Context, req Request) (any, *Error) {
	if h == nil || h.store == nil {
		return nil, &Error{Code: ErrorInternal, Message: "security audit is not configured"}
	}
	params, rpcErr := ParamsAs[SecurityAuditSARIFParams](req)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if params.AuditID <= 0 {
		return nil, &Error{Code: ErrorInvalidParams, Message: "audit_id must be positive"}
	}
	if req.Sender == nostr.ZeroPK {
		return nil, &Error{Code: ErrorUnauthorized, Message: "requester pubkey is required"}
	}
	sarif, hash, err := h.store.SecurityAuditSARIFForRequester(ctx, params.AuditID, req.Sender.Hex())
	if err != nil {
		return nil, &Error{Code: ErrorNotFound, Message: "security audit SARIF not found"}
	}
	if !json.Valid(sarif) {
		return nil, &Error{Code: ErrorInternal, Message: "stored security audit SARIF is invalid"}
	}
	return SecurityAuditSARIFResult{
		AuditID: params.AuditID,
		SHA256:  hash,
		SARIF:   json.RawMessage(append([]byte(nil), sarif...)),
	}, nil
}

func (h *SecurityAuditHandler) auditConfig(ctx context.Context, repoID, requestedDepth string) (auditengine.Depth, repoconfig.RepoConfig, error) {
	cfg := repoconfig.Default()
	if h.configLoader != nil {
		raw, err := h.configLoader.LoadBaseRepoConfig(ctx, repoID)
		if err != nil {
			h.logger.Warn("failed to load repository config for security audit; using defaults", "repo_id", repoID, "error", err)
		} else if parsed, err := repoconfig.Parse(raw); err != nil {
			h.logger.Warn("invalid repository config for security audit; using defaults", "repo_id", repoID, "error", err)
		} else {
			cfg = parsed
		}
	}
	value := strings.ToLower(strings.TrimSpace(requestedDepth))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(cfg.Security.Audit.Depth))
	}
	if value == "" {
		value = string(auditengine.DepthStandard)
	}
	depth := auditengine.Depth(value)
	if _, err := auditengine.BudgetForDepth(depth); err != nil {
		return "", cfg, err
	}
	return depth, cfg, nil
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
