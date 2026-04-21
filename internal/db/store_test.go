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
