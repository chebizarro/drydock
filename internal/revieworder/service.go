// Package revieworder owns durable stored-patch review admission and queueing.
package revieworder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"drydock/internal/db"
	"drydock/internal/metrics"
	"drydock/internal/payment"
	"drydock/internal/repoconfig"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
)

const defaultQueueSize = 256

var (
	ErrQueueFull       = errors.New("review queue full")
	ErrNotMonitored    = errors.New("repository is not monitored")
	ErrSecurityCeiling = errors.New("repository is outside operator scope")
	ErrOrderConflict   = errors.New("review target is already in progress or permanently skipped")
	ErrPaymentDenied   = errors.New("review payment denied")
	ErrForceDenied     = errors.New("force requires repository maintainer or paid access")
)

// MonitoringRegistry is the live monitored-repository membership projection.
type MonitoringRegistry interface {
	Contains(repositoryAddress string) bool
}

// RepositoryConfigLoader reads policy from the canonical repository base.
type RepositoryConfigLoader interface {
	LoadBaseRepoConfig(ctx context.Context, repoID string) ([]byte, error)
}

// PaymentAuthorizer applies the shared patch payment policy.
type PaymentAuthorizer interface {
	AuthorizePatch(ctx context.Context, patchEvent nostr.Event, repoID string, policy repoconfig.PaymentsConfig) (payment.AuthorizeResult, error)
}

type Config struct {
	QueueSize int
}

type Service struct {
	store           *db.Store
	securityCeiling scope.Matcher
	monitoring      MonitoringRegistry
	configLoader    RepositoryConfigLoader
	paymentAuth     PaymentAuthorizer
	queue           chan db.ReviewTask
	logger          *slog.Logger
}

// SubmissionResult describes reactive admission without treating a durable skip
// as a transient handler failure.
type SubmissionResult struct {
	Acquired     bool
	Queued       bool
	RetryPending bool
	Skipped      bool
	SkipReason   string
}

// OnDemandRequest is the session-independent stored-patch intake used by IDE
// and ContextVM callers. The protocol handler is intentionally separate.
type OnDemandRequest struct {
	PatchEventID      string
	RepositoryAddress string
	RequesterPubkey   string
	OrderID           string
	RequestEventID    string
	Force             bool
	Invocation        db.ReviewInvocation
}

// AcceptedOrder reports durable acceptance independently from the in-memory
// wake-up hint.
type AcceptedOrder struct {
	Task         db.ReviewTask
	Queued       bool
	RetryPending bool
	Idempotent   bool
}

func New(
	cfg Config,
	store *db.Store,
	securityCeiling scope.Matcher,
	monitoring MonitoringRegistry,
	configLoader RepositoryConfigLoader,
	paymentAuth PaymentAuthorizer,
	logger *slog.Logger,
) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	queueSize := cfg.QueueSize
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	return &Service{
		store:           store,
		securityCeiling: securityCeiling,
		monitoring:      monitoring,
		configLoader:    configLoader,
		paymentAuth:     paymentAuth,
		queue:           make(chan db.ReviewTask, queueSize),
		logger:          logger,
	}
}

func (s *Service) Queue() <-chan db.ReviewTask {
	return s.queue
}

// SubmitReactive applies live monitoring and the static security ceiling,
// durably claims an eligible review, and emits the in-memory wake-up hint.
func (s *Service) SubmitReactive(ctx context.Context, patchEvent nostr.Event, repository scope.RepositoryRef) (SubmissionResult, error) {
	if s.store == nil {
		return SubmissionResult{}, errors.New("review order store is not configured")
	}
	if repository.Address == "" || repository.RepositoryID == "" || db.RepoIDFromPatch(patchEvent) != repository.RepositoryID {
		return SubmissionResult{}, errors.New("reactive patch repository does not match canonical repository reference")
	}
	if s.monitoring == nil || !s.monitoring.Contains(repository.Address) {
		return s.skipReactive(ctx, patchEvent.ID.Hex(), repository.RepositoryID, "not_monitored", ErrNotMonitored)
	}
	if s.securityCeiling.Enabled() && !s.securityCeiling.Allows(repository.RepositoryID, repository.OwnerPubkey) {
		s.logger.Info("skipping reactive review",
			"patch_event_id", patchEvent.ID.Hex(),
			"repo_id", repository.RepositoryID,
			"reason", "security_ceiling",
		)
		return SubmissionResult{Skipped: true, SkipReason: "security_ceiling"}, nil
	}

	task := db.ReviewTask{
		PatchEventID: patchEvent.ID.Hex(),
		RepoID:       repository.RepositoryID,
		Invocation:   db.ReviewInvocationReactive,
	}
	acquired, err := s.store.BeginReviewWithClaim(ctx, task.PatchEventID, task.RepoID, claimFromTask(task))
	if errors.Is(err, db.ErrReviewAlreadyPublished) {
		return SubmissionResult{}, nil
	}
	if err != nil {
		return SubmissionResult{}, err
	}
	if !acquired {
		return SubmissionResult{}, nil
	}
	if err := s.enqueueClaimed(ctx, task, "patch"); err != nil {
		if errors.Is(err, ErrQueueFull) {
			return SubmissionResult{Acquired: true, RetryPending: true}, err
		}
		return SubmissionResult{Acquired: true}, err
	}
	return SubmissionResult{Acquired: true, Queued: true}, nil
}

func (s *Service) skipReactive(ctx context.Context, patchEventID, repoID, reason string, admissionErr error) (SubmissionResult, error) {
	task := db.ReviewTask{PatchEventID: patchEventID, RepoID: repoID, Invocation: db.ReviewInvocationReactive}
	acquired, err := s.store.BeginReviewWithClaim(ctx, patchEventID, repoID, claimFromTask(task))
	if errors.Is(err, db.ErrReviewAlreadyPublished) {
		return SubmissionResult{Skipped: true, SkipReason: reason}, nil
	}
	if err != nil {
		return SubmissionResult{}, err
	}
	if acquired {
		if err := s.store.MarkReviewSkipped(ctx, patchEventID, repoID, reason); err != nil {
			return SubmissionResult{}, err
		}
	}
	s.logger.Info("skipping reactive review",
		"patch_event_id", patchEventID,
		"repo_id", repoID,
		"reason", reason,
		"admission_error", admissionErr,
	)
	return SubmissionResult{Skipped: true, SkipReason: reason}, nil
}

// SubmitOnDemand performs shared stored-patch admission. IDE callers use the
// IDE invocation; generic ContextVM orders additionally receive an atomic order
// receipt. Protocol-specific envelope/session validation remains with callers.
func (s *Service) SubmitOnDemand(ctx context.Context, req OnDemandRequest) (AcceptedOrder, error) {
	if s.store == nil {
		return AcceptedOrder{}, errors.New("review order store is not configured")
	}
	if req.Invocation != db.ReviewInvocationIDE && req.Invocation != db.ReviewInvocationContextVM {
		return AcceptedOrder{}, errors.New("on-demand invocation must be ide or contextvm")
	}
	req.PatchEventID = strings.TrimSpace(req.PatchEventID)
	req.RepositoryAddress = strings.TrimSpace(req.RepositoryAddress)
	req.RequesterPubkey = strings.TrimSpace(req.RequesterPubkey)
	req.OrderID = strings.TrimSpace(req.OrderID)
	req.RequestEventID = strings.TrimSpace(req.RequestEventID)
	if req.PatchEventID == "" || req.RequesterPubkey == "" || req.OrderID == "" {
		return AcceptedOrder{}, errors.New("patch event, requester, and order id are required")
	}

	patchRec, err := s.store.GetPatchEvent(ctx, req.PatchEventID)
	if err != nil {
		return AcceptedOrder{}, err
	}
	var patchEvent nostr.Event
	if err := json.Unmarshal([]byte(patchRec.RawEvent), &patchEvent); err != nil {
		return AcceptedOrder{}, fmt.Errorf("decode stored patch event: %w", err)
	}
	repository, err := repositoryRefFromPatch(patchEvent)
	if err != nil {
		return AcceptedOrder{}, err
	}
	if req.RepositoryAddress != "" {
		requested, err := scope.ParseRepositoryRef(req.RepositoryAddress)
		if err != nil {
			return AcceptedOrder{}, err
		}
		if requested.Address != repository.Address {
			return AcceptedOrder{}, errors.New("repository address does not match stored patch")
		}
	}
	owner, err := s.store.GetRepositoryOwnerPubkey(ctx, repository.RepositoryID)
	if err != nil {
		return AcceptedOrder{}, fmt.Errorf("load repository announcement: %w", err)
	}
	if s.securityCeiling.Enabled() && !s.securityCeiling.Allows(repository.RepositoryID, owner) {
		return AcceptedOrder{}, ErrSecurityCeiling
	}
	if s.configLoader == nil {
		return AcceptedOrder{}, errors.New("repository config loader is not configured")
	}
	rawConfig, err := s.configLoader.LoadBaseRepoConfig(ctx, repository.RepositoryID)
	if err != nil {
		return AcceptedOrder{}, fmt.Errorf("load repository policy: %w", err)
	}
	repoCfg := repoconfig.Default()
	if len(rawConfig) > 0 {
		parsed, parseErr := repoconfig.Parse(rawConfig)
		if parseErr != nil {
			if repoconfig.ContainsPaymentsConfig(rawConfig) {
				return AcceptedOrder{}, fmt.Errorf("%w: invalid_repo_payment_policy", ErrPaymentDenied)
			}
			s.logger.Warn("failed to parse .drydock.yaml for on-demand review, using defaults",
				"patch_event_id", req.PatchEventID, "repo_id", repository.RepositoryID, "error", parseErr)
		} else {
			repoCfg = parsed
		}
	}

	var authorization payment.AuthorizeResult
	if repoCfg.Payments.Enabled {
		if s.paymentAuth == nil {
			return AcceptedOrder{}, fmt.Errorf("%w: payment_service_not_configured", ErrPaymentDenied)
		}
		authorization, err = s.paymentAuth.AuthorizePatch(ctx, patchEvent, repository.RepositoryID, repoCfg.Payments)
		if err != nil {
			return AcceptedOrder{}, fmt.Errorf("authorize payment: %w", err)
		}
		if !authorization.Allowed {
			return AcceptedOrder{}, fmt.Errorf("%w: %s", ErrPaymentDenied, authorization.Reason)
		}
	}
	if req.Force {
		requester, err := scope.ParsePubkey(req.RequesterPubkey)
		if err != nil {
			return AcceptedOrder{}, ErrForceDenied
		}
		allowed, err := s.store.CanStatusAuthor(ctx, patchRec.RootID, repository.RepositoryID, requester)
		if err != nil {
			return AcceptedOrder{}, fmt.Errorf("authorize force: %w", err)
		}
		if !allowed && !payment.IsPaidAccessKind(authorization.AccessKind) {
			return AcceptedOrder{}, ErrForceDenied
		}
	}

	task := db.ReviewTask{
		PatchEventID:    patchRec.EventID,
		RepoID:          repository.RepositoryID,
		Force:           req.Force,
		Invocation:      req.Invocation,
		RequesterPubkey: req.RequesterPubkey,
		OrderID:         req.OrderID,
	}
	accepted := AcceptedOrder{Task: task}
	if req.Invocation == db.ReviewInvocationContextVM {
		if req.RequestEventID == "" {
			return AcceptedOrder{}, errors.New("contextvm request event id is required")
		}
		result, err := s.store.AcceptReviewOrder(ctx, db.ReviewOrderReceipt{
			RequesterPubkey:   req.RequesterPubkey,
			OrderID:           req.OrderID,
			RequestEventID:    req.RequestEventID,
			PatchEventID:      patchRec.EventID,
			RepositoryID:      repository.RepositoryID,
			RepositoryAddress: repository.Address,
			Force:             req.Force,
		}, claimFromTask(task))
		if err != nil {
			return AcceptedOrder{}, err
		}
		switch result.Disposition {
		case db.ReviewOrderConflict:
			return AcceptedOrder{}, ErrOrderConflict
		case db.ReviewOrderIdempotent:
			accepted.Idempotent = true
			if err := s.EnqueueRecovered(ctx, task, "contextvm_order_redelivery"); err != nil {
				if errors.Is(err, ErrQueueFull) {
					accepted.RetryPending = true
					return accepted, nil
				}
				return AcceptedOrder{}, err
			}
			accepted.Queued = true
			return accepted, nil
		}
	} else {
		acquired, err := s.store.BeginReviewWithClaim(ctx, task.PatchEventID, task.RepoID, claimFromTask(task))
		if errors.Is(err, db.ErrReviewAlreadyPublished) {
			return AcceptedOrder{}, ErrOrderConflict
		}
		if err != nil {
			return AcceptedOrder{}, err
		}
		if !acquired {
			return AcceptedOrder{}, ErrOrderConflict
		}
	}

	if err := s.enqueueClaimed(ctx, task, string(req.Invocation)); err != nil {
		if errors.Is(err, ErrQueueFull) {
			accepted.RetryPending = true
			return accepted, nil
		}
		return AcceptedOrder{}, err
	}
	accepted.Queued = true
	return accepted, nil
}

// EnqueueRecovered atomically reclaims a pending/failed durable row with its
// original invocation metadata before emitting a wake-up hint.
func (s *Service) EnqueueRecovered(ctx context.Context, task db.ReviewTask, source string) error {
	if task.Invocation == "" {
		task.Invocation = db.ReviewInvocationReactive
	}
	acquired, err := s.store.BeginReviewWithClaim(ctx, task.PatchEventID, task.RepoID, claimFromTask(task))
	if errors.Is(err, db.ErrReviewAlreadyPublished) {
		return nil
	}
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	return s.enqueueClaimed(ctx, task, source)
}

// EnqueueClaimed emits a task whose durable row was atomically claimed by the
// caller (currently zap receipts and the legacy IDE stored-patch path).
func (s *Service) EnqueueClaimed(ctx context.Context, task db.ReviewTask, source string) error {
	if task.Invocation == "" {
		task.Invocation = db.ReviewInvocationReactive
	}
	return s.enqueueClaimed(ctx, task, source)
}

// EnqueueReview preserves the narrow legacy IDE enqueuer interface until the
// IDE stored-patch path migrates to SubmitOnDemand.
func (s *Service) EnqueueReview(ctx context.Context, task db.ReviewTask, source string) error {
	return s.EnqueueClaimed(ctx, task, source)
}

func (s *Service) enqueueClaimed(ctx context.Context, task db.ReviewTask, source string) error {
	select {
	case s.queue <- task:
		metrics.ReviewQueuePushed.Inc()
		metrics.ReviewQueueDepth.Inc()
		s.logger.Info("queued patch review",
			"event_id", task.PatchEventID,
			"repo_id", task.RepoID,
			"invocation", task.Invocation,
			"source", source,
		)
		return nil
	default:
		metrics.ReviewQueueFull.Inc()
		s.logger.Warn("review queue full, marking task for retry",
			"event_id", task.PatchEventID,
			"repo_id", task.RepoID,
			"invocation", task.Invocation,
			"source", source,
		)
		if err := s.store.MarkReviewFailed(ctx, task.PatchEventID, task.RepoID, ErrQueueFull.Error()); err != nil {
			return errors.Join(ErrQueueFull, fmt.Errorf("mark review failed: %w", err))
		}
		return ErrQueueFull
	}
}

func claimFromTask(task db.ReviewTask) db.ReviewClaim {
	return db.ReviewClaim{
		Force:           task.Force,
		Invocation:      task.Invocation,
		RequesterPubkey: task.RequesterPubkey,
		OrderID:         task.OrderID,
	}
}

func repositoryRefFromPatch(event nostr.Event) (scope.RepositoryRef, error) {
	var repository scope.RepositoryRef
	count := 0
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "a" || !strings.HasPrefix(strings.TrimSpace(tag[1]), "30617:") {
			continue
		}
		ref, err := scope.ParseRepositoryRef(tag[1])
		if err != nil {
			return scope.RepositoryRef{}, err
		}
		count++
		repository = ref
	}
	if count != 1 {
		return scope.RepositoryRef{}, errors.New("patch must contain exactly one canonical 30617 repository address")
	}
	return repository, nil
}
