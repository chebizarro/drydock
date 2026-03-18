package ingest_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"drydock/internal/db"
	"drydock/internal/ingest"

	"fiatjaf.com/nostr"
)

func TestProcessorDedupesByEventID(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	event := nostr.Event{
		ID:        nostr.MustIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		PubKey:    nostr.MustPubKeyFromHex("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"),
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "repo-1"},
			{"name", "Repo One"},
			{"clone", "https://example.com/repo-1.git"},
		},
	}

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

	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	event := nostr.Event{
		ID:        nostr.MustIDFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		PubKey:    nostr.MustPubKeyFromHex("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798:repo-1"},
			{"e", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "", "root"},
		},
		Content: "diff --git a/main.go b/main.go\nindex 0000000..1111111 100644\n--- a/main.go\n+++ b/main.go\n@@ -0,0 +1 @@\n+package main\n",
	}

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

func TestProcessorSkipsPatchWhenSnapshotAlreadyContainsTip(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	repoOwner := "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	snapshotTip := "1111111111111111111111111111111111111111"

	snapshot := nostr.Event{
		ID:        nostr.MustIDFromHex("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"),
		PubKey:    nostr.MustPubKeyFromHex(repoOwner),
		Kind:      30618,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "repo-1"},
			{"refs/heads/main", snapshotTip},
			{"HEAD", "ref: refs/heads/main"},
		},
	}

	if err := processor.ProcessEvent(ctx, snapshot, "wss://relay.test"); err != nil {
		t.Fatalf("process snapshot failed: %v", err)
	}

	patch := nostr.Event{
		ID:        nostr.MustIDFromHex("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"),
		PubKey:    nostr.MustPubKeyFromHex("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		Kind:      1618,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + repoOwner + ":repo-1"},
			{"c", snapshotTip},
			{"e", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "", "root"},
		},
	}

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

	repoOwner := "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	if err := processor.ProcessEvent(ctx, nostr.Event{
		ID:        nostr.MustIDFromHex("5757575757575757575757575757575757575757575757575757575757575757"),
		PubKey:    nostr.MustPubKeyFromHex(repoOwner),
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"d", "repo-1"}},
	}, "wss://relay.test"); err != nil {
		t.Fatalf("process announcement failed: %v", err)
	}
	rootPatchID := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	status := nostr.Event{
		ID:        nostr.MustIDFromHex("5656565656565656565656565656565656565656565656565656565656565656"),
		PubKey:    nostr.MustPubKeyFromHex(repoOwner),
		Kind:      1632,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + repoOwner + ":repo-1"},
			{"e", rootPatchID, "", "root"},
		},
	}
	if err := processor.ProcessEvent(ctx, status, "wss://relay.test"); err != nil {
		t.Fatalf("process status failed: %v", err)
	}

	patch := nostr.Event{
		ID:        nostr.MustIDFromHex("6767676767676767676767676767676767676767676767676767676767676767"),
		PubKey:    nostr.MustPubKeyFromHex("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"a", "30617:" + repoOwner + ":repo-1"}, {"e", rootPatchID, "", "root"}},
	}
	if err := processor.ProcessEvent(ctx, patch, "wss://relay.test"); err != nil {
		t.Fatalf("process patch failed: %v", err)
	}

	reviewRows, err := store.CountReviewLog(ctx)
	if err != nil {
		t.Fatalf("count review log: %v", err)
	}
	if reviewRows != 0 {
		t.Fatalf("expected 0 review rows when root is closed, got %d", reviewRows)
	}
}

func TestProcessorIgnoresUnauthorizedClosedStatus(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	repoOwner := "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	if err := processor.ProcessEvent(ctx, nostr.Event{
		ID:        nostr.MustIDFromHex("7878787878787878787878787878787878787878787878787878787878787878"),
		PubKey:    nostr.MustPubKeyFromHex(repoOwner),
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"d", "repo-1"}},
	}, "wss://relay.test"); err != nil {
		t.Fatalf("process announcement failed: %v", err)
	}

	rootPatchID := "8989898989898989898989898989898989898989898989898989898989898989"
	status := nostr.Event{
		ID:        nostr.MustIDFromHex("8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a8a"),
		PubKey:    nostr.MustPubKeyFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Kind:      1632,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"a", "30617:" + repoOwner + ":repo-1"}, {"e", rootPatchID, "", "root"}},
	}
	if err := processor.ProcessEvent(ctx, status, "wss://relay.test"); err != nil {
		t.Fatalf("process status failed: %v", err)
	}

	patch := nostr.Event{
		ID:        nostr.MustIDFromHex("8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b"),
		PubKey:    nostr.MustPubKeyFromHex("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"a", "30617:" + repoOwner + ":repo-1"}, {"e", rootPatchID, "", "root"}},
	}
	if err := processor.ProcessEvent(ctx, patch, "wss://relay.test"); err != nil {
		t.Fatalf("process patch failed: %v", err)
	}

	reviewRows, err := store.CountReviewLog(ctx)
	if err != nil {
		t.Fatalf("count review log: %v", err)
	}
	if reviewRows != 1 {
		t.Fatalf("expected 1 review row (status should be ignored), got %d", reviewRows)
	}
}

func TestProcessorUsesEAsRootForPRUpdates(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	processor := ingest.NewProcessor(store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	repoOwner := "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	rootPRID := "9999999999999999999999999999999999999999999999999999999999999999"

	evt := nostr.Event{
		ID:        nostr.MustIDFromHex("4545454545454545454545454545454545454545454545454545454545454545"),
		PubKey:    nostr.MustPubKeyFromHex("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		Kind:      1619,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + repoOwner + ":repo-1"},
			{"E", rootPRID},
			{"P", repoOwner},
			{"c", "1111111111111111111111111111111111111111"},
		},
	}
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
