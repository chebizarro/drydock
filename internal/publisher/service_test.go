package publisher

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/db"
	"drydock/internal/reviewengine"

	"fiatjaf.com/nostr"
)

type fakeSigner struct {
	sk nostr.SecretKey
}

func (f fakeSigner) GetPublicKey(context.Context) (nostr.PubKey, error) { return nostr.GetPublicKey(f.sk), nil }
func (f fakeSigner) SignEvent(_ context.Context, evt *nostr.Event) error {
	return evt.Sign(f.sk)
}

type fakeRelayPublisher struct {
	calls []publishCall
}

type publishCall struct {
	relays []string
	event  nostr.Event
}

func (f *fakeRelayPublisher) Publish(_ context.Context, relays []string, event nostr.Event) error {
	copied := append([]string(nil), relays...)
	f.calls = append(f.calls, publishCall{relays: copied, event: event})
	return nil
}

func TestPublishReviewSummaryAndHighDetail(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	if _, err := store.BeginReview(ctx, patchID, repoID); err != nil {
		t.Fatalf("begin review: %v", err)
	}

	fakePub := &fakeRelayPublisher{}
	svc := New(Config{
		DefaultRelays:       []string{"wss://fallback.example"},
		DetailSeverityFloor: "high",
	}, store, fakeSigner{sk: nostr.Generate()}, fakePub, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	eventID, err := svc.PublishReview(ctx, PublishInput{
		PatchEventID:      patchID,
		RepoID:            repoID,
		Summary:           "Looks good overall.",
		Model:             "qwen2.5-coder-32b-instruct-q4_k_m",
		ContextHash:       "abc123",
		Confidence:        0.82,
		ContextLayersUsed: []string{"patch", "modified-files"},
		Findings: []reviewengine.Finding{
			{Severity: "high", Category: "correctness", File: "main.go", Line: 12, Evidence: "x", Explanation: "bug", Suggestion: "fix", Confidence: 0.9},
			{Severity: "low", Category: "style", File: "main.go", Line: 18, Evidence: "y", Explanation: "style", Suggestion: "optional", Confidence: 0.99},
		},
	})
	if err != nil {
		t.Fatalf("publish review: %v", err)
	}
	if strings.TrimSpace(eventID) == "" {
		t.Fatalf("expected non-empty summary event id")
	}

	if len(fakePub.calls) != 2 {
		t.Fatalf("expected 2 publish calls (summary + high detail), got %d", len(fakePub.calls))
	}
	for _, c := range fakePub.calls {
		if c.event.Kind != 1622 {
			t.Fatalf("expected kind 1622, got %d", c.event.Kind)
		}
		if !strings.Contains(c.event.Content, "context-layers-dropped:") {
			t.Fatalf("missing mandatory context-layers-dropped footer field")
		}
	}
	if !contains(fakePub.calls[0].relays, "wss://relay.patch.example") || !contains(fakePub.calls[0].relays, "wss://relay.repo.example") {
		t.Fatalf("expected relay union from patch+repo, got %#v", fakePub.calls[0].relays)
	}
}

func seedRepoAndPatch(t *testing.T, ctx context.Context, store *db.Store) (patchID string, repoID string) {
	t.Helper()
	repoOwner := nostr.MustPubKeyFromHex("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	repoEvt := nostr.Event{
		ID:        nostr.MustIDFromHex("1111111111111111111111111111111111111111111111111111111111111111"),
		PubKey:    repoOwner,
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "repo-1"},
			{"clone", "https://example.com/repo-1.git"},
			{"relays", "wss://relay.repo.example"},
		},
	}
	if err := store.UpsertRepositoryAnnouncement(ctx, repoEvt); err != nil {
		t.Fatalf("seed repo announcement: %v", err)
	}

	patchEvt := nostr.Event{
		ID:        nostr.MustIDFromHex("2222222222222222222222222222222222222222222222222222222222222222"),
		PubKey:    nostr.MustPubKeyFromHex("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + repoOwner.Hex() + ":repo-1"},
			{"e", "3333333333333333333333333333333333333333333333333333333333333333", "", "root"},
		},
		Content: "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -0,0 +1 @@\n+package main\n",
	}
	if err := store.InsertPatchEvent(ctx, patchEvt); err != nil {
		t.Fatalf("seed patch event: %v", err)
	}
	if err := store.RecordPatchEventRelay(ctx, patchEvt.ID.Hex(), "wss://relay.patch.example"); err != nil {
		t.Fatalf("seed patch relay: %v", err)
	}
	return patchEvt.ID.Hex(), db.RepoIDFromPatch(patchEvt)
}

func mustStore(t *testing.T, ctx context.Context) *db.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "publisher-test.db")
	store, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

