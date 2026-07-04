package idegateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"drydock/internal/db"

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
	return newTestHandlerWithStore(pub, nil)
}

func newTestHandlerWithStore(pub *mockPublisher, store *db.Store) *Handler {
	return &Handler{
		cfg:       Config{},
		store:     store,
		signer:    mockSigner{},
		publish:   pub,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		ourPubKey: zeroPubKeyHex(),
		sem:       make(chan struct{}, maxConcurrent),
		sessions:  make(map[string]*activeSession),
		fixTTL:    time.Minute,
	}
}

func zeroPubKeyHex() string {
	var pk nostr.PubKey
	return pk.Hex()
}

func fixRequestEvent(t *testing.T, h *Handler, sessionID, requestID, fixID, file string) nostr.Event {
	t.Helper()
	content, err := json.Marshal(FixRequest{
		SessionID: sessionID,
		RequestID: requestID,
		FixID:     fixID,
		File:      file,
	})
	if err != nil {
		t.Fatalf("marshal fix request: %v", err)
	}
	return nostr.Event{
		Content: string(content),
		Tags: nostr.Tags{
			{"p", h.ourPubKey},
			{"session", sessionID},
			{"request", requestID},
		},
	}
}

func TestHandleFixRequestReturnsStoredFix(t *testing.T) {
	ctx := context.Background()
	pub := &mockPublisher{}
	h := newTestHandler(pub)

	h.storeFix(ctx, "fix-1", storedFix{
		SessionID: "sess-1",
		File:      "main.go",
		Diff:      "@@ -1 +1 @@\n-old\n+new",
		CreatedAt: time.Now(),
	})

	event := fixRequestEvent(t, h, "sess-1", "req-1", "fix-1", "main.go")

	if err := h.handleFixRequest(ctx, event, ""); err != nil {
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

func TestHandleFixRequestRejectsUnaddressedRecipient(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		tags nostr.Tags
	}{
		{
			name: "missing p tag",
			tags: nostr.Tags{{"session", "sess-1"}, {"request", "req-1"}},
		},
		{
			name: "wrong p tag",
			tags: nostr.Tags{{"p", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}, {"session", "sess-1"}, {"request", "req-1"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &mockPublisher{}
			h := newTestHandler(pub)
			h.storeFix(ctx, "fix-1", storedFix{
				SessionID: "sess-1",
				File:      "main.go",
				Diff:      "@@ -1 +1 @@\n-old\n+new",
				CreatedAt: time.Now(),
			})

			event := fixRequestEvent(t, h, "sess-1", "req-1", "fix-1", "main.go")
			event.Tags = tc.tags

			if err := h.handleFixRequest(ctx, event, ""); err != nil {
				t.Fatalf("handleFixRequest failed: %v", err)
			}
			if len(pub.events) != 0 {
				t.Fatalf("published events = %d, want 0", len(pub.events))
			}
		})
	}
}

func TestHandleFixRequestRejectsUnauthorizedSender(t *testing.T) {
	ctx := context.Background()
	pub := &mockPublisher{}
	h := newTestHandler(pub)

	ownerSK := nostr.Generate()
	otherSK := nostr.Generate()
	h.storeFix(ctx, "fix-1", storedFix{
		SessionID:    "sess-1",
		AuthorPubKey: nostr.GetPublicKey(ownerSK).Hex(),
		File:         "main.go",
		Diff:         "@@ -1 +1 @@\n-old\n+new",
		CreatedAt:    time.Now(),
	})

	event := fixRequestEvent(t, h, "sess-1", "req-1", "fix-1", "main.go")
	event.PubKey = nostr.GetPublicKey(otherSK)

	if err := h.handleFixRequest(ctx, event, ""); err != nil {
		t.Fatalf("handleFixRequest failed: %v", err)
	}
	if len(pub.events) != 0 {
		t.Fatalf("published events = %d, want 0", len(pub.events))
	}
}

func TestHandleFixRequestMissingFix(t *testing.T) {
	ctx := context.Background()
	pub := &mockPublisher{}
	h := newTestHandler(pub)

	event := fixRequestEvent(t, h, "sess-1", "req-1", "missing", "main.go")

	if err := h.handleFixRequest(ctx, event, ""); err != nil {
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

func TestStoredFixSurvivesNewHandlerInstance(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ide-fixes.db")

	store1, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store1: %v", err)
	}
	if err := store1.Migrate(ctx); err != nil {
		t.Fatalf("migrate store1: %v", err)
	}

	ownerSK := nostr.Generate()
	ownerPubKey := nostr.GetPublicKey(ownerSK)
	patch := "@@ -1 +1 @@\n-old\n+real durable patch"
	h1 := newTestHandlerWithStore(&mockPublisher{}, store1)
	h1.storeFix(ctx, "fix-durable", storedFix{
		SessionID:    "sess-1",
		AuthorPubKey: ownerPubKey.Hex(),
		File:         "main.go",
		Diff:         patch,
		CreatedAt:    time.Now(),
	})
	if err := store1.Close(); err != nil {
		t.Fatalf("close store1: %v", err)
	}

	store2, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store2: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	if err := store2.Migrate(ctx); err != nil {
		t.Fatalf("migrate store2: %v", err)
	}

	pub := &mockPublisher{}
	h2 := newTestHandlerWithStore(pub, store2)
	event := fixRequestEvent(t, h2, "sess-1", "req-1", "fix-durable", "main.go")
	event.PubKey = ownerPubKey

	if err := h2.handleFixRequest(ctx, event, ""); err != nil {
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
	if resp.Diff != patch {
		t.Fatalf("Diff = %q, want %q", resp.Diff, patch)
	}
}

func TestCleanupExpiredFixes(t *testing.T) {
	ctx := context.Background()
	pub := &mockPublisher{}
	h := newTestHandler(pub)
	h.fixTTL = time.Second

	h.storeFix(ctx, "expired", storedFix{
		SessionID: "sess-1",
		File:      "main.go",
		Diff:      "diff",
		CreatedAt: time.Now().Add(-2 * time.Second),
	})

	h.cleanupExpiredFixes(ctx, time.Now())

	if _, ok := h.lookupFix(ctx, "expired", "sess-1", time.Now()); ok {
		t.Fatal("expired fix should have been removed")
	}
}
