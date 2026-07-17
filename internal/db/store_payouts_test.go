package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func TestMarketplacePayoutTransitionsAndPersistenceFaults(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	reviewerSK := nostr.Generate()
	assignmentID, assignmentEventID, completionEventID, reviewEventID := seedMarketplacePayout(t, ctx, store, reviewerSK)
	escrow, err := store.GetMarketplaceEscrowAllocation(ctx, assignmentID)
	if err != nil || escrow.Status != "reserved" || escrow.AmountSats != 321 {
		t.Fatalf("initial escrow=%+v err=%v", escrow, err)
	}

	mustExec(t, store, `CREATE TRIGGER fail_completion_update BEFORE UPDATE ON review_assignments
		WHEN NEW.status = 'completed'
		BEGIN SELECT RAISE(ABORT, 'injected completion failure'); END`)
	if _, _, err := store.CompleteAssignmentAndAllocatePayout(ctx, assignmentEventID,
		nostr.GetPublicKey(reviewerSK).Hex(), completionEventID, reviewEventID, time.Now().Unix()); err == nil {
		t.Fatal("expected completion update fault")
	}
	assignment, _ := store.GetAssignmentByID(ctx, assignmentID)
	if assignment.Status != "accepted" {
		t.Fatalf("completion fault changed assignment: %q", assignment.Status)
	}
	mustExec(t, store, "DROP TRIGGER fail_completion_update")

	mustExec(t, store, `CREATE TRIGGER fail_payout_allocation BEFORE INSERT ON marketplace_payouts
		BEGIN SELECT RAISE(ABORT, 'injected allocation failure'); END`)
	if _, _, err := store.CompleteAssignmentAndAllocatePayout(ctx, assignmentEventID,
		nostr.GetPublicKey(reviewerSK).Hex(), completionEventID, reviewEventID, time.Now().Unix()); err == nil {
		t.Fatal("expected allocation fault")
	}
	assignment, _ = store.GetAssignmentByID(ctx, assignmentID)
	if assignment.Status != "accepted" {
		t.Fatalf("allocation fault did not roll back completion: %q", assignment.Status)
	}
	mustExec(t, store, "DROP TRIGGER fail_payout_allocation")

	payout, hasPayout, err := store.CompleteAssignmentAndAllocatePayout(ctx, assignmentEventID,
		nostr.GetPublicKey(reviewerSK).Hex(), completionEventID, reviewEventID, time.Now().Unix())
	if err != nil || !hasPayout || payout.Status != "pending" || payout.AmountSats != 321 {
		t.Fatalf("allocate payout=%+v has=%v err=%v", payout, hasPayout, err)
	}

	mustExec(t, store, `CREATE TRIGGER fail_payout_submitted_audit BEFORE INSERT ON marketplace_payout_audit
		WHEN NEW.to_status = 'submitted'
		BEGIN SELECT RAISE(ABORT, 'injected submitted audit failure'); END`)
	if _, err := store.MarkMarketplacePayoutSubmitted(ctx, assignmentID, time.Now().Unix()); err == nil {
		t.Fatal("expected submitted audit fault")
	}
	payout, _ = store.GetMarketplacePayout(ctx, assignmentID)
	if payout.Status != "pending" {
		t.Fatalf("submitted audit fault did not roll back payout: %q", payout.Status)
	}
	mustExec(t, store, "DROP TRIGGER fail_payout_submitted_audit")

	claimed, err := store.MarkMarketplacePayoutSubmitted(ctx, assignmentID, time.Now().Unix())
	if err != nil || !claimed {
		t.Fatalf("claim submission: claimed=%v err=%v", claimed, err)
	}
	claimed, err = store.MarkMarketplacePayoutSubmitted(ctx, assignmentID, time.Now().Unix())
	if err != nil || claimed {
		t.Fatalf("duplicate submission claim: claimed=%v err=%v", claimed, err)
	}

	mustExec(t, store, `CREATE TRIGGER fail_payout_settled_audit BEFORE INSERT ON marketplace_payout_audit
		WHEN NEW.to_status = 'settled'
		BEGIN SELECT RAISE(ABORT, 'injected settled audit failure'); END`)
	preimage := strings.Repeat("22", 32)
	preimageBytes, _ := hex.DecodeString(preimage)
	derivedHash := sha256.Sum256(preimageBytes)
	hash := hex.EncodeToString(derivedHash[:])
	if err := store.MarkMarketplacePayoutSettled(ctx, assignmentID, hash, preimage, time.Now().Unix(), time.Now().Unix()); err == nil {
		t.Fatal("expected settled audit fault")
	}
	escrow, _ = store.GetMarketplaceEscrowAllocation(ctx, assignmentID)
	if escrow.Status != "reserved" {
		t.Fatalf("settlement fault consumed escrow: %+v", escrow)
	}
	payout, _ = store.GetMarketplacePayout(ctx, assignmentID)
	if payout.Status != "submitted" {
		t.Fatalf("settled audit fault did not roll back payout: %q", payout.Status)
	}
	mustExec(t, store, "DROP TRIGGER fail_payout_settled_audit")

	settledAt := time.Now().Unix()
	if err := store.MarkMarketplacePayoutSettled(ctx, assignmentID, hash, preimage, settledAt, settledAt); err != nil {
		t.Fatalf("settle payout: %v", err)
	}
	payout, _ = store.GetMarketplacePayout(ctx, assignmentID)
	if payout.Status != "settled" || payout.PaymentHash != hash || payout.Preimage != preimage {
		t.Fatalf("settled evidence not stored: %+v", payout)
	}
	escrow, err = store.GetMarketplaceEscrowAllocation(ctx, assignmentID)
	if err != nil || escrow.Status != "paid" || escrow.PayoutPaymentHash != hash || escrow.PaidAt != settledAt {
		t.Fatalf("paid escrow=%+v err=%v", escrow, err)
	}
	paidAt := escrow.PaidAt
	if err := store.MarkMarketplacePayoutSettled(ctx, assignmentID, hash, preimage, settledAt, settledAt); err != nil {
		t.Fatalf("idempotent settle payout: %v", err)
	}
	escrow, _ = store.GetMarketplaceEscrowAllocation(ctx, assignmentID)
	if escrow.PaidAt != paidAt || escrow.Status != "paid" {
		t.Fatalf("duplicate settlement consumed escrow twice: %+v", escrow)
	}
	audit, err := store.ListMarketplacePayoutAudit(ctx, assignmentID)
	if err != nil || len(audit) != 3 {
		t.Fatalf("audit=%+v err=%v", audit, err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE marketplace_payout_audit SET detail='tampered' WHERE assignment_id=?", assignmentID); err == nil {
		t.Fatal("immutable audit allowed update")
	}
	if _, err := store.DB().ExecContext(ctx, "DELETE FROM marketplace_payout_audit WHERE assignment_id=?", assignmentID); err == nil {
		t.Fatal("immutable audit allowed delete")
	}
}

func TestMarketplacePayoutDefinitiveFailure(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	reviewerSK := nostr.Generate()
	assignmentID, assignmentEventID, completionEventID, reviewEventID := seedMarketplacePayout(t, ctx, store, reviewerSK)
	if _, _, err := store.CompleteAssignmentAndAllocatePayout(ctx, assignmentEventID,
		nostr.GetPublicKey(reviewerSK).Hex(), completionEventID, reviewEventID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkMarketplacePayoutSubmitted(ctx, assignmentID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, store, `CREATE TRIGGER fail_payout_failed_audit BEFORE INSERT ON marketplace_payout_audit
		WHEN NEW.to_status = 'failed'
		BEGIN SELECT RAISE(ABORT, 'injected failed audit failure'); END`)
	if err := store.MarkMarketplacePayoutFailed(ctx, assignmentID, "wallet rejected payment", time.Now().Unix()); err == nil {
		t.Fatal("expected failed audit fault")
	}
	payout, _ := store.GetMarketplacePayout(ctx, assignmentID)
	if payout.Status != "submitted" {
		t.Fatalf("failed audit fault did not roll back payout: %q", payout.Status)
	}
	mustExec(t, store, "DROP TRIGGER fail_payout_failed_audit")
	if err := store.MarkMarketplacePayoutFailed(ctx, assignmentID, "wallet rejected payment", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	payout, _ = store.GetMarketplacePayout(ctx, assignmentID)
	if payout.Status != "failed" || payout.FailureReason == "" {
		t.Fatalf("failed payout evidence missing: %+v", payout)
	}
}

func seedMarketplacePayout(t *testing.T, ctx context.Context, store *Store, reviewerSK nostr.SecretKey) (int, string, string, string) {
	t.Helper()
	reviewer := nostr.GetPublicKey(reviewerSK).Hex()
	if err := store.UpsertReviewerProfile(ctx, ReviewerProfile{
		Pubkey: reviewer, Availability: "available", PayoutDestination: "lnbc1dbtest",
	}, "profile-event"); err != nil {
		t.Fatal(err)
	}
	assignmentEventID := "assignment-event"
	requester := strings.Repeat("ab", 32)
	seedAuthorizedReviewPayment(t, ctx, store, "patch-event", "repo-1", requester, 321)
	if err := store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID: "patch-event", RepoID: "repo-1", ReviewerPubkey: reviewer,
		RequesterPubkey: requester, Status: "accepted", PriceSats: 321,
		AssignmentEventID: assignmentEventID, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	assignment, err := store.GetAssignmentByEventID(ctx, assignmentEventID)
	if err != nil {
		t.Fatal(err)
	}
	review := nostr.Event{Kind: nostr.KindComment, CreatedAt: nostr.Now(), Content: "review"}
	if err := review.Sign(reviewerSK); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertReviewEvent(ctx, review, "patch-event", "repo-1"); err != nil {
		t.Fatal(err)
	}
	completion := nostr.Event{Kind: 25910, CreatedAt: nostr.Now(), Content: "completion"}
	if err := completion.Sign(reviewerSK); err != nil {
		t.Fatal(err)
	}
	return assignment.ID, assignmentEventID, completion.ID.Hex(), review.ID.Hex()
}

func mustExec(t *testing.T, store *Store, query string) {
	t.Helper()
	if _, err := store.DB().Exec(query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
