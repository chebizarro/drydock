// Package marketplace provides a review marketplace for community reviewers.
package marketplace

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

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
		router.Register(MethodComplete, h.handleContextVMCompletion),
		router.RegisterNotification(MethodFeedback, h.handleContextVMFeedback),
	)
}

// HandleEvent processes a marketplace event.
func (h *Handler) HandleEvent(ctx context.Context, event nostr.Event, relayURL string) error {
	switch event.Kind {
	case KindReviewerProfile:
		return h.handleReviewerProfile(ctx, event)
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
	requesterPubkey, err := h.authorizeAssignmentIntent(ctx, req.Sender.Hex(), assignment.PatchEventID, assignment.RepoID, assignment.RequesterPubkey, assignment.PriceSats)
	if err != nil {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidRequest, Message: err.Error()}
	}

	if err := h.store.UpsertAssignmentReceipt(ctx, db.ReviewAssignment{
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
		code := contextvm.ErrorInternal
		if errors.Is(err, db.ErrAssignmentEscrow) {
			code = contextvm.ErrorInvalidRequest
		}
		return nil, &contextvm.Error{Code: code, Message: err.Error()}
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
	requesterPubkey, err := h.authorizeAssignmentIntent(ctx, event.PubKey.Hex(), assignment.PatchEventID, assignment.RepoID, assignment.RequesterPubkey, assignment.PriceSats)
	if err != nil {
		return err
	}
	return h.store.UpsertAssignmentReceipt(ctx, db.ReviewAssignment{
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

func (h *Handler) authorizeAssignmentIntent(ctx context.Context, senderPubkey, patchEventID, repoID, requesterPubkey string, priceSats int64) (string, error) {
	if priceSats < 0 {
		return "", fmt.Errorf("assignment intent rejected: price cannot be negative")
	}
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
		if payment.RepoID != repoID {
			return "", fmt.Errorf("assignment intent rejected: payment repo %s does not match assignment repo %s", payment.RepoID, repoID)
		}
		if payment.AuthorPubkey == "" {
			return "", fmt.Errorf("assignment intent rejected: authorized payment has no author pubkey")
		}
		if requesterPubkey != "" && requesterPubkey != payment.AuthorPubkey {
			return "", fmt.Errorf("assignment intent rejected: payment requester %s does not match assignment requester %s", payment.AuthorPubkey, requesterPubkey)
		}
		if priceSats > 0 {
			if payment.AccessKind != "cashu_review" || payment.RequestedMode != "review" {
				return "", fmt.Errorf("assignment intent rejected: payment access kind %s cannot fund marketplace payout", payment.AccessKind)
			}
			if payment.SettledAmountSats < priceSats {
				return "", fmt.Errorf("assignment intent rejected: price %d exceeds settled funds %d", priceSats, payment.SettledAmountSats)
			}
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

// handleContextVMCompletion authenticates assignment completion and starts/reconciles payout.
func (h *Handler) handleContextVMCompletion(ctx context.Context, req contextvm.Request) (any, *contextvm.Error) {
	completion, rpcErr := contextvm.ParamsAs[ReviewCompletion](req)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if completion.AssignmentID == "" || completion.ReviewEventID == "" {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "assignment_id and review_event_id are required"}
	}
	if req.Event.PubKey == nostr.ZeroPK || req.Sender != req.Event.PubKey ||
		!req.Event.CheckID() || !req.Event.VerifySignature() {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidRequest, Message: "completion event failed signature verification"}
	}
	if err := h.router.complete(ctx, completion, req.Sender.Hex(), req.Event.ID.Hex()); err != nil {
		h.logger.Error("failed to handle marketplace completion intent",
			"event_id", req.Event.ID.Hex(), "error", err)
		return nil, &contextvm.Error{Code: contextvm.ErrorInternal, Message: err.Error()}
	}
	return map[string]string{"status": "completed", "review_event_id": completion.ReviewEventID}, nil
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

const (
	feedbackNotificationMaxLifetime = 15 * time.Minute
	feedbackCommentMaxBytes         = 4096
)

// handleContextVMFeedback processes a notification-only marketplace rating.
// Invalid, expired, unauthorized, and rate-limited notifications are handled
// rejections. Transient limiter/store failures are returned so ingest remains
// incomplete and relay redelivery can retry them.
func (h *Handler) handleContextVMFeedback(ctx context.Context, req contextvm.Request) error {
	senderPubkey := req.Sender.Hex()
	if h.feedbackLimiter != nil {
		result, err := h.feedbackLimiter.Allow(ctx, senderPubkey)
		if err != nil {
			metrics.FeedbackRateLimitFailures.Inc()
			h.logger.Error("feedback rate limit check failed", "sender", senderPubkey, "error", err)
			return fmt.Errorf("check feedback rate limit: %w", err)
		}
		if !result.Allowed {
			metrics.FeedbackRateLimited.Inc()
			h.logger.Info("feedback rate limited", "sender", senderPubkey, "reset_at", result.ResetAt)
			return nil
		}
	}

	params, err := parseMarketplaceFeedbackParams(req.Msg.Params)
	if err == nil {
		err = validateMarketplaceFeedbackEnvelope(req, params)
	}
	if err != nil {
		metrics.MarketplaceFeedbackMalformed.Inc()
		h.logger.Warn("rejected malformed marketplace feedback notification", "event_id", req.Event.ID.Hex(), "error", err)
		return nil
	}
	if h.registry == nil {
		return errors.New("marketplace feedback registry is not configured")
	}

	inserted, err := h.registry.RecordFeedback(ctx, ReviewFeedback{
		ReviewEventID: params.ReviewEventID,
		RaterPubkey:   senderPubkey,
		Rating:        params.Rating, Helpful: params.Helpful, Accurate: params.Accurate,
		Comment: params.Comment, EventID: req.Event.ID.Hex(), CreatedAt: int64(req.Event.CreatedAt),
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrFeedbackUnauthorized):
			metrics.MarketplaceFeedbackUnauthorized.Inc()
			h.logger.Warn("rejected unauthorized marketplace feedback", "event_id", req.Event.ID.Hex(), "sender", senderPubkey)
			return nil
		case errors.Is(err, ErrFeedbackInvalid), errors.Is(err, ErrFeedbackNotFound):
			metrics.MarketplaceFeedbackMalformed.Inc()
			h.logger.Warn("rejected invalid marketplace feedback", "event_id", req.Event.ID.Hex(), "error", err)
			return nil
		default:
			h.logger.Error("failed to store marketplace feedback", "event_id", req.Event.ID.Hex(), "error", err)
			if h.feedbackLimiter != nil {
				if refundErr := h.feedbackLimiter.Refund(ctx, senderPubkey); refundErr != nil {
					return errors.Join(err, refundErr)
				}
			}
			return err
		}
	}
	if !inserted {
		metrics.MarketplaceFeedbackDuplicate.Inc()
		h.logger.Info("ignored idempotent marketplace feedback duplicate", "event_id", req.Event.ID.Hex(), "review_event_id", params.ReviewEventID)
		return nil
	}

	metrics.MarketplaceFeedbackReceived.Inc()
	metrics.MarketplaceFeedbackAccepted.Inc()
	h.logger.Info("recorded marketplace feedback", "event_id", req.Event.ID.Hex(), "review_event_id", params.ReviewEventID, "rating", params.Rating)
	return nil
}

func parseMarketplaceFeedbackParams(raw json.RawMessage) (MarketplaceFeedbackParams, error) {
	var params MarketplaceFeedbackParams
	if len(raw) == 0 {
		return params, errors.New("feedback params are required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		return params, fmt.Errorf("decode feedback params: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return params, errors.New("feedback params must contain one JSON object")
	}
	if strings.TrimSpace(params.ReviewEventID) == "" {
		return params, errors.New("review_event_id is required")
	}
	if !IsValidRating(params.Rating) {
		return params, errors.New("rating must be between 1 and 5")
	}
	if len(params.Comment) > feedbackCommentMaxBytes {
		return params, errors.New("comment exceeds 4096 bytes")
	}
	return params, nil
}

func validateMarketplaceFeedbackEnvelope(req contextvm.Request, params MarketplaceFeedbackParams) error {
	if req.Sender == nostr.ZeroPK || req.Event.ID == nostr.ZeroID {
		return errors.New("authenticated sender and event id are required")
	}
	methods := feedbackTagValues(req.Event.Tags, "method")
	if len(methods) != 1 || methods[0] != MethodFeedback {
		return errors.New("method tag must match marketplace/feedback")
	}
	related := feedbackTagValues(req.Event.Tags, "e")
	if len(related) != 1 || related[0] != params.ReviewEventID {
		return errors.New("e tag must match review_event_id")
	}
	expirations := feedbackTagValues(req.Event.Tags, "expiration")
	if len(expirations) > 1 {
		return errors.New("at most one expiration tag is allowed")
	}
	if len(expirations) == 0 {
		return nil
	}
	expiresAt, err := strconv.ParseInt(expirations[0], 10, 64)
	if err != nil {
		return errors.New("expiration must be a Unix timestamp")
	}
	now := time.Now().Unix()
	if expiresAt <= now {
		return errors.New("feedback notification expired")
	}
	createdAt := int64(req.Event.CreatedAt)
	if createdAt <= 0 || expiresAt <= createdAt || expiresAt > createdAt+int64(feedbackNotificationMaxLifetime/time.Second) {
		return errors.New("expiration exceeds the 15 minute feedback lifetime")
	}
	return nil
}

func feedbackTagValues(tags nostr.Tags, name string) []string {
	var values []string
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == name {
			values = append(values, tag[1])
		}
	}
	return values
}
