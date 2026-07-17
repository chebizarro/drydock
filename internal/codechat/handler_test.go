package codechat

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

func TestParseMessage_RepoPrefix(t *testing.T) {
	h := &Handler{}

	cases := []struct {
		input    string
		wantRepo string
		wantQ    string
	}{
		{
			input:    "repo:npub1abc/myrepo what does foo do?",
			wantRepo: "npub1abc/myrepo",
			wantQ:    "what does foo do?",
		},
		{
			input:    "@npub1xyz/project how does auth work?",
			wantRepo: "npub1xyz/project",
			wantQ:    "how does auth work?",
		},
		{
			input:    "what does the main function do?",
			wantRepo: "",
			wantQ:    "what does the main function do?",
		},
		{
			input:    "repo:myrepo",
			wantRepo: "myrepo",
			wantQ:    "",
		},
		{
			input:    "",
			wantRepo: "",
			wantQ:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			result := h.parseMessage(tc.input)
			if result.repoID != tc.wantRepo {
				t.Errorf("repoID: got %q, want %q", result.repoID, tc.wantRepo)
			}
			if result.question != tc.wantQ {
				t.Errorf("question: got %q, want %q", result.question, tc.wantQ)
			}
		})
	}
}

func TestHandleDM_RateLimiterEnforced(t *testing.T) {
	ctx := context.Background()
	event := nostr.Event{}
	limiter := ratelimit.New(ratelimit.Config{
		Window:      time.Hour,
		MaxRequests: 1,
		KeyPrefix:   "handler-test:",
	}, ratelimit.NewMemoryStore())
	if result, err := limiter.Allow(ctx, event.PubKey.Hex()); err != nil || !result.Allowed {
		t.Fatalf("pre-consume rate limit: result=%+v err=%v", result, err)
	}

	keyer := &trackingKeyer{}
	h := &Handler{
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		sem:         make(chan struct{}, 1),
		keyer:       keyer,
		rateLimiter: limiter,
	}
	if err := h.HandleDM(ctx, event, ""); err != nil {
		t.Fatalf("HandleDM returned error: %v", err)
	}
	if keyer.decryptCalls != 0 {
		t.Fatalf("rate-limited request reached decryption path %d times", keyer.decryptCalls)
	}
}

func TestHandleDM_RateLimiterBackendFailureDenies(t *testing.T) {
	keyer := &trackingKeyer{}
	h := &Handler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sem:    make(chan struct{}, 1),
		keyer:  keyer,
		rateLimiter: ratelimit.New(ratelimit.Config{
			Window:      time.Hour,
			MaxRequests: 1,
			KeyPrefix:   "handler-failure-test:",
		}, failingRateLimitStore{}),
	}

	if err := h.HandleDM(context.Background(), nostr.Event{}, ""); err != nil {
		t.Fatalf("HandleDM returned error: %v", err)
	}
	if keyer.decryptCalls != 0 {
		t.Fatalf("request bypassed failed limiter and reached decryption path %d times", keyer.decryptCalls)
	}
}

type trackingKeyer struct {
	decryptCalls int
}

func (k *trackingKeyer) GetPublicKey(context.Context) (nostr.PubKey, error) {
	return nostr.PubKey{}, nil
}

func (k *trackingKeyer) SignEvent(context.Context, *nostr.Event) error { return nil }

func (k *trackingKeyer) Encrypt(context.Context, string, nostr.PubKey) (string, error) {
	return "", nil
}

func (k *trackingKeyer) Decrypt(context.Context, string, nostr.PubKey) (string, error) {
	k.decryptCalls++
	return "", errors.New("unexpected decrypt")
}

type failingRateLimitStore struct{}

func (failingRateLimitStore) GetRateLimitCount(context.Context, string, int64) (int, error) {
	return 0, errors.New("backend unavailable")
}

func (failingRateLimitStore) IncrementRateLimit(context.Context, string, int64) error {
	return errors.New("backend unavailable")
}

func (failingRateLimitStore) CheckAndIncrementRateLimit(context.Context, string, int64, int64, int) (int, bool, error) {
	return 0, false, errors.New("backend unavailable")
}

func (failingRateLimitStore) CleanupOldRateLimits(context.Context, int64) (int64, error) {
	return 0, errors.New("backend unavailable")
}

func TestPayloadInt(t *testing.T) {
	cases := []struct {
		payload map[string]any
		key     string
		want    int
	}{
		{map[string]any{"line": float64(42)}, "line", 42},
		{map[string]any{"line": 42}, "line", 42},
		{map[string]any{"line": int64(42)}, "line", 42},
		{map[string]any{}, "line", 0},
		{map[string]any{"line": "not a number"}, "line", 0},
	}

	for _, tc := range cases {
		got := payloadInt(tc.payload, tc.key)
		if got != tc.want {
			t.Errorf("payloadInt(%v, %q) = %d, want %d", tc.payload, tc.key, got, tc.want)
		}
	}
}
