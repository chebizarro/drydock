package marketplace

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"drydock/internal/db"
	"drydock/internal/ratelimit"

	"fiatjaf.com/nostr"
)

func TestHandleFeedback_RateLimiterEnforced(t *testing.T) {
	ctx := context.Background()
	req := feedbackNotificationRequest(t, nostr.Generate(), MarketplaceFeedbackParams{ReviewEventID: "review-event", Rating: 5})
	limiter := ratelimit.New(ratelimit.Config{
		Window:      time.Hour,
		MaxRequests: 1,
		KeyPrefix:   "feedback-handler-test:",
	}, ratelimit.NewMemoryStore())
	if result, err := limiter.Allow(ctx, req.Sender.Hex()); err != nil || !result.Allowed {
		t.Fatalf("pre-consume rate limit: result=%+v err=%v", result, err)
	}

	h := &Handler{
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		feedbackLimiter: limiter,
	}
	if err := h.handleContextVMFeedback(ctx, req); err != nil {
		t.Fatalf("handleFeedback returned error: %v", err)
	}
}

func TestHandleFeedback_TransientStoreFailureRefundsRateLimit(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	requesterSK := nostr.Generate()
	requester := nostr.GetPublicKey(requesterSK).Hex()
	if err := store.CreateAssignment(ctx, db.ReviewAssignment{
		PatchEventID: "patch-rate-retry", RepoID: "repo-1", ReviewerPubkey: testPubKey().Hex(),
		RequesterPubkey: requester, Status: "completed", AssignmentEventID: "assignment-rate-retry",
		CompletionEventID: "review-rate-retry", ReviewEventID: "review-rate-retry",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("CreateAssignment: %v", err)
	}
	assignment, err := store.GetAssignmentByEventID(ctx, "assignment-rate-retry")
	if err != nil {
		t.Fatalf("GetAssignmentByEventID: %v", err)
	}
	limiter := ratelimit.New(ratelimit.Config{Window: time.Hour, MaxRequests: 1, KeyPrefix: "feedback-retry:"}, ratelimit.NewMemoryStore())
	h := NewHandler(NewRegistry(store, slog.Default()), nil, store, slog.New(slog.NewTextHandler(io.Discard, nil))).WithFeedbackLimiter(limiter)
	req := feedbackNotificationRequest(t, requesterSK, MarketplaceFeedbackParams{ReviewEventID: "review-rate-retry", Rating: 5})

	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE reviewer_reputations RENAME TO reviewer_reputations_unavailable`); err != nil {
		t.Fatalf("hide reviewer_reputations: %v", err)
	}
	if err := h.handleContextVMFeedback(ctx, req); err == nil {
		t.Fatal("transient reputation-store failure was not returned")
	}
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE reviewer_reputations_unavailable RENAME TO reviewer_reputations`); err != nil {
		t.Fatalf("restore reviewer_reputations: %v", err)
	}
	if err := h.handleContextVMFeedback(ctx, req); err != nil {
		t.Fatalf("redelivery after transient failure: %v", err)
	}
	if count := feedbackCount(t, ctx, store, assignment.ID); count != 1 {
		t.Fatalf("feedback count after redelivery = %d, want 1", count)
	}
}

func TestHandleFeedback_RateLimiterBackendFailureDenies(t *testing.T) {
	h := &Handler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		feedbackLimiter: ratelimit.New(ratelimit.Config{
			Window:      time.Hour,
			MaxRequests: 1,
			KeyPrefix:   "feedback-handler-failure-test:",
		}, failingFeedbackRateLimitStore{}),
	}

	req := feedbackNotificationRequest(t, nostr.Generate(), MarketplaceFeedbackParams{ReviewEventID: "review-event", Rating: 5})
	if err := h.handleContextVMFeedback(context.Background(), req); err == nil {
		t.Fatal("rate-limit backend failure was not returned for retry")
	}
}

type failingFeedbackRateLimitStore struct{}

func (failingFeedbackRateLimitStore) GetRateLimitCount(context.Context, string, int64) (int, error) {
	return 0, errors.New("backend unavailable")
}

func (failingFeedbackRateLimitStore) IncrementRateLimit(context.Context, string, int64) error {
	return errors.New("backend unavailable")
}

func (failingFeedbackRateLimitStore) CheckAndIncrementRateLimit(context.Context, string, int64, int64, int) (int, bool, error) {
	return 0, false, errors.New("backend unavailable")
}

func (failingFeedbackRateLimitStore) CleanupOldRateLimits(context.Context, int64) (int64, error) {
	return 0, errors.New("backend unavailable")
}
