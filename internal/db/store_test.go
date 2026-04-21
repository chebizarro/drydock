package db

import (
	"context"
	"path/filepath"
	"testing"
)

func mustOpenStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func TestGetRecentFewShotsReturnsPositiveExamplesNewestFirst(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Insert 3 positive and 1 negative example
	if err := store.InsertFewShot(ctx, "patch1", "repo1", "positive", "good review 1", 0.9); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertFewShot(ctx, "patch2", "repo1", "negative", "bad review", 0.3); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertFewShot(ctx, "patch3", "repo1", "positive", "good review 2", 0.85); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertFewShot(ctx, "patch4", "repo1", "positive", "good review 3", 0.95); err != nil {
		t.Fatal(err)
	}

	// Fetch top 2 — should be the two most recent positives
	results, err := store.GetRecentFewShots(ctx, 2)
	if err != nil {
		t.Fatalf("GetRecentFewShots: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Most recent positive first
	if results[0] != "good review 3" {
		t.Errorf("results[0] = %q, want %q", results[0], "good review 3")
	}
	if results[1] != "good review 2" {
		t.Errorf("results[1] = %q, want %q", results[1], "good review 2")
	}
}

func TestGetRecentFewShotsExcludesNegativeExamples(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	if err := store.InsertFewShot(ctx, "patch1", "repo1", "negative", "this is not a finding", 0.4); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertFewShot(ctx, "patch2", "repo1", "positive", "good review", 0.9); err != nil {
		t.Fatal(err)
	}

	results, err := store.GetRecentFewShots(ctx, 10)
	if err != nil {
		t.Fatalf("GetRecentFewShots: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result (only positive), got %d", len(results))
	}
	if results[0] != "good review" {
		t.Errorf("result = %q, want %q", results[0], "good review")
	}
}

func TestGetRecentFewShotsEmptyTable(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	results, err := store.GetRecentFewShots(ctx, 5)
	if err != nil {
		t.Fatalf("GetRecentFewShots: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results from empty table, got %d", len(results))
	}
}

func TestGetRecentFewShotsZeroLimit(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	results, err := store.GetRecentFewShots(ctx, 0)
	if err != nil {
		t.Fatalf("GetRecentFewShots: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil for zero limit, got %v", results)
	}
}

func TestIsPatchSuperseded(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Seed: insert two patches in the same thread.
	// patch-1 is the root, patch-2 is a newer revision.
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO patch_events(event_id, repo_id, kind, author_pubkey, root_id, created_at, content, raw_event_json, seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"patch-1", "repo-1", 1617, "author-1", "patch-1", 1000, "diff v1", "{}", 1000,
	)
	if err != nil {
		t.Fatalf("insert patch-1: %v", err)
	}
	_, err = store.db.ExecContext(ctx,
		`INSERT INTO patch_events(event_id, repo_id, kind, author_pubkey, root_id, created_at, content, raw_event_json, seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"patch-2", "repo-1", 1617, "author-1", "patch-1", 2000, "diff v2", "{}", 2000,
	)
	if err != nil {
		t.Fatalf("insert patch-2: %v", err)
	}

	t.Run("root patch is superseded by child", func(t *testing.T) {
		sup, err := store.IsPatchSuperseded(ctx, "patch-1", "patch-1", "repo-1")
		if err != nil {
			t.Fatalf("IsPatchSuperseded: %v", err)
		}
		if !sup {
			t.Error("expected root patch to be superseded (child exists)")
		}
	})

	t.Run("latest patch is not superseded", func(t *testing.T) {
		sup, err := store.IsPatchSuperseded(ctx, "patch-2", "patch-1", "repo-1")
		if err != nil {
			t.Fatalf("IsPatchSuperseded: %v", err)
		}
		if sup {
			t.Error("expected latest patch to NOT be superseded")
		}
	})

	t.Run("single patch not superseded", func(t *testing.T) {
		// Insert an isolated patch.
		_, err := store.db.ExecContext(ctx,
			`INSERT INTO patch_events(event_id, repo_id, kind, author_pubkey, root_id, created_at, content, raw_event_json, seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"patch-solo", "repo-2", 1617, "author-1", "patch-solo", 3000, "solo diff", "{}", 3000,
		)
		if err != nil {
			t.Fatalf("insert patch-solo: %v", err)
		}
		sup, err := store.IsPatchSuperseded(ctx, "patch-solo", "patch-solo", "repo-2")
		if err != nil {
			t.Fatalf("IsPatchSuperseded: %v", err)
		}
		if sup {
			t.Error("expected solo patch to NOT be superseded")
		}
	})

	t.Run("different repo not counted", func(t *testing.T) {
		// patch-1 in repo-1 is superseded, but not in repo-3.
		sup, err := store.IsPatchSuperseded(ctx, "patch-1", "patch-1", "repo-3")
		if err != nil {
			t.Fatalf("IsPatchSuperseded: %v", err)
		}
		if sup {
			t.Error("expected patch in different repo to NOT be superseded")
		}
	})
}
