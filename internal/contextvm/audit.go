package contextvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"drydock/internal/auditengine"
	"drydock/internal/eventkind"
	"drydock/internal/repoconfig"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
)

const (
	MethodSecurityAudit      = "security/audit"
	MethodSecurityAuditSARIF = "security/audit/sarif"
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

type AuditFeedbackPublisher interface {
	Publish(context.Context, []string, nostr.Event) error
}

// AuditFeedbackReporter publishes NIP-90 kind 7000 progress for an audit.
type AuditFeedbackReporter struct {
	signer    Signer
	publisher AuditFeedbackPublisher
	relays    []string
}

func NewAuditFeedbackReporter(signer Signer, publisher AuditFeedbackPublisher, relays []string) *AuditFeedbackReporter {
	return &AuditFeedbackReporter{signer: signer, publisher: publisher, relays: append([]string(nil), relays...)}
}

func (r *AuditFeedbackReporter) ReportAuditProgress(ctx context.Context, auditID int64, phase string) error {
	status := "processing"
	if phase == "published" {
		status = "success"
	}
	return r.publish(ctx, auditID, phase, status, "")
}

func (r *AuditFeedbackReporter) ReportAuditFailure(ctx context.Context, auditID int64, err error) error {
	message := "security audit failed"
	if err != nil {
		message = err.Error()
	}
	return r.publish(ctx, auditID, "failed", "error", message)
}

func (r *AuditFeedbackReporter) publish(ctx context.Context, auditID int64, phase, status, message string) error {
	if r == nil || r.signer == nil || r.publisher == nil {
		return errors.New("audit feedback publisher is not configured")
	}
	target, ok := ctx.Value(auditFeedbackContextKey{}).(auditFeedbackTarget)
	if !ok || target.RequestEventID == "" || target.Requester == nostr.ZeroPK {
		return errors.New("audit feedback target is missing")
	}
	relays := append([]string(nil), target.Relays...)
	if len(relays) == 0 {
		relays = append(relays, r.relays...)
	}
	content, err := json.Marshal(struct {
		AuditID int64  `json:"audit_id"`
		Phase   string `json:"phase"`
		Status  string `json:"status"`
		Message string `json:"message,omitempty"`
	}{AuditID: auditID, Phase: phase, Status: status, Message: message})
	if err != nil {
		return err
	}
	event := nostr.Event{
		Kind:      eventkind.ReviewFeedback,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", target.RequestEventID},
			{"p", target.Requester.Hex()},
			{"status", status},
			{"phase", phase},
			{"audit", strconv.FormatInt(auditID, 10)},
			{"t", "security-audit"},
		},
		Content: string(content),
	}
	if err := r.signer.SignEvent(ctx, &event); err != nil {
		return fmt.Errorf("sign audit feedback: %w", err)
	}
	if err := r.publisher.Publish(ctx, relays, event); err != nil {
		return fmt.Errorf("publish audit feedback: %w", err)
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
