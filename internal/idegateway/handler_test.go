package idegateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

type mockSigner struct{}

func (m mockSigner) GetPublicKey(context.Context) (nostr.PubKey, error) {
	return nostr.PubKey{}, nil
}

func (m mockSigner) SignEvent(context.Context, *nostr.Event) error {
	return nil
}

type mockPublisher struct {
	events []nostr.Event
}

func (m *mockPublisher) Publish(_ context.Context, _ []string, event nostr.Event) error {
	m.events = append(m.events, event)
	return nil
}

func newTestHandler(pub *mockPublisher) *Handler {
	return &Handler{
		cfg:      Config{},
		signer:   mockSigner{},
		publish:  pub,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		sem:      make(chan struct{}, maxConcurrent),
		sessions: make(map[string]*activeSession),
		fixTTL:   time.Minute,
	}
}

func TestHandleFixRequestReturnsStoredFix(t *testing.T) {
	pub := &mockPublisher{}
	h := newTestHandler(pub)

	h.storeFix("fix-1", storedFix{
		SessionID: "sess-1",
		File:      "main.go",
		Diff:      "@@ -1 +1 @@\n-old\n+new",
		CreatedAt: time.Now(),
	})

	event := nostr.Event{
		Content: `{"session_id":"sess-1","request_id":"req-1","fix_id":"fix-1","file":"main.go"}`,
	}

	if err := h.handleFixRequest(context.Background(), event, ""); err != nil {
		t.Fatalf("handleFixRequest failed: %v", err)
	}

	if len(pub.events) != 1 {
		t.Fatalf("published events = %d, want 1", len(pub.events))
	}

	var resp FixResponse
	if err := json.Unmarshal([]byte(pub.events[0].Content), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if !resp.Success {
		t.Fatalf("Success = false, want true (error: %q)", resp.Error)
	}
	if resp.Diff == "" {
		t.Fatal("Diff is empty, want stored diff")
	}
}

func TestHandleFixRequestMissingFix(t *testing.T) {
	pub := &mockPublisher{}
	h := newTestHandler(pub)

	event := nostr.Event{
		Content: `{"session_id":"sess-1","request_id":"req-1","fix_id":"missing","file":"main.go"}`,
	}

	if err := h.handleFixRequest(context.Background(), event, ""); err != nil {
		t.Fatalf("handleFixRequest failed: %v", err)
	}

	if len(pub.events) != 1 {
		t.Fatalf("published events = %d, want 1", len(pub.events))
	}

	var resp FixResponse
	if err := json.Unmarshal([]byte(pub.events[0].Content), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Success {
		t.Fatal("Success = true, want false")
	}
	if resp.Error == "" {
		t.Fatal("Error is empty, want descriptive failure")
	}
}

func TestCleanupExpiredFixes(t *testing.T) {
	pub := &mockPublisher{}
	h := newTestHandler(pub)
	h.fixTTL = time.Second

	h.storeFix("expired", storedFix{
		SessionID: "sess-1",
		File:      "main.go",
		Diff:      "diff",
		CreatedAt: time.Now().Add(-2 * time.Second),
	})

	h.cleanupExpiredFixes(time.Now())

	if _, ok := h.lookupFix("expired", time.Now()); ok {
		t.Fatal("expired fix should have been removed")
	}
}

