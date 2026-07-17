package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReserveReviewPaymentToken_DuplicateHashReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	first := ReviewPaymentRecord{
		PatchEventID:    "patch-1",
		RepoID:          "repo-1",
		AuthorPubkey:    "author-1",
		RequestedMode:   "review",
		TokenHash:       "hash123",
		MintURL:         "https://mint.example.com",
		TokenAmountSats: 100,
	}
	if err := store.ReserveReviewPaymentToken(ctx, first); err != nil {
		t.Fatalf("ReserveReviewPaymentToken first: %v", err)
	}

	second := first
	second.PatchEventID = "patch-2"
	if err := store.ReserveReviewPaymentToken(ctx, second); !errors.Is(err, ErrTokenHashAlreadyReserved) {
		t.Fatalf("expected ErrTokenHashAlreadyReserved for duplicate hash, got %v", err)
	}

	rec, err := store.GetReviewPaymentByTokenHash(ctx, "hash123")
	if err != nil {
		t.Fatalf("GetReviewPaymentByTokenHash: %v", err)
	}
	if rec.PatchEventID != "patch-1" || rec.Status != "pending" {
		t.Fatalf("expected original pending reservation, got patch=%q status=%q", rec.PatchEventID, rec.Status)
	}
}

func TestUpsertAndGetReviewPayment(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	record := ReviewPaymentRecord{
		PatchEventID:     "patch-123",
		RepoID:           "repo-1",
		AuthorPubkey:     "author-1",
		RequestedMode:    "review",
		TokenHash:        "hash123",
		MintURL:          "https://mint.example.com",
		TokenAmountSats:  500,
		InvoiceID:        "inv-123",
		InvoiceRequest:   "lnbc...",
		InvoiceExpiresAt: time.Now().Add(time.Hour).Unix(),
	}

	// Insert
	if err := store.UpsertPendingReviewPayment(ctx, record); err != nil {
		t.Fatalf("UpsertPendingReviewPayment: %v", err)
	}

	// Get
	got, err := store.GetReviewPayment(ctx, "patch-123")
	if err != nil {
		t.Fatalf("GetReviewPayment: %v", err)
	}

	if got.RepoID != "repo-1" {
		t.Errorf("expected RepoID 'repo-1', got %q", got.RepoID)
	}
	if got.Status != "pending" {
		t.Errorf("expected Status 'pending', got %q", got.Status)
	}
	if got.TokenAmountSats != 500 {
		t.Errorf("expected TokenAmountSats 500, got %d", got.TokenAmountSats)
	}
}

func TestGetReviewPayment_NotFound(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	_, err := store.GetReviewPayment(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent payment")
	}
}

func TestMarkReviewPaymentAuthorized_NotFoundReturnsError(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	if err := store.MarkReviewPaymentAuthorized(ctx, "missing-patch", "cashu_review"); err == nil {
		t.Fatal("expected error for nonexistent payment")
	}
}

func TestMarkReviewPaymentAuthorized(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Create pending payment
	err := store.UpsertPendingReviewPayment(ctx, ReviewPaymentRecord{
		PatchEventID:     "patch-1",
		RepoID:           "repo-1",
		AuthorPubkey:     "author-1",
		RequestedMode:    "review",
		TokenHash:        "hash123",
		MintURL:          "https://mint.example.com",
		InvoiceID:        "inv-1",
		InvoiceRequest:   "lnbc...",
		InvoiceExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("UpsertPendingReviewPayment: %v", err)
	}

	// Verify initial status is pending
	got, err := store.GetReviewPayment(ctx, "patch-1")
	if err != nil {
		t.Fatalf("GetReviewPayment: %v", err)
	}
	if got.Status != "pending" {
		t.Fatalf("expected initial status 'pending', got %q", got.Status)
	}

	if err := store.MarkReviewPaymentMeltSubmitted(ctx, "patch-1", "hash123", "quote-1", 100, 1); err != nil {
		t.Fatalf("MarkReviewPaymentMeltSubmitted: %v", err)
	}
	if err := store.MarkReviewPaymentTokenSpent(ctx, "patch-1"); err != nil {
		t.Fatalf("MarkReviewPaymentTokenSpent: %v", err)
	}

	// Mark authorized
	if err := store.MarkReviewPaymentAuthorized(ctx, "patch-1", "cashu_review"); err != nil {
		t.Fatalf("MarkReviewPaymentAuthorized: %v", err)
	}

	got, _ = store.GetReviewPayment(ctx, "patch-1")
	if got.Status != "authorized" {
		t.Errorf("expected status 'authorized', got %q", got.Status)
	}
	if got.AccessKind != "cashu_review" {
		t.Errorf("expected access_kind 'cashu_review', got %q", got.AccessKind)
	}
}

func TestDeletePendingReviewPayment(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Create payment
	store.UpsertPendingReviewPayment(ctx, ReviewPaymentRecord{
		PatchEventID:     "patch-1",
		RepoID:           "repo-1",
		AuthorPubkey:     "author-1",
		RequestedMode:    "review",
		InvoiceID:        "inv-1",
		InvoiceRequest:   "lnbc...",
		InvoiceExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	// Delete
	if err := store.DeletePendingReviewPayment(ctx, "patch-1"); err != nil {
		t.Fatalf("DeletePendingReviewPayment: %v", err)
	}

	// Should be gone
	_, err := store.GetReviewPayment(ctx, "patch-1")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestGetActiveSubscription(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	now := time.Now().Unix()

	// No subscription
	_, active, err := store.GetActiveSubscription(ctx, "author-1", "repo-1", now)
	if err != nil {
		t.Fatalf("GetActiveSubscription: %v", err)
	}
	if active {
		t.Error("expected no active subscription")
	}

	// Create subscription
	if err := store.UpsertSubscription(ctx, "author-1", "repo-1", "patch-1", "tokenhash", 1000, 30); err != nil {
		t.Fatalf("UpsertSubscription: %v", err)
	}

	// Should be active
	sub, active, err := store.GetActiveSubscription(ctx, "author-1", "repo-1", now)
	if err != nil {
		t.Fatalf("GetActiveSubscription (after create): %v", err)
	}
	if !active {
		t.Error("expected active subscription")
	}
	if sub.AuthorPubkey != "author-1" {
		t.Errorf("expected author 'author-1', got %q", sub.AuthorPubkey)
	}
}

func TestTryAuthorizeFreeReview(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	usageDay := "2026-04-24"

	// First review should succeed
	authorized, err := store.TryAuthorizeFreeReview(ctx, "patch-1", "repo-1", "author-1", 2, usageDay)
	if err != nil {
		t.Fatalf("TryAuthorizeFreeReview: %v", err)
	}
	if !authorized {
		t.Error("expected first review to be authorized")
	}

	// Second review should succeed
	authorized, err = store.TryAuthorizeFreeReview(ctx, "patch-2", "repo-1", "author-1", 2, usageDay)
	if err != nil {
		t.Fatalf("TryAuthorizeFreeReview (second): %v", err)
	}
	if !authorized {
		t.Error("expected second review to be authorized")
	}

	// Third review should fail (exceeded quota)
	authorized, err = store.TryAuthorizeFreeReview(ctx, "patch-3", "repo-1", "author-1", 2, usageDay)
	if err != nil {
		t.Fatalf("TryAuthorizeFreeReview (third): %v", err)
	}
	if authorized {
		t.Error("expected third review to be rejected (quota exceeded)")
	}
}

func TestTryAuthorizeFreeReview_NextDay(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Exhaust quota on day 1
	store.TryAuthorizeFreeReview(ctx, "patch-1", "repo-1", "author-1", 1, "2026-04-24")

	// Should fail on same day
	authorized, _ := store.TryAuthorizeFreeReview(ctx, "patch-2", "repo-1", "author-1", 1, "2026-04-24")
	if authorized {
		t.Error("expected rejection on same day")
	}

	// Should succeed on next day
	authorized, _ = store.TryAuthorizeFreeReview(ctx, "patch-3", "repo-1", "author-1", 1, "2026-04-25")
	if !authorized {
		t.Error("expected authorization on new day")
	}
}

func TestAuthorizeReviewFromSubscription(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Create subscription first
	if err := store.UpsertSubscription(ctx, "author-1", "repo-1", "orig-patch", "tokenhash", 1000, 30); err != nil {
		t.Fatalf("UpsertSubscription: %v", err)
	}

	// Authorize via subscription - this creates a record with NULL token_hash
	if err := store.AuthorizeReviewFromSubscription(ctx, "new-patch", "repo-1", "author-1"); err != nil {
		t.Fatalf("AuthorizeReviewFromSubscription: %v", err)
	}

	// Verify the authorization was created by checking the row directly
	// (GetReviewPayment has issues with NULL token_hash)
	var status, accessKind string
	err := store.db.QueryRowContext(ctx,
		`SELECT status, access_kind FROM review_payments WHERE patch_event_id = ?`,
		"new-patch",
	).Scan(&status, &accessKind)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if status != "authorized" {
		t.Errorf("expected status 'authorized', got %q", status)
	}
	if accessKind != "subscription" {
		t.Errorf("expected access_kind 'subscription', got %q", accessKind)
	}
}

func TestMarkReviewPaymentTokenSpent_NotFoundReturnsError(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	if err := store.MarkReviewPaymentTokenSpent(ctx, "missing-patch"); err == nil {
		t.Fatal("expected error for nonexistent payment")
	}
}

func TestMarkReviewPaymentTokenSpent(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Create pending payment
	err := store.UpsertPendingReviewPayment(ctx, ReviewPaymentRecord{
		PatchEventID:     "patch-1",
		RepoID:           "repo-1",
		AuthorPubkey:     "author-1",
		RequestedMode:    "review",
		TokenHash:        "hash123",
		MintURL:          "https://mint.example.com",
		InvoiceID:        "inv-1",
		InvoiceRequest:   "lnbc...",
		InvoiceExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("UpsertPendingReviewPayment: %v", err)
	}

	if err := store.MarkReviewPaymentMeltSubmitted(ctx, "patch-1", "hash123", "quote-1", 100, 1); err != nil {
		t.Fatalf("MarkReviewPaymentMeltSubmitted: %v", err)
	}
	// Mark token as spent
	if err := store.MarkReviewPaymentTokenSpent(ctx, "patch-1"); err != nil {
		t.Fatalf("MarkReviewPaymentTokenSpent: %v", err)
	}

	// Verify status is token_spent
	got, err := store.GetReviewPayment(ctx, "patch-1")
	if err != nil {
		t.Fatalf("GetReviewPayment: %v", err)
	}
	if got.Status != "token_spent" {
		t.Errorf("expected status 'token_spent', got %q", got.Status)
	}

	// Token should be considered used to prevent double-spending
	used, err := store.IsTokenHashUsed(ctx, "hash123")
	if err != nil {
		t.Fatalf("IsTokenHashUsed: %v", err)
	}
	if !used {
		t.Error("expected token_spent hash to be considered used")
	}

	// Should be able to complete authorization from token_spent state
	if err := store.MarkReviewPaymentAuthorized(ctx, "patch-1", "cashu_review"); err != nil {
		t.Fatalf("MarkReviewPaymentAuthorized from token_spent: %v", err)
	}

	got, _ = store.GetReviewPayment(ctx, "patch-1")
	if got.Status != "authorized" {
		t.Errorf("expected status 'authorized', got %q", got.Status)
	}
	if got.AccessKind != "cashu_review" {
		t.Errorf("expected access_kind 'cashu_review', got %q", got.AccessKind)
	}
}

func TestFinalizePaidReview_SubscriptionIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	rec := ReviewPaymentRecord{
		PatchEventID: "paid-sub", RepoID: "repo-1", AuthorPubkey: "author-1",
		RequestedMode: "subscription", TokenHash: "paid-token", MintURL: "https://mint.example.com",
		TokenAmountSats: 110, ExpectedAmountSats: 100, SubscriptionDays: 30,
		InvoiceID: "invoice", InvoiceRequest: "lnbc1", InvoiceAmountMSats: 100000,
		InvoiceExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
	if err := store.UpsertPendingReviewPayment(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReviewPaymentMeltSubmitted(ctx, rec.PatchEventID, rec.TokenHash, "quote", 100, 5); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReviewPaymentTokenSpent(ctx, rec.PatchEventID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_paid_authorize
		BEFORE UPDATE OF status ON review_payments
		WHEN NEW.patch_event_id = 'paid-sub' AND NEW.status = 'authorized'
		BEGIN SELECT RAISE(ABORT, 'injected authorization failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizePaidReview(ctx, rec.PatchEventID, rec.TokenHash); err == nil {
		t.Fatal("expected injected finalization failure")
	}
	if _, active, err := store.GetActiveSubscription(ctx, rec.AuthorPubkey, rec.RepoID, time.Now().Unix()); err != nil || active {
		t.Fatalf("subscription write must roll back with authorization: active=%v err=%v", active, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_paid_authorize`); err != nil {
		t.Fatal(err)
	}

	kind, err := store.FinalizePaidReview(ctx, rec.PatchEventID, rec.TokenHash)
	if err != nil || kind != "cashu_subscription" {
		t.Fatalf("FinalizePaidReview: kind=%q err=%v", kind, err)
	}
	first, active, err := store.GetActiveSubscription(ctx, rec.AuthorPubkey, rec.RepoID, time.Now().Unix())
	if err != nil || !active {
		t.Fatalf("GetActiveSubscription: active=%v err=%v", active, err)
	}
	if kind, err = store.FinalizePaidReview(ctx, rec.PatchEventID, rec.TokenHash); err != nil || kind != "cashu_subscription" {
		t.Fatalf("idempotent FinalizePaidReview: kind=%q err=%v", kind, err)
	}
	second, _, _ := store.GetActiveSubscription(ctx, rec.AuthorPubkey, rec.RepoID, time.Now().Unix())
	if second.ExpiresAt != first.ExpiresAt {
		t.Fatalf("idempotent recovery extended subscription twice: first=%d second=%d", first.ExpiresAt, second.ExpiresAt)
	}
}

func TestIsTokenHashUsed(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// New hash should not be used
	used, err := store.IsTokenHashUsed(ctx, "newhash123")
	if err != nil {
		t.Fatalf("IsTokenHashUsed: %v", err)
	}
	if used {
		t.Error("expected new hash to not be used")
	}

	// Create pending payment with the hash (not authorized yet)
	store.UpsertPendingReviewPayment(ctx, ReviewPaymentRecord{
		PatchEventID:     "patch-1",
		RepoID:           "repo-1",
		AuthorPubkey:     "author-1",
		RequestedMode:    "review",
		TokenHash:        "newhash123",
		InvoiceID:        "inv-1",
		InvoiceRequest:   "lnbc...",
		InvoiceExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	// Pending payment hash should NOT be considered used (only authorized)
	used, err = store.IsTokenHashUsed(ctx, "newhash123")
	if err != nil {
		t.Fatalf("IsTokenHashUsed (pending): %v", err)
	}
	if used {
		t.Error("expected pending payment hash to not be considered used")
	}

	// Mark as definitively paid and authorized.
	if err := store.MarkReviewPaymentMeltSubmitted(ctx, "patch-1", "newhash123", "quote-1", 100, 1); err != nil {
		t.Fatalf("MarkReviewPaymentMeltSubmitted: %v", err)
	}
	if err := store.MarkReviewPaymentTokenSpent(ctx, "patch-1"); err != nil {
		t.Fatalf("MarkReviewPaymentTokenSpent: %v", err)
	}
	store.MarkReviewPaymentAuthorized(ctx, "patch-1", "cashu_review")

	// Now should be used
	used, err = store.IsTokenHashUsed(ctx, "newhash123")
	if err != nil {
		t.Fatalf("IsTokenHashUsed (authorized): %v", err)
	}
	if !used {
		t.Error("expected hash to be used after authorization")
	}
}
