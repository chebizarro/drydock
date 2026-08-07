package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestContextVMFoundationMigrationFromLegacyReviewLog(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE review_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		patch_event_id TEXT NOT NULL,
		repo_id TEXT NOT NULL,
		status TEXT NOT NULL CHECK (status IN ('pending', 'reviewing', 'published', 'failed')),
		review_event_id TEXT,
		failure_reason TEXT,
		status_event_id TEXT,
		status_event_kind INTEGER NOT NULL DEFAULT 0,
		status_published_at INTEGER NOT NULL DEFAULT 0,
		force INTEGER NOT NULL DEFAULT 0 CHECK (force IN (0, 1)),
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		UNIQUE(patch_event_id, repo_id)
	);
	INSERT INTO review_log(patch_event_id, repo_id, status, force, created_at, updated_at)
	VALUES ('legacy-patch', 'legacy-repo', 'pending', 1, 1, 1);`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, column := range []string{"invocation", "requester_pubkey", "order_id"} {
		exists, err := store.hasColumn(ctx, "review_log", column)
		if err != nil || !exists {
			t.Fatalf("review_log.%s: exists=%v err=%v", column, exists, err)
		}
	}
	var invocation ReviewInvocation
	var requester, orderID string
	if err := store.db.QueryRowContext(ctx, `SELECT invocation, requester_pubkey, order_id
		FROM review_log WHERE patch_event_id='legacy-patch'`).Scan(&invocation, &requester, &orderID); err != nil {
		t.Fatal(err)
	}
	if invocation != ReviewInvocationReactive || requester != "" || orderID != "" {
		t.Fatalf("legacy metadata = %q %q %q", invocation, requester, orderID)
	}
	for _, table := range []string{
		"review_orders", "review_skips",
		"monitored_repository_list_state", "monitored_repository_members",
	} {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
			WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table %s: count=%d err=%v", table, count, err)
		}
	}
}

func TestReviewInvocationMetadataSurvivesRecovery(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	claim := ReviewClaim{
		Force:           true,
		Invocation:      ReviewInvocationContextVM,
		RequesterPubkey: "requester",
		OrderID:         "order-1",
	}
	acquired, err := store.BeginReviewWithClaim(ctx, "patch", "repo", claim)
	if err != nil || !acquired {
		t.Fatalf("BeginReviewWithClaim: acquired=%v err=%v", acquired, err)
	}
	if err := store.MarkReviewFailed(ctx, "patch", "repo", "temporary"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE review_log SET updated_at=0
		WHERE patch_event_id='patch' AND repo_id='repo'`); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.RequeueFailedReviews(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %#v", tasks)
	}
	got := tasks[0]
	if !got.Force || got.Invocation != claim.Invocation ||
		got.RequesterPubkey != claim.RequesterPubkey || got.OrderID != claim.OrderID {
		t.Fatalf("recovered task = %#v", got)
	}
}

func TestAcceptReviewOrderIdempotencyAndConflict(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	receipt := ReviewOrderReceipt{
		RequesterPubkey:   "requester",
		OrderID:           "order-1",
		RequestEventID:    "event-1",
		PatchEventID:      "patch-1",
		RepositoryID:      "owner:repo",
		RepositoryAddress: "30617:owner:repo",
		Force:             true,
		AcceptedAt:        123,
	}
	result, err := store.AcceptReviewOrder(ctx, receipt, ReviewClaim{})
	if err != nil || result.Disposition != ReviewOrderAcquired {
		t.Fatalf("first acceptance = %#v err=%v", result, err)
	}

	redelivery := receipt
	redelivery.RequestEventID = "event-2"
	redelivery.AcceptedAt = 999
	result, err = store.AcceptReviewOrder(ctx, redelivery, ReviewClaim{})
	if err != nil || result.Disposition != ReviewOrderIdempotent {
		t.Fatalf("redelivery = %#v err=%v", result, err)
	}
	if result.Receipt.RequestEventID != receipt.RequestEventID || result.Receipt.AcceptedAt != receipt.AcceptedAt {
		t.Fatalf("idempotent receipt = %#v", result.Receipt)
	}

	changed := receipt
	changed.PatchEventID = "patch-other"
	result, err = store.AcceptReviewOrder(ctx, changed, ReviewClaim{})
	if err != nil || result.Disposition != ReviewOrderConflict {
		t.Fatalf("changed order = %#v err=%v", result, err)
	}

	second := receipt
	second.OrderID = "order-2"
	second.RequestEventID = "event-3"
	result, err = store.AcceptReviewOrder(ctx, second, ReviewClaim{})
	if err != nil || result.Disposition != ReviewOrderConflict {
		t.Fatalf("second order same target = %#v err=%v", result, err)
	}
	if _, ok, err := store.GetReviewOrder(ctx, second.RequesterPubkey, second.OrderID); err != nil || ok {
		t.Fatalf("conflicting receipt persisted: ok=%v err=%v", ok, err)
	}

	var invocation ReviewInvocation
	var requester, orderID string
	if err := store.db.QueryRowContext(ctx, `SELECT invocation, requester_pubkey, order_id
		FROM review_log WHERE patch_event_id=? AND repo_id=?`,
		receipt.PatchEventID, receipt.RepositoryID,
	).Scan(&invocation, &requester, &orderID); err != nil {
		t.Fatal(err)
	}
	if invocation != ReviewInvocationContextVM || requester != receipt.RequesterPubkey || orderID != receipt.OrderID {
		t.Fatalf("review claim metadata = %q %q %q", invocation, requester, orderID)
	}
}

func TestMarkReviewSkippedIsDurableAndOnDemandCanReopen(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	if acquired, err := store.BeginReview(ctx, "patch", "repo"); err != nil || !acquired {
		t.Fatalf("BeginReview: acquired=%v err=%v", acquired, err)
	}
	if err := store.MarkReviewSkipped(ctx, "patch", "repo", "monitoring_removed"); err != nil {
		t.Fatal(err)
	}
	reason, ok, err := store.GetReviewSkip(ctx, "patch", "repo")
	if err != nil || !ok || reason != "monitoring_removed" {
		t.Fatalf("GetReviewSkip: reason=%q ok=%v err=%v", reason, ok, err)
	}
	status, err := store.GetReviewStatus(ctx, "patch", "repo")
	if err != nil || status != "failed" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	if tasks, err := store.RequeueFailedReviews(ctx, 0, 10); err != nil || len(tasks) != 0 {
		t.Fatalf("skipped review requeued: tasks=%#v err=%v", tasks, err)
	}

	acquired, err := store.BeginReviewWithClaim(ctx, "patch", "repo", ReviewClaim{
		Invocation: ReviewInvocationContextVM,
		OrderID:    "order",
	})
	if err != nil || !acquired {
		t.Fatalf("on-demand reopen: acquired=%v err=%v", acquired, err)
	}
	if _, ok, err := store.GetReviewSkip(ctx, "patch", "repo"); err != nil || ok {
		t.Fatalf("skip remained after reopen: ok=%v err=%v", ok, err)
	}
}
