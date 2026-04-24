// Package marketplace provides a review marketplace for community reviewers.
package marketplace

import (
	"context"
	"encoding/json"
	"log/slog"

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

// HandleEvent processes a marketplace event.
func (h *Handler) HandleEvent(ctx context.Context, event nostr.Event, relayURL string) error {
	switch event.Kind {
	case KindReviewerProfile:
		return h.handleReviewerProfile(ctx, event)
	case KindReviewAssignment:
		return h.handleAssignment(ctx, event)
	case KindReviewAcceptance:
		return h.handleAcceptance(ctx, event)
	case KindReviewRejection:
		return h.handleRejection(ctx, event)
	case KindReviewFeedback:
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
	var profile ReviewerProfile
	if err := json.Unmarshal([]byte(event.Content), &profile); err != nil {
		h.logger.Warn("failed to parse reviewer profile",
			"event_id", event.ID.Hex(),
			"error", err,
		)
		return nil // don't error on malformed events
	}

	profile.Pubkey = event.PubKey.Hex()

	// Extract d-tag for identifier (standard NIP-33 replaceable event)
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "d" {
			// d-tag is the stable identifier; pubkey is still the key
			break
		}
	}

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

// handleAssignment processes a review assignment event.
func (h *Handler) handleAssignment(ctx context.Context, event nostr.Event) error {
	// Assignment events are created by the router, not ingested externally.
	// This handler is for assignments created by other nodes in the network.
	var assignment struct {
		PatchEventID   string `json:"patch_event_id"`
		RepoID         string `json:"repo_id"`
		ReviewerPubkey string `json:"reviewer_pubkey"`
		Priority       int    `json:"priority"`
		PriceSats      int64  `json:"price_sats"`
		ExpiresAt      int64  `json:"expires_at"`
	}

	if err := json.Unmarshal([]byte(event.Content), &assignment); err != nil {
		h.logger.Warn("failed to parse assignment event",
			"event_id", event.ID.Hex(),
			"error", err,
		)
		return nil
	}

	// Store the assignment
	err := h.store.CreateAssignment(ctx, db.ReviewAssignment{
		PatchEventID:      assignment.PatchEventID,
		RepoID:            assignment.RepoID,
		ReviewerPubkey:    assignment.ReviewerPubkey,
		RequesterPubkey:   event.PubKey.Hex(),
		Status:            "pending",
		Priority:          assignment.Priority,
		PriceSats:         assignment.PriceSats,
		AssignmentEventID: event.ID.Hex(),
		ExpiresAt:         assignment.ExpiresAt,
	})
	if err != nil {
		h.logger.Error("failed to store assignment",
			"event_id", event.ID.Hex(),
			"error", err,
		)
		return err
	}

	metrics.MarketplaceAssignmentsCreated.Inc()

	h.logger.Info("stored review assignment",
		"event_id", event.ID.Hex(),
		"patch_event_id", assignment.PatchEventID,
		"reviewer", assignment.ReviewerPubkey,
	)

	return nil
}

// handleAcceptance processes an assignment acceptance event.
func (h *Handler) handleAcceptance(ctx context.Context, event nostr.Event) error {
	return h.router.HandleAcceptance(ctx, event)
}

// handleRejection processes an assignment rejection event.
func (h *Handler) handleRejection(ctx context.Context, event nostr.Event) error {
	return h.router.HandleRejection(ctx, event)
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

	var feedback struct {
		AssignmentID int    `json:"assignment_id"`
		Rating       int    `json:"rating"`
		Comment      string `json:"comment"`
	}

	if err := json.Unmarshal([]byte(event.Content), &feedback); err != nil {
		h.logger.Warn("failed to parse feedback event",
			"event_id", event.ID.Hex(),
			"error", err,
		)
		return nil
	}

	// Get the assignment to find the reviewer
	assignment, err := h.store.GetAssignmentByID(ctx, feedback.AssignmentID)
	if err != nil {
		h.logger.Warn("feedback references unknown assignment",
			"assignment_id", feedback.AssignmentID,
			"event_id", event.ID.Hex(),
		)
		return nil
	}

	// Store the feedback
	err = h.store.RecordFeedback(ctx, db.ReviewFeedback{
		AssignmentID:   feedback.AssignmentID,
		ReviewerPubkey: assignment.ReviewerPubkey,
		RaterPubkey:    event.PubKey.Hex(),
		Rating:         feedback.Rating,
		Comment:        feedback.Comment,
		EventID:        event.ID.Hex(),
	})
	if err != nil {
		h.logger.Error("failed to store feedback",
			"event_id", event.ID.Hex(),
			"error", err,
		)
		return err
	}

	metrics.MarketplaceFeedbackReceived.Inc()

	// Recalculate reviewer's reputation
	if err := h.registry.RecalculateReputation(ctx, assignment.ReviewerPubkey); err != nil {
		h.logger.Error("failed to recalculate reputation",
			"reviewer", assignment.ReviewerPubkey,
			"error", err,
		)
	}

	h.logger.Info("recorded review feedback",
		"event_id", event.ID.Hex(),
		"reviewer", assignment.ReviewerPubkey,
		"rating", feedback.Rating,
	)

	return nil
}
