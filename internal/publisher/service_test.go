package publisher

import (
	"context"
	"fmt"
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

func (f fakeSigner) GetPublicKey(context.Context) (nostr.PubKey, error) {
	return nostr.GetPublicKey(f.sk), nil
}
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

// failingRelayPublisher always returns an error simulating all-relay rejection.
type failingRelayPublisher struct {
	err error
}

func (f *failingRelayPublisher) Publish(_ context.Context, _ []string, _ nostr.Event) error {
	return f.err
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
		if c.event.Kind != nostr.KindComment {
			t.Fatalf("expected kind %d, got %d", nostr.KindComment, c.event.Kind)
		}
		if !strings.Contains(c.event.Content, "context-layers-dropped:") {
			t.Fatalf("missing mandatory context-layers-dropped footer field")
		}
		assertHasTag(t, c.event.Tags, "E")
		assertHasTag(t, c.event.Tags, "K")
		assertHasTag(t, c.event.Tags, "P")
		assertHasTag(t, c.event.Tags, "e")
		assertHasTag(t, c.event.Tags, "k")
		assertHasTag(t, c.event.Tags, "p")
		if strings.Contains(c.event.Content, "##") || strings.Contains(c.event.Content, "**") {
			t.Fatalf("expected plaintext comment content, got markdown-like formatting: %q", c.event.Content)
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

func TestPublishReviewPRUpdateUsesRootAndParentScopes(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	repoOwner := nostr.MustPubKeyFromHex("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	rootPRID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	repoEvt := nostr.Event{ID: nostr.MustIDFromHex("1212121212121212121212121212121212121212121212121212121212121212"), PubKey: repoOwner, Kind: 30617, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", "repo-2"}, {"relays", "wss://relay.repo.example"}}}
	if err := store.UpsertRepositoryAnnouncement(ctx, repoEvt); err != nil {
		t.Fatalf("seed repo announcement: %v", err)
	}

	updateEvt := nostr.Event{
		ID:        nostr.MustIDFromHex("3434343434343434343434343434343434343434343434343434343434343434"),
		PubKey:    nostr.MustPubKeyFromHex("abababababababababababababababababababababababababababababababab"),
		Kind:      1619,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + repoOwner.Hex() + ":repo-2"},
			{"E", rootPRID},
			{"P", repoOwner.Hex()},
			{"c", "1111111111111111111111111111111111111111"},
		},
	}
	if err := store.InsertPatchEvent(ctx, updateEvt); err != nil {
		t.Fatalf("seed pr update: %v", err)
	}
	if err := store.RecordPatchEventRelay(ctx, updateEvt.ID.Hex(), "wss://relay.patch.example"); err != nil {
		t.Fatalf("seed patch relay: %v", err)
	}
	if _, err := store.BeginReview(ctx, updateEvt.ID.Hex(), db.RepoIDFromPatch(updateEvt)); err != nil {
		t.Fatalf("begin review: %v", err)
	}

	fakePub := &fakeRelayPublisher{}
	svc := New(Config{DefaultRelays: []string{"wss://fallback.example"}}, store, fakeSigner{sk: nostr.Generate()}, fakePub, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if _, err := svc.PublishReview(ctx, PublishInput{PatchEventID: updateEvt.ID.Hex(), RepoID: db.RepoIDFromPatch(updateEvt), Summary: "ok", Model: "m", ContextHash: "h", Confidence: 0.7}); err != nil {
		t.Fatalf("publish review: %v", err)
	}
	if len(fakePub.calls) == 0 {
		t.Fatalf("expected at least one publish call")
	}
	tags := fakePub.calls[0].event.Tags
	if got := findTagValue(tags, "E"); got != rootPRID {
		t.Fatalf("expected E=%s got %s", rootPRID, got)
	}
	if got := findTagValue(tags, "K"); got != "1618" {
		t.Fatalf("expected K=1618 got %s", got)
	}
	if got := findTagValue(tags, "e"); got != updateEvt.ID.Hex() {
		t.Fatalf("expected e=%s got %s", updateEvt.ID.Hex(), got)
	}
	if got := findTagValue(tags, "k"); got != "1619" {
		t.Fatalf("expected k=1619 got %s", got)
	}
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

func assertHasTag(t *testing.T, tags nostr.Tags, name string) {
	t.Helper()
	for _, tag := range tags {
		if len(tag) > 0 && tag[0] == name {
			return
		}
	}
	t.Fatalf("missing required tag %s in %v", name, tags)
}

func findTagValue(tags nostr.Tags, name string) string {
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == name {
			return tag[1]
		}
	}
	return ""
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func TestPublishReviewFailsGracefullyWhenAllRelaysReject(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	if _, err := store.BeginReview(ctx, patchID, repoID); err != nil {
		t.Fatalf("begin review: %v", err)
	}

	rejectPub := &failingRelayPublisher{err: fmt.Errorf("publish failed on all relays: wss://relay.test: msg: blocked: spam")}
	svc := New(Config{
		DefaultRelays:       []string{"wss://fallback.example"},
		DetailSeverityFloor: "high",
	}, store, fakeSigner{sk: nostr.Generate()}, rejectPub, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	_, err := svc.PublishReview(ctx, PublishInput{
		PatchEventID:      patchID,
		RepoID:            repoID,
		Summary:           "Review summary.",
		Model:             "test-model",
		ContextHash:       "hash123",
		Confidence:        0.5,
		ContextLayersUsed: []string{"patch"},
	})
	if err == nil {
		t.Fatal("expected error when all relays reject, got nil")
	}
	if !strings.Contains(err.Error(), "publish summary review event") {
		t.Fatalf("expected publish error, got: %v", err)
	}
}

func TestPublishReviewPartialRelayFailureStillSucceeds(t *testing.T) {
	// This test verifies that when the publisher succeeds (returns nil),
	// the service treats it as success. The real NostrRelayPublisher handles
	// partial failure internally (success > 0 means ok).
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
		Summary:           "All good.",
		Model:             "test-model",
		ContextHash:       "hash456",
		Confidence:        0.9,
		ContextLayersUsed: []string{"patch"},
	})
	if err != nil {
		t.Fatalf("publish should succeed: %v", err)
	}
	if strings.TrimSpace(eventID) == "" {
		t.Fatal("expected non-empty event id")
	}
	if len(fakePub.calls) == 0 {
		t.Fatal("expected at least one publish call")
	}
}
