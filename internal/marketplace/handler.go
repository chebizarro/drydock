// Package marketplace provides a review marketplace for community reviewers.
package marketplace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"drydock/internal/contextvm"
	"drydock/internal/db"
	"drydock/internal/metrics"
	"drydock/internal/ratelimit"

	"fiatjaf.com/nostr"
)

// Handler processes marketplace events from the Nostr network.
type Handler struct {
	registry        *Registry
	router          *Router
	store           *db.Store
	logger          *slog.Logger
	feedbackLimiter *ratelimit.Limiter // rate limits feedback submissions
}

// NewHandler creates a new marketplace handler.
func NewHandler(registry *Registry, router *Router, store *db.Store, logger *slog.Logger) *Handler {
	return &Handler{
		registry:        registry,
		router:          router,
		store:           store,
		logger:          logger,
		feedbackLimiter: nil, // set via WithFeedbackLimiter
	}
}

// WithFeedbackLimiter sets a rate limiter for feedback submissions.
func (h *Handler) WithFeedbackLimiter(limiter *ratelimit.Limiter) *Handler {
	h.feedbackLimiter = limiter
	return h
}

// RegisterContextVMMethods registers marketplace intent handlers on a ContextVM router.
func (h *Handler) RegisterContextVMMethods(router *contextvm.Router) error {
	if router == nil {
		return errors.New("contextvm router is required")
	}
	return errors.Join(
		router.Register(MethodAssign, h.HandleAssignmentIntent),
		router.Register(MethodAccept, h.handleContextVMAcceptance),
		router.Register(MethodReject, h.handleContextVMRejection),
	)
}

// HandleEvent processes a marketplace event.
func (h *Handler) HandleEvent(ctx context.Context, event nostr.Event, relayURL string) error {
	switch event.Kind {
	case KindReviewerProfile:
		return h.handleReviewerProfile(ctx, event)
	case KindReviewFeedback:
		if tagValue(event.Tags, "t") != TagReviewFeedback {
			h.logger.Debug("ignoring non-marketplace NIP-90 feedback",
				"event_id", event.ID.Hex(),
			)
			return nil
		}
		return h.handleFeedback(ctx, event)
	default:
		h.logger.Debug("ignoring unknown marketplace event kind",
			"kind", int(event.Kind),
			"event_id", event.ID.Hex(),
		)
		return nil
	}
}

// handleReviewerProfile processes a reviewer profile announcement.
func (h *Handler) handleReviewerProfile(ctx context.Context, event nostr.Event) error {
	profile, ok, err := ParseReviewerProfileEvent(event)
	if err != nil {
		h.logger.Warn("failed to parse reviewer profile",
			"event_id", event.ID.Hex(),
			"error", err,
		)
		return nil // don't error on malformed events
	}
	if !ok {
		h.logger.Debug("ignoring non-drydock NIP-89 app handler",
			"event_id", event.ID.Hex(),
		)
		return nil
	}

	profile.Pubkey = event.PubKey.Hex()

	if err := h.registry.RegisterReviewer(ctx, profile, event.ID.Hex()); err != nil {
		h.logger.Error("failed to register reviewer",
			"pubkey", profile.Pubkey,
			"error", err,
		)
		return err
	}

	h.logger.Info("registered reviewer profile",
		"pubkey", profile.Pubkey,
		"languages", profile.Languages,
		"availability", profile.Availability,
	)

	return nil
}

// HandleAssignmentIntent processes a ContextVM marketplace/assign intent.
func (h *Handler) HandleAssignmentIntent(ctx context.Context, req contextvm.Request) (any, *contextvm.Error) {
	assignment, rpcErr := contextvm.ParamsAs[ReviewAssignment](req)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if assignment.AssignmentID == "" || assignment.PatchEventID == "" || assignment.RepoID == "" {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "assignment_id, patch_event_id, and repo_id are required"}
	}
	if assignment.ReviewerPubkey == "" {
		assignment.ReviewerPubkey = tagValue(req.Event.Tags, "p")
	}
	if assignment.CreatedAt == 0 {
		assignment.CreatedAt = int64(req.Event.CreatedAt)
	}
	assignmentEventID := req.Msg.ID
	if assignmentEventID == "" {
		assignmentEventID = req.Event.ID.Hex()
	}
	requesterPubkey, err := h.authorizeAssignmentIntent(ctx, req.Sender.Hex(), assignment.PatchEventID, assignment.RepoID, assignment.PriceSats)
	if err != nil {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidRequest, Message: err.Error()}
	}

	if err := h.store.CreateAssignment(ctx, db.ReviewAssignment{
		PatchEventID:      assignment.PatchEventID,
		RepoID:            assignment.RepoID,
		ReviewerPubkey:    assignment.ReviewerPubkey,
		RequesterPubkey:   requesterPubkey,
		Status:            "pending",
		Priority:          2,
		PriceSats:         assignment.PriceSats,
		AssignmentEventID: assignmentEventID,
		ExpiresAt:         assignment.Deadline,
	}); err != nil {
		h.logger.Error("failed to store contextvm assignment",
			"assignment_id", assignment.AssignmentID,
			"error", err,
		)
		return nil, &contextvm.Error{Code: contextvm.ErrorInternal, Message: err.Error()}
	}

	metrics.MarketplaceAssignmentsCreated.Inc()

	h.logger.Info("stored contextvm review assignment",
		"assignment_id", assignment.AssignmentID,
		"patch_event_id", assignment.PatchEventID,
		"reviewer", assignment.ReviewerPubkey,
	)

	return map[string]string{"status": "stored", "assignment_id": assignment.AssignmentID}, nil
}

// handleAssignment processes a legacy review assignment event for compatibility with older tests/helpers.
// Live assignment delivery uses ContextVM MethodAssign.
func (h *Handler) handleAssignment(ctx context.Context, event nostr.Event) error {
	var assignment ReviewAssignment
	if err := json.Unmarshal([]byte(event.Content), &assignment); err != nil {
		h.logger.Warn("failed to parse assignment event", "event_id", event.ID.Hex(), "error", err)
		return nil
	}
	assignmentEventID := assignment.AssignmentID
	if assignmentEventID == "" {
		assignmentEventID = event.ID.Hex()
	}
	requesterPubkey, err := h.authorizeAssignmentIntent(ctx, event.PubKey.Hex(), assignment.PatchEventID, assignment.RepoID, assignment.PriceSats)
	if err != nil {
		return err
	}
	return h.store.CreateAssignment(ctx, db.ReviewAssignment{
		PatchEventID:      assignment.PatchEventID,
		RepoID:            assignment.RepoID,
		ReviewerPubkey:    assignment.ReviewerPubkey,
		RequesterPubkey:   requesterPubkey,
		Status:            "pending",
		Priority:          2,
		PriceSats:         assignment.PriceSats,
		AssignmentEventID: assignmentEventID,
		ExpiresAt:         assignment.Deadline,
	})
}

func (h *Handler) authorizeAssignmentIntent(ctx context.Context, senderPubkey, patchEventID, repoID string, priceSats int64) (string, error) {
	if h.router == nil {
		return "", fmt.Errorf("marketplace assignment intent rejected: router authority is not configured")
	}
	authorityPubkey, err := h.router.AuthorityPubkey(ctx)
	if err != nil {
		return "", err
	}
	if senderPubkey != authorityPubkey {
		return "", fmt.Errorf("unauthorized assignment intent: sender %s is not router authority %s", senderPubkey, authorityPubkey)
	}

	if h.store == nil {
		return "", fmt.Errorf("marketplace assignment intent rejected: store is not configured")
	}
	if payment, err := h.store.GetReviewPayment(ctx, patchEventID); err == nil {
		if payment.Status != "authorized" {
			return "", fmt.Errorf("assignment intent rejected: payment for patch %s is %s, not authorized", patchEventID, payment.Status)
		}
		if repoID != "" && payment.RepoID != "" && payment.RepoID != repoID {
			return "", fmt.Errorf("assignment intent rejected: payment repo %s does not match assignment repo %s", payment.RepoID, repoID)
		}
		if payment.AuthorPubkey == "" {
			return "", fmt.Errorf("assignment intent rejected: authorized payment has no author pubkey")
		}
		return payment.AuthorPubkey, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("check assignment payment authorization: %w", err)
	}

	if priceSats > 0 {
		return "", fmt.Errorf("assignment intent rejected: paid assignment for patch %s has no authorized payment record", patchEventID)
	}
	patchAuthor, err := h.store.GetPatchAuthorPubKey(ctx, patchEventID)
	if err != nil {
		return "", fmt.Errorf("assignment intent rejected: patch author is not known: %w", err)
	}
	if patchAuthor == "" {
		return "", fmt.Errorf("assignment intent rejected: patch author is empty")
	}
	return patchAuthor, nil
}

// handleContextVMAcceptance processes a ContextVM assignment acceptance intent.
func (h *Handler) handleContextVMAcceptance(ctx context.Context, req contextvm.Request) (any, *contextvm.Error) {
	if _, rpcErr := contextvm.ParamsAs[ReviewAcceptance](req); rpcErr != nil {
		return nil, rpcErr
	}
	event := req.Event
	event.Content = string(req.Msg.Params)
	event.PubKey = req.Sender
	if err := h.router.HandleAcceptance(ctx, event); err != nil {
		h.logger.Error("failed to handle marketplace acceptance intent",
			"event_id", req.Event.ID.Hex(),
			"error", err,
		)
		return nil, &contextvm.Error{Code: contextvm.ErrorInternal, Message: err.Error()}
	}
	return map[string]string{"status": "accepted"}, nil
}

// handleContextVMRejection processes a ContextVM assignment rejection intent.
func (h *Handler) handleContextVMRejection(ctx context.Context, req contextvm.Request) (any, *contextvm.Error) {
	if _, rpcErr := contextvm.ParamsAs[ReviewRejection](req); rpcErr != nil {
		return nil, rpcErr
	}
	event := req.Event
	event.Content = string(req.Msg.Params)
	event.PubKey = req.Sender
	if err := h.router.HandleRejection(ctx, event); err != nil {
		h.logger.Error("failed to handle marketplace rejection intent",
			"event_id", req.Event.ID.Hex(),
			"error", err,
		)
		return nil, &contextvm.Error{Code: contextvm.ErrorInternal, Message: err.Error()}
	}
	return map[string]string{"status": "rejected"}, nil
}

// handleFeedback processes review feedback/rating events.
func (h *Handler) handleFeedback(ctx context.Context, event nostr.Event) error {
	senderPubkey := event.PubKey.Hex()

	// Check per-user rate limit for feedback submissions.
	if h.feedbackLimiter != nil {
		result, err := h.feedbackLimiter.Allow(ctx, senderPubkey)
		if err != nil {
			h.logger.Warn("feedback rate limit check failed", "error", err)
			// Continue on error - fail open
		} else if !result.Allowed {
			h.logger.Info("feedback rate limited",
				"sender", senderPubkey,
				"reset_at", result.ResetAt,
			)
			return nil // Silently drop rate-limited feedback
		}
	}

	feedback, err := ParseReviewFeedbackEvent(event)
	if err != nil {
		// Legacy compatibility: older callers/tests send feedback as JSON content
		// without the NIP-90 t/status/rating tags.
		if jsonErr := json.Unmarshal([]byte(event.Content), &feedback); jsonErr != nil {
			h.logger.Warn("failed to parse feedback event",
				"event_id", event.ID.Hex(),
				"error", err,
			)
			return nil
		}
	}
	feedback.RaterPubkey = event.PubKey.Hex()
	feedback.CreatedAt = int64(event.CreatedAt)
	feedback.EventID = event.ID.Hex()

	if err := h.registry.RecordFeedback(ctx, feedback); err != nil {
		h.logger.Error("failed to store feedback",
			"event_id", event.ID.Hex(),
			"error", err,
		)
		return err
	}

	metrics.MarketplaceFeedbackReceived.Inc()

	h.logger.Info("recorded review feedback",
		"event_id", event.ID.Hex(),
		"review_event_id", feedback.ReviewEventID,
		"rating", feedback.Rating,
	)

	return nil
}
