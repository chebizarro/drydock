package db

import (
	"context"
	"testing"
	"time"
)

func TestInsertZapReceiptClaimsPaymentBlockedReview(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	const patchID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const repoID = "owner:repo"

	acquired, err := store.BeginReview(ctx, patchID, repoID)
	if err != nil || !acquired {
		t.Fatalf("BeginReview = %v, %v", acquired, err)
	}
	if err := store.MarkReviewFailed(ctx, patchID, repoID, "payment_blocked:no_payment"); err != nil {
		t.Fatal(err)
	}

	receipt := ZapReceiptRecord{
		EventID:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		PatchEventID:  patchID,
		PayerPubkey:   "payer",
		ReceiptAuthor: "zapper",
		AmountMSat:    100_000,
		CreatedAt:     time.Now().Unix(),
	}
	inserted, tasks, err := store.InsertZapReceiptAndClaimBlockedReviews(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted || len(tasks) != 1 || tasks[0].PatchEventID != patchID || tasks[0].RepoID != repoID {
		t.Fatalf("unexpected insert result: inserted=%v tasks=%+v", inserted, tasks)
	}
	status, err := store.GetReviewStatus(ctx, patchID, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if status != "reviewing" {
		t.Fatalf("status = %q, want reviewing", status)
	}
	note, err := store.GetReviewNote(ctx, patchID, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("failure reason was not cleared: %q", note)
	}

	inserted, tasks, err = store.InsertZapReceiptAndClaimBlockedReviews(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if inserted || len(tasks) != 0 {
		t.Fatalf("duplicate receipt should be idempotent: inserted=%v tasks=%+v", inserted, tasks)
	}
}

func TestMarkReviewPaymentBlockedUsesZapCursor(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	const blockedPatch = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	acquired, err := store.BeginReview(ctx, blockedPatch, "owner:blocked")
	if err != nil || !acquired {
		t.Fatalf("BeginReview = %v, %v", acquired, err)
	}
	advanced, err := store.MarkReviewPaymentBlocked(ctx, blockedPatch, "owner:blocked", "no_payment", 0)
	if err != nil {
		t.Fatal(err)
	}
	if advanced {
		t.Fatal("cursor unexpectedly advanced")
	}
	status, err := store.GetReviewStatus(ctx, blockedPatch, "owner:blocked")
	if err != nil || status != "failed" {
		t.Fatalf("blocked review status = %q, %v", status, err)
	}

	const racingPatch = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	acquired, err = store.BeginReview(ctx, racingPatch, "owner:racing")
	if err != nil || !acquired {
		t.Fatalf("BeginReview = %v, %v", acquired, err)
	}
	if _, _, err := store.InsertZapReceiptAndClaimBlockedReviews(ctx, ZapReceiptRecord{
		EventID:      "abababababababababababababababababababababababababababababababab",
		PatchEventID: racingPatch, ReceiptAuthor: "zapper", AmountMSat: 100_000, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	advanced, err = store.MarkReviewPaymentBlocked(ctx, racingPatch, "owner:racing", "no_payment", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !advanced {
		t.Fatal("new receipt should advance cursor and refuse payment block")
	}
	status, err = store.GetReviewStatus(ctx, racingPatch, "owner:racing")
	if err != nil || status != "reviewing" {
		t.Fatalf("racing review status = %q, %v", status, err)
	}
}

func TestInsertZapReceiptDoesNotClaimOtherFailures(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	const patchID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	const repoID = "owner:repo"
	acquired, err := store.BeginReview(ctx, patchID, repoID)
	if err != nil || !acquired {
		t.Fatalf("BeginReview = %v, %v", acquired, err)
	}
	if err := store.MarkReviewFailed(ctx, patchID, repoID, "temporary failure"); err != nil {
		t.Fatal(err)
	}
	_, tasks, err := store.InsertZapReceiptAndClaimBlockedReviews(ctx, ZapReceiptRecord{
		EventID:      "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		PatchEventID: patchID, ReceiptAuthor: "zapper", AmountMSat: 1, CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("non-payment failure was claimed: %+v", tasks)
	}
}
