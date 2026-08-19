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
	"drydock/internal/targetidentity"

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
	envelope := targetidentity.New(repoID, patchID, patchID, "sha256:remote", strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 64), "ignored", "bundle")
	contextHash, err := envelope.Hash()
	if err != nil {
		t.Fatalf("hash target envelope: %v", err)
	}

	eventID, err := svc.PublishReview(ctx, PublishInput{
		PatchEventID:      patchID,
		RepoID:            repoID,
		Summary:           "Looks good overall.",
		Model:             "qwen2.5-coder-32b-instruct-q4_k_m",
		ContextHash:       contextHash,
		TargetEnvelope:    envelope,
		Confidence:        0.82,
		ContextLayersUsed: []string{"patch", "modified-files"},
		BaseCommit:        strings.Repeat("a", 40),
		TipCommit:         strings.Repeat("b", 40),
		DiffSHA256:        strings.Repeat("c", 64),
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
		for _, field := range []string{"repo-id: " + repoID, "root-id: " + patchID, "patch-event-id: " + patchID, "canonical-remote-identity: sha256:remote", "base-commit: " + strings.Repeat("a", 40), "tip-commit: " + strings.Repeat("b", 40), "diff-sha256: " + strings.Repeat("c", 64), "bundle-sha256: " + envelope.BundleSHA256} {
			if !strings.Contains(c.event.Content, field) {
				t.Fatalf("missing target envelope footer field %q in %s", field, c.event.Content)
			}
		}
		assertHasTag(t, c.event.Tags, "E")
		assertHasTag(t, c.event.Tags, "K")
		assertHasTag(t, c.event.Tags, "P")
		assertHasTag(t, c.event.Tags, "e")
		assertHasTag(t, c.event.Tags, "k")
		assertHasTag(t, c.event.Tags, "p")
		for key, want := range map[string]string{
			"base_commit": strings.Repeat("a", 40),
			"tip_commit":  strings.Repeat("b", 40),
			"diff_sha256": strings.Repeat("c", 64),
		} {
			tag := c.event.Tags.Find(key)
			if tag == nil || len(tag) < 2 || tag[1] != want {
				t.Fatalf("%s tag = %v, want %s", key, tag, want)
			}
		}
		if strings.Contains(c.event.Content, "##") || strings.Contains(c.event.Content, "**") {
			t.Fatalf("expected plaintext comment content, got markdown-like formatting: %q", c.event.Content)
		}
	}
	if !contains(fakePub.calls[0].relays, "wss://relay.patch.example") || !contains(fakePub.calls[0].relays, "wss://relay.repo.example") {
		t.Fatalf("expected relay union from patch+repo, got %#v", fakePub.calls[0].relays)
	}
}

func TestPublishReviewRedactsSensitiveFindingText(t *testing.T) {
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

	rawText := []string{
		"raw-evidence-token",
		"raw-explanation-token",
		"raw-suggestion-token",
		"raw-diff-old-token",
		"raw-diff-new-token",
		"raw-code-token",
	}
	_, err := svc.PublishReview(ctx, PublishInput{
		PatchEventID: patchID,
		RepoID:       repoID,
		Summary:      "A sensitive finding was detected.",
		Model:        "test-model",
		ContextHash:  "sensitive-test",
		Findings: []reviewengine.Finding{{
			Severity:      "high",
			Category:      "security",
			File:          "config.go",
			Line:          7,
			Evidence:      rawText[0],
			Explanation:   rawText[1],
			Suggestion:    rawText[2],
			SuggestedDiff: "@@ -1 +1 @@\n-" + rawText[3] + "\n+" + rawText[4],
			SuggestedCode: rawText[5],
			Sensitive:     true,
			Confidence:    0.99,
		}},
	})
	if err != nil {
		t.Fatalf("PublishReview() error = %v", err)
	}
	if len(fakePub.calls) != 2 {
		t.Fatalf("published events = %d, want summary and detail", len(fakePub.calls))
	}
	for _, call := range fakePub.calls {
		for _, raw := range rawText {
			if strings.Contains(call.event.Content, raw) {
				t.Fatalf("published event leaked %q: %s", raw, call.event.Content)
			}
		}
		if !strings.Contains(call.event.Content, sensitiveFindingSafeText) {
			t.Fatalf("published event missing fixed redaction text: %s", call.event.Content)
		}
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

func TestPublishReviewExcludedFilesInFooter(t *testing.T) {
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

	_, err := svc.PublishReview(ctx, PublishInput{
		PatchEventID:      patchID,
		RepoID:            repoID,
		Summary:           "Review with excluded files.",
		Model:             "test-model",
		ContextHash:       "hash789",
		Confidence:        0.85,
		ContextLayersUsed: []string{"patch"},
		ExcludedFiles:     []string{"package-lock.json", "schema.proto"},
	})
	if err != nil {
		t.Fatalf("publish should succeed: %v", err)
	}
	if len(fakePub.calls) == 0 {
		t.Fatal("expected at least one publish call")
	}
	for _, c := range fakePub.calls {
		if !strings.Contains(c.event.Content, "excluded-files: package-lock.json, schema.proto") {
			t.Fatalf("expected excluded-files in footer, got: %s", c.event.Content)
		}
	}
}

// failNthRelayPublisher fails on specific 0-based call indices.
type failNthRelayPublisher struct {
	failOnIndices map[int]bool
	callIndex     int
	calls         []publishCall
}

func (f *failNthRelayPublisher) Publish(_ context.Context, relays []string, event nostr.Event) error {
	idx := f.callIndex
	f.callIndex++
	f.calls = append(f.calls, publishCall{relays: append([]string(nil), relays...), event: event})
	if f.failOnIndices[idx] {
		return fmt.Errorf("simulated relay failure on call %d", idx)
	}
	return nil
}

func TestPublishReviewDetailFailureLeavesReviewRetryable(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	if _, err := store.BeginReview(ctx, patchID, repoID); err != nil {
		t.Fatalf("begin review: %v", err)
	}

	// Fail on calls 1 and 2 (the detail findings); call 0 (summary) succeeds.
	fakePub := &failNthRelayPublisher{failOnIndices: map[int]bool{1: true, 2: true}}
	svc := New(Config{
		DefaultRelays:       []string{"wss://fallback.example"},
		DetailSeverityFloor: "high",
	}, store, fakeSigner{sk: nostr.Generate()}, fakePub, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	eventID, err := svc.PublishReview(ctx, PublishInput{
		PatchEventID:      patchID,
		RepoID:            repoID,
		Summary:           "Found issues.",
		Model:             "test-model",
		ContextHash:       "hash-detail-fail",
		Confidence:        0.8,
		ContextLayersUsed: []string{"patch"},
		Findings: []reviewengine.Finding{
			{Severity: "critical", Category: "security", File: "auth.go", Line: 5, Evidence: "x", Explanation: "bad", Suggestion: "fix"},
			{Severity: "high", Category: "correctness", File: "main.go", Line: 10, Evidence: "y", Explanation: "wrong", Suggestion: "change"},
		},
	})
	if err == nil {
		t.Fatal("expected detail delivery error")
	}
	if strings.TrimSpace(eventID) == "" {
		t.Fatal("expected the successfully delivered summary event id")
	}
	status, statusErr := store.GetReviewStatus(ctx, patchID, repoID)
	if statusErr != nil {
		t.Fatalf("get review status: %v", statusErr)
	}
	if status == "published" {
		t.Fatal("review was marked published despite failed details")
	}

	// Summary was published (call 0), detail attempts were made (calls 1,2) but failed.
	if len(fakePub.calls) != 3 {
		t.Fatalf("expected 3 publish calls (1 summary + 2 detail attempts), got %d", len(fakePub.calls))
	}
}

func TestPublishReviewReservationFailureBlocksRelayPublish(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	if _, err := store.BeginReview(ctx, patchID, repoID); err != nil {
		t.Fatalf("begin review: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `CREATE TRIGGER fail_review_event_reservation
		BEFORE UPDATE OF review_event_id ON review_log
		BEGIN SELECT RAISE(FAIL, 'simulated reservation failure'); END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	fakePub := &fakeRelayPublisher{}
	svc := New(Config{DefaultRelays: []string{"wss://fallback.example"}}, store, fakeSigner{sk: nostr.Generate()}, fakePub, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	_, err := svc.PublishReview(ctx, PublishInput{PatchEventID: patchID, RepoID: repoID, Summary: "summary", Model: "m", ContextHash: "h"})
	if err == nil || !strings.Contains(err.Error(), "durably reserve summary review event id") {
		t.Fatalf("expected durable reservation error, got %v", err)
	}
	if len(fakePub.calls) != 0 {
		t.Fatalf("relay publish occurred without durable reservation: %d calls", len(fakePub.calls))
	}
}

func TestPublishReviewRetrySkipsDeliveredEvents(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	if _, err := store.BeginReview(ctx, patchID, repoID); err != nil {
		t.Fatalf("begin review: %v", err)
	}

	fakePub := &failNthRelayPublisher{failOnIndices: map[int]bool{2: true}}
	svc := New(Config{DefaultRelays: []string{"wss://fallback.example"}, DetailSeverityFloor: "high"}, store, fakeSigner{sk: nostr.Generate()}, fakePub, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	input := PublishInput{
		PatchEventID: patchID, RepoID: repoID, Summary: "Found issues.", Model: "m", ContextHash: "h",
		Findings: []reviewengine.Finding{
			{Severity: "critical", File: "a.go", Line: 1, Explanation: "first"},
			{Severity: "high", File: "b.go", Line: 2, Explanation: "second"},
		},
	}
	if _, err := svc.PublishReview(ctx, input); err == nil {
		t.Fatal("expected first attempt to fail on the second detail")
	}
	if len(fakePub.calls) != 3 {
		t.Fatalf("first attempt calls = %d, want 3", len(fakePub.calls))
	}
	summaryID := fakePub.calls[0].event.ID.Hex()
	firstDetailID := fakePub.calls[1].event.ID.Hex()

	if _, err := svc.PublishReview(ctx, input); err != nil {
		t.Fatalf("retry publish: %v", err)
	}
	if len(fakePub.calls) != 4 {
		t.Fatalf("retry should publish only the failed detail; total calls = %d, want 4", len(fakePub.calls))
	}
	if fakePub.calls[3].event.ID.Hex() == summaryID || fakePub.calls[3].event.ID.Hex() == firstDetailID {
		t.Fatal("retry duplicated an already-delivered event")
	}
	status, err := store.GetReviewStatus(ctx, patchID, repoID)
	if err != nil {
		t.Fatalf("get review status: %v", err)
	}
	if status != "published" {
		t.Fatalf("review status = %q, want published", status)
	}
}

func TestPublishReviewWithStructuredSuggestions(t *testing.T) {
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
		Summary:           "Found a bug.",
		Model:             "test-model",
		ContextHash:       "hash-suggest",
		Confidence:        0.9,
		ContextLayersUsed: []string{"patch"},
		Findings: []reviewengine.Finding{
			{
				Severity:      "high",
				Category:      "correctness",
				File:          "main.go",
				Line:          42,
				Evidence:      "err ignored",
				Explanation:   "error not checked",
				Suggestion:    "check error",
				SuggestedDiff: "@@ -42,1 +42,3 @@\n-\tValidate(token)\n+\tif err := Validate(token); err != nil {\n+\t\treturn err\n+\t}",
				Confidence:    0.95,
			},
		},
	})
	if err != nil {
		t.Fatalf("publish review: %v", err)
	}
	if strings.TrimSpace(eventID) == "" {
		t.Fatalf("expected non-empty event id")
	}

	// Should have 2 publish calls: summary + high-severity detail.
	if len(fakePub.calls) != 2 {
		t.Fatalf("expected 2 publish calls, got %d", len(fakePub.calls))
	}

	// Summary should contain "fix available" marker.
	summaryContent := fakePub.calls[0].event.Content
	if !strings.Contains(summaryContent, "fix available") {
		t.Fatalf("expected 'fix available' in summary, got: %s", summaryContent)
	}

	// Detail should contain fenced diff block.
	detailContent := fakePub.calls[1].event.Content
	if !strings.Contains(detailContent, "```diff") {
		t.Fatalf("expected fenced diff block in detail, got: %s", detailContent)
	}
	if !strings.Contains(detailContent, "Validate(token)") {
		t.Fatalf("expected diff content in detail, got: %s", detailContent)
	}
}

func TestPublishReviewWithWalkthrough(t *testing.T) {
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

	_, err := svc.PublishReview(ctx, PublishInput{
		PatchEventID:      patchID,
		RepoID:            repoID,
		Summary:           "Looks good.",
		Model:             "test-model",
		ContextHash:       "hash-wt",
		Confidence:        0.9,
		ContextLayersUsed: []string{"patch"},
		Walkthrough: reviewengine.WalkthroughOutput{
			Walkthrough: "This PR adds retry logic to the HTTP client.",
			FileSummaries: []reviewengine.FileSummary{
				{File: "client.go", Summary: "Added exponential backoff"},
				{File: "config.go", Summary: "Added retry settings"},
			},
		},
	})
	if err != nil {
		t.Fatalf("publish review: %v", err)
	}

	if len(fakePub.calls) == 0 {
		t.Fatal("expected at least one publish call")
	}
	summaryContent := fakePub.calls[0].event.Content
	// Should contain walkthrough section before the review summary.
	if !strings.Contains(summaryContent, "Walkthrough") {
		t.Fatalf("expected 'Walkthrough' section in summary, got: %s", summaryContent)
	}
	if !strings.Contains(summaryContent, "retry logic") {
		t.Fatalf("expected walkthrough text in summary, got: %s", summaryContent)
	}
	if !strings.Contains(summaryContent, "client.go: Added exponential backoff") {
		t.Fatalf("expected file summary in summary, got: %s", summaryContent)
	}
	// Walkthrough should appear BEFORE the review summary.
	wtIdx := strings.Index(summaryContent, "Walkthrough")
	summaryIdx := strings.Index(summaryContent, "Automated review summary")
	if wtIdx > summaryIdx {
		t.Fatalf("walkthrough should appear before review summary, wt=%d summary=%d", wtIdx, summaryIdx)
	}
}

func TestPublishReviewPerRequestDetailFloor(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	if _, err := store.BeginReview(ctx, patchID, repoID); err != nil {
		t.Fatalf("begin review: %v", err)
	}

	fakePub := &fakeRelayPublisher{}
	svc := New(Config{
		DefaultRelays:       []string{"wss://fallback.example"},
		DetailSeverityFloor: "high", // Service default
	}, store, fakeSigner{sk: nostr.Generate()}, fakePub, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	// Override to "medium" — the medium finding should now get a detail event.
	_, err := svc.PublishReview(ctx, PublishInput{
		PatchEventID:        patchID,
		RepoID:              repoID,
		Summary:             "Review with overridden floor.",
		Model:               "test-model",
		ContextHash:         "hash-floor",
		Confidence:          0.85,
		ContextLayersUsed:   []string{"patch"},
		DetailSeverityFloor: "medium",
		Findings: []reviewengine.Finding{
			{Severity: "medium", Category: "correctness", File: "main.go", Line: 10, Evidence: "x", Explanation: "medium issue", Suggestion: "fix", Confidence: 0.9},
			{Severity: "low", Category: "style", File: "main.go", Line: 20, Evidence: "y", Explanation: "low issue", Suggestion: "optional", Confidence: 0.8},
		},
	})
	if err != nil {
		t.Fatalf("publish should succeed: %v", err)
	}

	// Summary + medium detail = 2 calls (low is still below "medium" floor).
	if len(fakePub.calls) != 2 {
		t.Fatalf("expected 2 publish calls (summary + medium detail), got %d", len(fakePub.calls))
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
