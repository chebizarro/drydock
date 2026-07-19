package ingest_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"drydock/internal/db"
	"drydock/internal/ingest"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
)

// signEvent signs the event with the given secret key (sets ID, PubKey, Sig).
func signEvent(t *testing.T, sk nostr.SecretKey, event *nostr.Event) {
	t.Helper()
	if err := event.Sign(sk); err != nil {
		t.Fatalf("sign event: %v", err)
	}
}

func TestProcessorRejectsInvalidSignature(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	// Create an event with no valid signature (forged event)
	event := nostr.Event{
		ID:        nostr.MustIDFromHex("f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0"),
		PubKey:    nostr.MustPubKeyFromHex("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"),
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "repo-1"},
		},
		// Sig is zero/empty — invalid
	}

	// Should not error (drops silently) but should not persist
	if err := processor.ProcessEvent(ctx, event, "wss://relay.test"); err != nil {
		t.Fatalf("process should not error on invalid sig: %v", err)
	}

	ingested, err := store.CountIngestedEvents(ctx)
	if err != nil {
		t.Fatalf("count ingested events: %v", err)
	}
	if ingested != 0 {
		t.Fatalf("expected 0 ingested events for invalid signature, got %d", ingested)
	}
}

func TestProcessorRejectsIDMismatchBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	repoSK := nostr.Generate()
	patchSK := nostr.Generate()
	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	repoEvt := nostr.Event{
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"d", "repo-1"}, {"clone", "https://example.com/repo-1.git"}},
	}
	signEvent(t, repoSK, &repoEvt)
	if err := store.UpsertRepositoryAnnouncement(ctx, repoEvt); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	bad := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":repo-1"},
			{"e", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", "root"},
		},
		Content: "diff --git a/main.go b/main.go\n+package main\n",
	}
	signEvent(t, patchSK, &bad)
	bad.ID = nostr.MustIDFromHex("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	if bad.CheckID() {
		t.Fatal("test setup expected mismatched event ID")
	}
	if !bad.VerifySignature() {
		t.Fatal("test setup expected signature to remain valid for event body")
	}

	if err := processor.ProcessEvent(ctx, bad, "wss://relay.test"); err != nil {
		t.Fatalf("process should not error on invalid ID: %v", err)
	}
	select {
	case task := <-processor.ReviewQueue:
		t.Fatalf("invalid-ID event was dispatched to review queue: %+v", task)
	default:
	}
	patches, err := store.CountPatchEvents(ctx)
	if err != nil {
		t.Fatalf("count patch events: %v", err)
	}
	if patches != 0 {
		t.Fatalf("expected invalid-ID event not to persist as patch, got %d", patches)
	}

	valid := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":repo-1"},
			{"e", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "", "root"},
		},
		Content: "diff --git a/valid.go b/valid.go\n+package main\n",
	}
	signEvent(t, patchSK, &valid)
	if !valid.CheckID() || !valid.VerifySignature() {
		t.Fatal("test setup expected valid signed event")
	}
	if err := processor.ProcessEvent(ctx, valid, "wss://relay.test"); err != nil {
		t.Fatalf("process valid event failed: %v", err)
	}
	select {
	case task := <-processor.ReviewQueue:
		if task.PatchEventID != valid.ID.Hex() || task.RepoID == "" {
			t.Fatalf("unexpected review task: %+v", task)
		}
	default:
		t.Fatal("valid event was not dispatched to review queue")
	}
}

func TestProcessorRejectsFutureTimestamp(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	sk := nostr.Generate()

	event := nostr.Event{
		Kind:      30617,
		CreatedAt: nostr.Timestamp(time.Now().Add(11 * time.Minute).Unix()),
		Tags:      nostr.Tags{{"d", "repo-1"}},
	}
	signEvent(t, sk, &event)
	if !event.CheckID() || !event.VerifySignature() {
		t.Fatal("test setup expected signed event with valid integrity")
	}

	if err := processor.ProcessEvent(ctx, event, "wss://relay.test"); err != nil {
		t.Fatalf("process should not error on future timestamp: %v", err)
	}
	ingested, err := store.CountIngestedEvents(ctx)
	if err != nil {
		t.Fatalf("count ingested events: %v", err)
	}
	if ingested != 0 {
		t.Fatalf("expected 0 ingested events for future timestamp, got %d", ingested)
	}
}

func TestProcessorDedupesByEventID(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	sk := nostr.Generate()

	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	event := nostr.Event{
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "repo-1"},
			{"name", "Repo One"},
			{"clone", "https://example.com/repo-1.git"},
		},
	}
	signEvent(t, sk, &event)

	if err := processor.ProcessEvent(ctx, event, "wss://relay.test"); err != nil {
		t.Fatalf("first process failed: %v", err)
	}
	if err := processor.ProcessEvent(ctx, event, "wss://relay.test"); err != nil {
		t.Fatalf("second process failed: %v", err)
	}

	ingested, err := store.CountIngestedEvents(ctx)
	if err != nil {
		t.Fatalf("count ingested events: %v", err)
	}
	if ingested != 1 {
		t.Fatalf("expected 1 ingested event, got %d", ingested)
	}

	repos, err := store.CountRepositories(ctx)
	if err != nil {
		t.Fatalf("count repositories: %v", err)
	}
	if repos != 1 {
		t.Fatalf("expected 1 repository, got %d", repos)
	}
}

func TestProcessorCreatesPatchReviewGateOnce(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	repoSK := nostr.Generate()
	patchSK := nostr.Generate()

	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	// First, seed the repo announcement so the patch has a valid repo_id
	repoEvt := nostr.Event{
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "repo-1"},
			{"clone", "https://example.com/repo-1.git"},
		},
	}
	signEvent(t, repoSK, &repoEvt)
	if err := processor.ProcessEvent(ctx, repoEvt, "wss://relay.test"); err != nil {
		t.Fatalf("process repo failed: %v", err)
	}

	event := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":repo-1"},
			{"e", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "", "root"},
		},
		Content: "diff --git a/main.go b/main.go\nindex 0000000..1111111 100644\n--- a/main.go\n+++ b/main.go\n@@ -0,0 +1 @@\n+package main\n",
	}
	signEvent(t, patchSK, &event)

	if err := processor.ProcessEvent(ctx, event, "wss://relay.test"); err != nil {
		t.Fatalf("first process failed: %v", err)
	}
	if err := processor.ProcessEvent(ctx, event, "wss://relay.test"); err != nil {
		t.Fatalf("second process failed: %v", err)
	}

	ingested, err := store.CountIngestedEvents(ctx)
	if err != nil {
		t.Fatalf("count ingested events: %v", err)
	}
	// 2 events: repo announcement + patch
	if ingested != 2 {
		t.Fatalf("expected 2 ingested events, got %d", ingested)
	}

	patches, err := store.CountPatchEvents(ctx)
	if err != nil {
		t.Fatalf("count patch events: %v", err)
	}
	if patches != 1 {
		t.Fatalf("expected 1 patch event, got %d", patches)
	}

	reviewRows, err := store.CountReviewLog(ctx)
	if err != nil {
		t.Fatalf("count review log: %v", err)
	}
	if reviewRows != 1 {
		t.Fatalf("expected 1 review_log row, got %d", reviewRows)
	}
}

func TestProcessorAppliesRepositoryOwnerScopeBeforeReview(t *testing.T) {
	tests := []struct {
		name    string
		allowed bool
	}{
		{name: "allowed owner", allowed: true},
		{name: "denied owner", allowed: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := mustOpenStore(t, ctx)
			repoSK := nostr.Generate()
			repoOwner := nostr.GetPublicKey(repoSK).Hex()
			allowedOwner := repoOwner
			if !test.allowed {
				allowedOwner = nostr.GetPublicKey(nostr.Generate()).Hex()
			}
			processor := ingest.NewProcessor(
				store,
				slog.New(slog.NewJSONHandler(io.Discard, nil)),
				ingest.WithRepositoryScope(scope.NewMatcher(nil, []string{allowedOwner})),
			)

			repoEvt := nostr.Event{
				Kind:      30617,
				CreatedAt: nostr.Now(),
				Tags:      nostr.Tags{{"d", "repo-1"}, {"clone", "https://example.com/repo-1.git"}},
			}
			signEvent(t, repoSK, &repoEvt)
			if err := processor.ProcessEvent(ctx, repoEvt, "wss://relay.test"); err != nil {
				t.Fatalf("process repo: %v", err)
			}

			patch := nostr.Event{
				Kind:      1617,
				CreatedAt: nostr.Now(),
				Tags: nostr.Tags{
					{"a", "30617:" + repoOwner + ":repo-1"},
					{"e", "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", "", "root"},
				},
				Content: "diff --git a/main.go b/main.go\n+package main\n",
			}
			signEvent(t, nostr.Generate(), &patch)
			if err := processor.ProcessEvent(ctx, patch, "wss://relay.test"); err != nil {
				t.Fatalf("process patch: %v", err)
			}

			reviewRows, err := store.CountReviewLog(ctx)
			if err != nil {
				t.Fatalf("count review log: %v", err)
			}
			if test.allowed {
				if reviewRows != 1 {
					t.Fatalf("expected allowed patch to begin review, got %d review rows", reviewRows)
				}
				select {
				case <-processor.ReviewQueue:
				default:
					t.Fatal("expected allowed patch in review queue")
				}
			} else {
				if reviewRows != 0 {
					t.Fatalf("expected denied patch to skip BeginReview, got %d review rows", reviewRows)
				}
				select {
				case task := <-processor.ReviewQueue:
					t.Fatalf("denied patch was queued: %+v", task)
				default:
				}
			}
		})
	}
}

func TestProcessorSkipsPatchWhenSnapshotAlreadyContainsTip(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	repoSK := nostr.Generate()
	patchSK := nostr.Generate()
	snapshotTip := "1111111111111111111111111111111111111111"

	snapshot := nostr.Event{
		Kind:      30618,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "repo-1"},
			{"refs/heads/main", snapshotTip},
			{"HEAD", "ref: refs/heads/main"},
		},
	}
	signEvent(t, repoSK, &snapshot)

	if err := processor.ProcessEvent(ctx, snapshot, "wss://relay.test"); err != nil {
		t.Fatalf("process snapshot failed: %v", err)
	}

	patch := nostr.Event{
		Kind:      1618,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":repo-1"},
			{"c", snapshotTip},
			{"e", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "", "root"},
		},
	}
	signEvent(t, patchSK, &patch)

	if err := processor.ProcessEvent(ctx, patch, "wss://relay.test"); err != nil {
		t.Fatalf("process patch failed: %v", err)
	}

	snapshots, err := store.CountRepositorySnapshots(ctx)
	if err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if snapshots != 1 {
		t.Fatalf("expected 1 snapshot, got %d", snapshots)
	}

	patches, err := store.CountPatchEvents(ctx)
	if err != nil {
		t.Fatalf("count patch events: %v", err)
	}
	if patches != 1 {
		t.Fatalf("expected patch to be persisted, got %d", patches)
	}

	reviewRows, err := store.CountReviewLog(ctx)
	if err != nil {
		t.Fatalf("count review log: %v", err)
	}
	if reviewRows != 0 {
		t.Fatalf("expected 0 review_log rows for stale patch, got %d", reviewRows)
	}
}

func TestProcessorSkipsWhenRootStatusClosed(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	repoSK := nostr.Generate()
	patchSK := nostr.Generate()

	repoEvt := nostr.Event{
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"d", "repo-1"}},
	}
	signEvent(t, repoSK, &repoEvt)
	if err := processor.ProcessEvent(ctx, repoEvt, "wss://relay.test"); err != nil {
		t.Fatalf("process announcement failed: %v", err)
	}

	// Create a root patch event so the status author check works
	rootPatch := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":repo-1"},
		},
		Content: "diff",
	}
	signEvent(t, patchSK, &rootPatch)
	if err := processor.ProcessEvent(ctx, rootPatch, "wss://relay.test"); err != nil {
		t.Fatalf("process root patch failed: %v", err)
	}

	status := nostr.Event{
		Kind:      1632,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":repo-1"},
			{"e", rootPatch.ID.Hex(), "", "root"},
		},
	}
	signEvent(t, repoSK, &status) // signed by repo owner = authorized
	if err := processor.ProcessEvent(ctx, status, "wss://relay.test"); err != nil {
		t.Fatalf("process status failed: %v", err)
	}

	// New patch on the same root — should be skipped because root is closed
	patch2 := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":repo-1"}, {"e", rootPatch.ID.Hex(), "", "root"}},
		Content:   "diff2",
	}
	signEvent(t, patchSK, &patch2)
	if err := processor.ProcessEvent(ctx, patch2, "wss://relay.test"); err != nil {
		t.Fatalf("process patch failed: %v", err)
	}

	// The root patch should have review_log but the second should be skipped
	reviewRows, err := store.CountReviewLog(ctx)
	if err != nil {
		t.Fatalf("count review log: %v", err)
	}
	// 1 row from root patch; second patch skipped because root is closed
	if reviewRows != 1 {
		t.Fatalf("expected 1 review row (second patch skipped for closed root), got %d", reviewRows)
	}
}

func TestProcessorIgnoresUnauthorizedClosedStatus(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	repoSK := nostr.Generate()
	patchSK := nostr.Generate()
	randomSK := nostr.Generate() // unauthorized third party

	repoEvt := nostr.Event{
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"d", "repo-1"}},
	}
	signEvent(t, repoSK, &repoEvt)
	if err := processor.ProcessEvent(ctx, repoEvt, "wss://relay.test"); err != nil {
		t.Fatalf("process announcement failed: %v", err)
	}

	// Create a root patch
	rootPatch := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":repo-1"},
		},
		Content: "diff",
	}
	signEvent(t, patchSK, &rootPatch)
	if err := processor.ProcessEvent(ctx, rootPatch, "wss://relay.test"); err != nil {
		t.Fatalf("process root patch failed: %v", err)
	}

	// Status from unauthorized third party — should be ignored
	status := nostr.Event{
		Kind:      1632,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":repo-1"},
			{"e", rootPatch.ID.Hex(), "", "root"},
		},
	}
	signEvent(t, randomSK, &status) // signed by random user = unauthorized
	if err := processor.ProcessEvent(ctx, status, "wss://relay.test"); err != nil {
		t.Fatalf("process status failed: %v", err)
	}

	// Another patch on the same root — should NOT be skipped because status was unauthorized
	patch2 := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":repo-1"}, {"e", rootPatch.ID.Hex(), "", "root"}},
		Content:   "diff2",
	}
	signEvent(t, patchSK, &patch2)
	if err := processor.ProcessEvent(ctx, patch2, "wss://relay.test"); err != nil {
		t.Fatalf("process patch failed: %v", err)
	}

	reviewRows, err := store.CountReviewLog(ctx)
	if err != nil {
		t.Fatalf("count review log: %v", err)
	}
	// Both patches should have review_log entries (status from random user is ignored)
	if reviewRows != 2 {
		t.Fatalf("expected 2 review rows (unauthorized status should be ignored), got %d", reviewRows)
	}
}

func TestProcessorUsesEAsRootForPRUpdates(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	repoSK := nostr.Generate()
	patchSK := nostr.Generate()
	rootPRID := "9999999999999999999999999999999999999999999999999999999999999999"

	evt := nostr.Event{
		Kind:      1619,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":repo-1"},
			{"E", rootPRID},
			{"P", nostr.GetPublicKey(repoSK).Hex()},
			{"c", "1111111111111111111111111111111111111111"},
		},
	}
	signEvent(t, patchSK, &evt)
	if err := processor.ProcessEvent(ctx, evt, "wss://relay.test"); err != nil {
		t.Fatalf("process pr update failed: %v", err)
	}
	rec, err := store.GetPatchEvent(ctx, evt.ID.Hex())
	if err != nil {
		t.Fatalf("get patch event: %v", err)
	}
	if rec.RootID != rootPRID {
		t.Fatalf("expected root_id=%s got %s", rootPRID, rec.RootID)
	}
}

type blockingConversationHandler struct {
	started chan struct{}
	release chan struct{}
}

func (h *blockingConversationHandler) IsReplyToUs(context.Context, nostr.Event) bool {
	return true
}

func (h *blockingConversationHandler) HandleReply(context.Context, nostr.Event, string) error {
	close(h.started)
	<-h.release
	return nil
}

func TestProcessorDrainsSynchronousHandlerDuringShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := mustOpenStore(t, ctx)
	handler := &blockingConversationHandler{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)), ingest.WithConversation(handler))
	event := nostr.Event{Kind: nostr.KindComment, CreatedAt: nostr.Now(), Content: "reply"}
	signEvent(t, nostr.Generate(), &event)

	done := make(chan error, 1)
	go func() {
		done <- processor.ProcessEvent(ctx, event, "wss://relay.test")
	}()

	select {
	case <-handler.started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	cancel()
	select {
	case err := <-done:
		t.Fatalf("processor returned before in-flight handler drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(handler.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("process event: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("processor did not return after handler drained")
	}
}

func TestProcessorMarksTaskForRetryWhenQueueFull(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	repoSK := nostr.Generate()
	patchSK := nostr.Generate()

	repoEvt := nostr.Event{
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"d", "repo-1"}},
	}
	signEvent(t, repoSK, &repoEvt)
	if err := store.UpsertRepositoryAnnouncement(ctx, repoEvt); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	// Also insert the ingested event so the processor doesn't reject it for sig check
	if _, err := store.InsertIngestedEvent(ctx, repoEvt); err != nil {
		t.Fatalf("insert ingested repo event: %v", err)
	}

	// Create processor with a tiny queue to test overflow.
	smallProcessor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	// Fill the queue completely.
	for i := 0; i < cap(smallProcessor.ReviewQueue); i++ {
		smallProcessor.ReviewQueue <- db.ReviewTask{PatchEventID: "filler", RepoID: "filler"}
	}

	patch := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":repo-1"},
			{"e", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "", "root"},
		},
		Content: "diff",
	}
	signEvent(t, patchSK, &patch)

	// Processing must fail when the queue is full so the listener does not checkpoint past it.
	if err := smallProcessor.ProcessEvent(ctx, patch, "wss://relay.test"); err == nil {
		t.Fatal("expected queue-full processing error")
	}
}

func mustOpenStore(t *testing.T, ctx context.Context) *db.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "drydock-test.db")
	store, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}
