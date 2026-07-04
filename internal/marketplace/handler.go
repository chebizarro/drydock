// Package marketplace provides a review marketplace for community reviewers.
package marketplace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
		AssignmentID   string `json:"assignment_id"`
		PatchEventID   string `json:"patch_event_id"`
		RepoID         string `json:"repo_id"`
		ReviewerPubkey string `json:"reviewer_pubkey"`
		Priority       int    `json:"priority"`
		PriceSats      int64  `json:"price_sats"`
		Deadline       int64  `json:"deadline"`
		ExpiresAt      int64  `json:"expires_at"`
	}

	if err := json.Unmarshal([]byte(event.Content), &assignment); err != nil {
		h.logger.Warn("failed to parse assignment event",
			"event_id", event.ID.Hex(),
			"error", err,
		)
		return nil
	}
	assignmentEventID := assignment.AssignmentID
	if assignmentEventID == "" {
		assignmentEventID = event.ID.Hex()
	}
	expiresAt := assignment.ExpiresAt
	if expiresAt == 0 {
		expiresAt = assignment.Deadline
	}
	requesterPubkey, err := h.authorizeAssignmentIntent(ctx, event.PubKey.Hex(), assignment.PatchEventID, assignment.RepoID, assignment.PriceSats)
	if err != nil {
		return err
	}

	// Store the assignment
	err = h.store.CreateAssignment(ctx, db.ReviewAssignment{
		PatchEventID:      assignment.PatchEventID,
		RepoID:            assignment.RepoID,
		ReviewerPubkey:    assignment.ReviewerPubkey,
		RequesterPubkey:   requesterPubkey,
		Status:            "pending",
		Priority:          assignment.Priority,
		PriceSats:         assignment.PriceSats,
		AssignmentEventID: assignmentEventID,
		ExpiresAt:         expiresAt,
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

	var feedback ReviewFeedback

	if err := json.Unmarshal([]byte(event.Content), &feedback); err != nil {
		h.logger.Warn("failed to parse feedback event",
			"event_id", event.ID.Hex(),
			"error", err,
		)
		return nil
	}
	feedback.RaterPubkey = event.PubKey.Hex()
	feedback.CreatedAt = int64(event.CreatedAt)
	feedback.EventID = event.ID.Hex()

	// Store authorized, non-duplicate feedback.
	err := h.registry.RecordFeedback(ctx, feedback)
	if err != nil {
		h.logger.Error("failed to store feedback",
			"event_id", event.ID.Hex(),
			"error", err,
		)
		return err
	}

	metrics.MarketplaceFeedbackReceived.Inc()

	h.logger.Info("recorded review feedback",
		"event_id", event.ID.Hex(),
		"assignment_id", feedback.AssignmentID,
		"review_event_id", feedback.ReviewEventID,
		"rating", feedback.Rating,
	)

	return nil
}
