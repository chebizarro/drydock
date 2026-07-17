package marketplace

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"drydock/internal/ratelimit"

	"fiatjaf.com/nostr"
)

func TestHandleFeedback_RateLimiterEnforced(t *testing.T) {
	ctx := context.Background()
	event := rateLimitTestFeedbackEvent()
	limiter := ratelimit.New(ratelimit.Config{
		Window:      time.Hour,
		MaxRequests: 1,
		KeyPrefix:   "feedback-handler-test:",
	}, ratelimit.NewMemoryStore())
	if result, err := limiter.Allow(ctx, event.PubKey.Hex()); err != nil || !result.Allowed {
		t.Fatalf("pre-consume rate limit: result=%+v err=%v", result, err)
	}

	h := &Handler{
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		feedbackLimiter: limiter,
	}
	if err := h.handleFeedback(ctx, event); err != nil {
		t.Fatalf("handleFeedback returned error: %v", err)
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

	if err := h.handleFeedback(context.Background(), rateLimitTestFeedbackEvent()); err != nil {
		t.Fatalf("handleFeedback returned error: %v", err)
	}
}

func rateLimitTestFeedbackEvent() nostr.Event {
	return nostr.Event{
		Kind:      KindReviewFeedback,
		CreatedAt: nostr.Now(),
		Tags:      ReviewFeedbackTags("review-event", nostr.PubKey{}.Hex(), 5),
		Content:   `{}`,
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
