package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
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

func TestOpenEnforcesForeignKeys(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	_, err := store.db.ExecContext(ctx, `INSERT INTO review_feedback (
		assignment_id, reviewer_pubkey, rater_pubkey, rating, comment, event_id, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)`, 999999, "reviewer", "rater", 5, "", "orphan-feedback", time.Now().Unix())
	if err == nil {
		t.Fatal("expected orphan feedback insert to violate assignment foreign key")
	}
}

func TestMigrateAppliesVersionedMigrationsIdempotently(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	var count, maxVersion int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&count, &maxVersion); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != len(schemaMigrations) {
		t.Fatalf("schema_migrations count = %d, want %d", count, len(schemaMigrations))
	}
	if maxVersion != schemaMigrations[len(schemaMigrations)-1].version {
		t.Fatalf("max schema version = %d, want %d", maxVersion, schemaMigrations[len(schemaMigrations)-1].version)
	}
}

func TestMigrateAddsReviewLogColumnsFromOldSnapshot(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	_, err = raw.ExecContext(ctx, `CREATE TABLE review_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		patch_event_id TEXT NOT NULL,
		repo_id TEXT NOT NULL,
		status TEXT NOT NULL,
		review_event_id TEXT,
		failure_reason TEXT,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		UNIQUE(patch_event_id, repo_id)
	)`)
	if err != nil {
		_ = raw.Close()
		t.Fatalf("create old review_log: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate old snapshot: %v", err)
	}

	for _, column := range []string{"status_event_id", "status_event_kind", "status_published_at", "force"} {
		exists, err := store.hasColumn(ctx, "review_log", column)
		if err != nil {
			t.Fatalf("hasColumn(%s): %v", column, err)
		}
		if !exists {
			t.Fatalf("expected migrated column %s", column)
		}
	}

	var name string
	if err := store.db.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=1`).Scan(&name); err != nil {
		t.Fatalf("schema migration version not recorded: %v", err)
	}
	if name != "review_log_status_event_columns" {
		t.Fatalf("migration name = %q", name)
	}
}

func TestMigrateAddsFeedbackDedupConstraintToExistingDatabase(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "old-feedback.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.ExecContext(ctx, `CREATE TABLE review_feedback (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		assignment_id INTEGER NOT NULL,
		reviewer_pubkey TEXT NOT NULL,
		rater_pubkey TEXT NOT NULL,
		rating INTEGER NOT NULL,
		comment TEXT NOT NULL DEFAULT '',
		event_id TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL
	);
	INSERT INTO review_feedback(assignment_id, reviewer_pubkey, rater_pubkey, rating, event_id, created_at)
	VALUES (1, 'reviewer', 'rater', 5, 'feedback-old-1', 1),
	(1, 'reviewer', 'rater', 4, 'feedback-old-2', 2);`)
	if err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID: "patch-old-feedback", RepoID: "repo-old-feedback",
		ReviewerPubkey: "reviewer", RequesterPubkey: "rater", Status: "completed",
		AssignmentEventID: "assignment-old-feedback", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("CreateAssignment: %v", err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_feedback WHERE assignment_id=1 AND rater_pubkey='rater'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("deduplicated feedback count = %d, want 1", count)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO review_feedback(
		assignment_id, reviewer_pubkey, rater_pubkey, rating, event_id, created_at
	) VALUES (1, 'reviewer', 'rater', 3, 'feedback-old-3', 3)`); err == nil {
		t.Fatal("expected migrated assignment/rater uniqueness to reject duplicate")
	}
}

func TestMigratePaymentMarketplaceSnapshotMatchesFreshSchema(t *testing.T) {
	ctx := context.Background()
	oldPath := filepath.Join(t.TempDir(), "old-payment-marketplace.db")
	raw, err := sql.Open("sqlite", oldPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.ExecContext(ctx, `
CREATE TABLE review_payments (
  patch_event_id TEXT PRIMARY KEY,
  repo_id TEXT NOT NULL,
  author_pubkey TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending', 'authorized')),
  access_kind TEXT NOT NULL DEFAULT ''
    CHECK (access_kind IN ('', 'free_tier', 'subscription', 'cashu_review', 'cashu_subscription')),
  requested_mode TEXT NOT NULL DEFAULT 'review'
    CHECK (requested_mode IN ('review', 'subscription')),
  token_hash TEXT,
  mint_url TEXT NOT NULL DEFAULT '',
  token_amount_sats INTEGER NOT NULL DEFAULT 0,
  invoice_id TEXT NOT NULL DEFAULT '',
  invoice_request TEXT NOT NULL DEFAULT '',
  invoice_expires_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE payment_subscriptions (
  author_pubkey TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  source_patch_event_id TEXT NOT NULL UNIQUE,
  source_token_hash TEXT NOT NULL UNIQUE,
  paid_amount_sats INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (author_pubkey, repo_id)
);
CREATE TABLE free_review_usage (
  author_pubkey TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  usage_day TEXT NOT NULL,
  used_count INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (author_pubkey, repo_id, usage_day)
);
CREATE TABLE reviewer_profiles (
  pubkey TEXT PRIMARY KEY,
  display_name TEXT NOT NULL DEFAULT '',
  languages TEXT NOT NULL DEFAULT '',
  domains TEXT NOT NULL DEFAULT '',
  availability TEXT NOT NULL DEFAULT 'available'
    CHECK (availability IN ('available', 'limited', 'unavailable')),
  price_per_review INTEGER NOT NULL DEFAULT 0,
  max_concurrent INTEGER NOT NULL DEFAULT 3,
  event_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE reviewer_reputations (
  pubkey TEXT PRIMARY KEY,
  overall_score REAL NOT NULL DEFAULT 0.5,
  total_reviews INTEGER NOT NULL DEFAULT 0,
  accepted_reviews INTEGER NOT NULL DEFAULT 0,
  rejected_reviews INTEGER NOT NULL DEFAULT 0,
  average_rating REAL NOT NULL DEFAULT 0,
  acceptance_rate REAL NOT NULL DEFAULT 0,
  last_review_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
CREATE TABLE review_assignments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  patch_event_id TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  reviewer_pubkey TEXT NOT NULL,
  requester_pubkey TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'accepted', 'rejected', 'completed', 'expired')),
  priority INTEGER NOT NULL DEFAULT 2,
  price_sats INTEGER NOT NULL DEFAULT 0,
  assignment_event_id TEXT NOT NULL UNIQUE,
  acceptance_event_id TEXT,
  completion_event_id TEXT,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(patch_event_id, reviewer_pubkey)
);
CREATE TABLE review_feedback (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  assignment_id INTEGER NOT NULL,
  reviewer_pubkey TEXT NOT NULL,
  rater_pubkey TEXT NOT NULL,
  rating INTEGER NOT NULL CHECK (rating >= 1 AND rating <= 5),
  comment TEXT NOT NULL DEFAULT '',
  event_id TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  FOREIGN KEY (assignment_id) REFERENCES review_assignments(id) ON DELETE CASCADE
);
INSERT INTO review_payments (
  patch_event_id, repo_id, author_pubkey, status, created_at, updated_at
) VALUES ('old-patch', 'old-repo', 'old-author', 'pending', 1, 1);`)
	if err != nil {
		_ = raw.Close()
		t.Fatalf("create old payment/marketplace snapshot: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(ctx, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if err := upgraded.Migrate(ctx); err != nil {
		t.Fatalf("migrate old payment/marketplace snapshot: %v", err)
	}

	fresh, err := Open(ctx, filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := fresh.Migrate(ctx); err != nil {
		t.Fatalf("migrate fresh database: %v", err)
	}

	for _, table := range []string{
		"review_payments",
		"payment_subscriptions",
		"free_review_usage",
		"reviewer_profiles",
		"reviewer_reputations",
		"review_assignments",
		"review_feedback",
		"marketplace_escrow_allocations",
		"marketplace_payouts",
		"marketplace_payout_audit",
	} {
		got := readTableColumns(t, ctx, upgraded.db, table)
		want := readTableColumns(t, ctx, fresh.db, table)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("PRAGMA table_info(%s) after upgrade = %#v, fresh = %#v", table, got, want)
		}
	}

	var paymentDDL string
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='review_payments'`,
	).Scan(&paymentDDL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(paymentDDL, "'token_spent'") {
		t.Fatalf("migrated review_payments constraint does not allow token_spent: %s", paymentDDL)
	}
	if _, err := upgraded.db.ExecContext(ctx,
		`UPDATE review_payments SET status='token_spent' WHERE patch_event_id='old-patch'`,
	); err != nil {
		t.Fatalf("migrated review_payments rejects token_spent: %v", err)
	}
}

type tableColumn struct {
	Name       string
	Type       string
	NotNull    int
	DefaultSQL sql.NullString
	PrimaryKey int
}

func readTableColumns(t *testing.T, ctx context.Context, db *sql.DB, table string) []tableColumn {
	t.Helper()
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()

	var columns []tableColumn
	for rows.Next() {
		var cid int
		var column tableColumn
		if err := rows.Scan(&cid, &column.Name, &column.Type, &column.NotNull, &column.DefaultSQL, &column.PrimaryKey); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info(%s): %v", table, err)
	}
	sort.Slice(columns, func(i, j int) bool { return columns[i].Name < columns[j].Name })
	return columns
}

func TestHasColumnPropagatesQueryErrors(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if _, err := store.hasColumn(ctx, "review_log", "status_event_id"); err == nil {
		t.Fatal("expected hasColumn to return query error after database close")
	}
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

func TestGetAndSetReviewEventID(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Seed a review_log entry via BeginReview.
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO review_log(patch_event_id, repo_id, status, created_at, updated_at)
		VALUES (?, ?, 'reviewing', 1000, 1000)`,
		"patch-id-1", "repo-id-1",
	)
	if err != nil {
		t.Fatalf("insert review_log: %v", err)
	}

	// Initially no review_event_id.
	eid, err := store.GetReviewEventID(ctx, "patch-id-1", "repo-id-1")
	if err != nil {
		t.Fatalf("GetReviewEventID: %v", err)
	}
	if eid != "" {
		t.Fatalf("expected empty review event id, got %q", eid)
	}

	// Set the review event ID.
	if err := store.SetReviewEventID(ctx, "patch-id-1", "repo-id-1", "review-evt-abc"); err != nil {
		t.Fatalf("SetReviewEventID: %v", err)
	}

	// Now it should be returned.
	eid, err = store.GetReviewEventID(ctx, "patch-id-1", "repo-id-1")
	if err != nil {
		t.Fatalf("GetReviewEventID after set: %v", err)
	}
	if eid != "review-evt-abc" {
		t.Fatalf("expected 'review-evt-abc', got %q", eid)
	}

	// Setting again should not overwrite (only updates when NULL).
	if err := store.SetReviewEventID(ctx, "patch-id-1", "repo-id-1", "review-evt-xyz"); err != nil {
		t.Fatalf("SetReviewEventID (second): %v", err)
	}
	eid, err = store.GetReviewEventID(ctx, "patch-id-1", "repo-id-1")
	if err != nil {
		t.Fatalf("GetReviewEventID after second set: %v", err)
	}
	if eid != "review-evt-abc" {
		t.Fatalf("expected original 'review-evt-abc', got %q (should not overwrite)", eid)
	}

	// Non-existent entry returns empty.
	eid, err = store.GetReviewEventID(ctx, "nonexistent", "repo-id-1")
	if err != nil {
		t.Fatalf("GetReviewEventID (nonexistent): %v", err)
	}
	if eid != "" {
		t.Fatalf("expected empty for nonexistent, got %q", eid)
	}
}

func TestBeginReviewForceReopensOnlyStatusSkipped(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	acquired, err := store.BeginReview(ctx, "patch", "repo", false)
	if err != nil || !acquired {
		t.Fatalf("initial BeginReview = %v, %v", acquired, err)
	}
	if err := store.MarkReviewFailed(ctx, "patch", "repo", "status_skipped:root status is draft"); err != nil {
		t.Fatalf("MarkReviewFailed: %v", err)
	}

	acquired, err = store.BeginReview(ctx, "patch", "repo", false)
	if err != nil {
		t.Fatalf("ordinary BeginReview: %v", err)
	}
	if acquired {
		t.Fatal("ordinary request reopened status_skipped review")
	}

	acquired, err = store.BeginReview(ctx, "patch", "repo", true)
	if err != nil || !acquired {
		t.Fatalf("forced BeginReview = %v, %v", acquired, err)
	}
	var status, reason string
	var force bool
	if err := store.db.QueryRowContext(ctx,
		`SELECT status, COALESCE(failure_reason, ''), force FROM review_log WHERE patch_event_id=? AND repo_id=?`,
		"patch", "repo").Scan(&status, &reason, &force); err != nil {
		t.Fatalf("read forced review: %v", err)
	}
	if status != "reviewing" || reason != "" || !force {
		t.Fatalf("forced review state = status %q reason %q force %v", status, reason, force)
	}
}

func TestRequeueFailedReviews(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Use real time so that the RequeueFailedReviews function's time.Now()
	// matches our test data expectations.
	now := time.Now().Unix()

	// Seed two failed reviews — one old enough to requeue, one too recent.
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO review_log(patch_event_id, repo_id, status, failure_reason, force, created_at, updated_at)
		VALUES (?, ?, 'failed', 'queue full', 1, ?, ?)`,
		"old-patch", "repo-1", now-600, now-600,
	)
	if err != nil {
		t.Fatalf("insert old failed: %v", err)
	}
	_, err = store.db.ExecContext(ctx,
		`INSERT INTO review_log(patch_event_id, repo_id, status, failure_reason, created_at, updated_at)
		VALUES (?, ?, 'failed', 'llm error', ?, ?)`,
		"recent-patch", "repo-1", now-60, now-60,
	)
	if err != nil {
		t.Fatalf("insert recent failed: %v", err)
	}
	// Also add one in pending state — should not be touched.
	_, err = store.db.ExecContext(ctx,
		`INSERT INTO review_log(patch_event_id, repo_id, status, created_at, updated_at)
		VALUES (?, ?, 'pending', ?, ?)`,
		"pending-patch", "repo-1", now-1000, now-1000,
	)
	if err != nil {
		t.Fatalf("insert pending: %v", err)
	}

	// Requeue with minAge=300s — only old-patch should match.
	tasks, err := store.RequeueFailedReviews(ctx, 300, 10)
	if err != nil {
		t.Fatalf("RequeueFailedReviews: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 requeued task, got %d", len(tasks))
	}
	if tasks[0].PatchEventID != "old-patch" {
		t.Fatalf("expected 'old-patch', got %q", tasks[0].PatchEventID)
	}
	if !tasks[0].Force {
		t.Fatal("requeued task lost persisted Force flag")
	}

	// Verify old-patch is now pending.
	var status string
	store.db.QueryRowContext(ctx,
		`SELECT status FROM review_log WHERE patch_event_id='old-patch'`,
	).Scan(&status)
	if status != "pending" {
		t.Fatalf("expected 'pending' after requeue, got %q", status)
	}

	// recent-patch should still be failed.
	store.db.QueryRowContext(ctx,
		`SELECT status FROM review_log WHERE patch_event_id='recent-patch'`,
	).Scan(&status)
	if status != "failed" {
		t.Fatalf("expected 'failed' for recent patch, got %q", status)
	}
}
