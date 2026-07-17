package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"drydock/internal/contextvm"
	"drydock/internal/db"
	"drydock/internal/payment"

	"fiatjaf.com/nostr"
)

type fakePayoutExecutor struct {
	mu           sync.Mutex
	submitCalls  int
	lookupCalls  int
	submitResult payment.PayoutEvidence
	submitErr    error
	lookupResult payment.PayoutEvidence
	lookupErr    error
}

func (f *fakePayoutExecutor) SubmitPayout(context.Context, string, int64, string) (payment.PayoutEvidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitCalls++
	return f.submitResult, f.submitErr
}

func (f *fakePayoutExecutor) ReconcilePayout(context.Context, string, int64) (payment.PayoutEvidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookupCalls++
	if f.submitCalls == 0 && f.lookupResult.Settled && f.lookupErr == nil {
		return payment.PayoutEvidence{}, nil
	}
	return f.lookupResult, f.lookupErr
}

func (f *fakePayoutExecutor) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.submitCalls, f.lookupCalls
}

func TestContextVMCompletionRoutesAuthenticatedMessage(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	reviewer := newIntegrationSigner()
	assignmentID, reviewID := seedAcceptedPaidCompletion(t, ctx, store, reviewer.sk)
	executor := settledPayoutExecutor()
	registry := NewRegistry(store, slog.Default())
	router := NewRouter(RouterConfig{}, registry, store, nil, nil, executor, slog.Default())
	handler := NewHandler(registry, router, store, slog.Default())
	cv := contextvm.NewRouter()
	if err := handler.RegisterContextVMMethods(cv); err != nil {
		t.Fatal(err)
	}
	req := contextVMRequest(t, reviewer, MethodComplete, "completion-rpc",
		ReviewCompletion{AssignmentID: assignmentID, ReviewEventID: reviewID}, nil)
	resp, err := cv.Handle(ctx, req)
	if err != nil || resp.Error != nil {
		t.Fatalf("completion response=%+v err=%v", resp, err)
	}
	assignment, _ := store.GetAssignmentByEventID(ctx, assignmentID)
	if assignment.Status != "completed" {
		t.Fatalf("contextvm completion status=%q", assignment.Status)
	}
}

func TestAuthenticatedCompletionRejectsWrongReviewer(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	reviewerSK := nostr.Generate()
	attackerSK := nostr.Generate()
	assignmentID, reviewID := seedAcceptedPaidCompletion(t, ctx, store, reviewerSK)
	executor := settledPayoutExecutor()
	router := NewRouter(RouterConfig{}, NewRegistry(store, slog.Default()), store, nil, nil, executor, slog.Default())

	event := signedMarketplaceEvent(t, attackerSK, 25910, ReviewCompletion{AssignmentID: assignmentID, ReviewEventID: reviewID})
	err := router.HandleCompletion(ctx, event)
	if err == nil || !strings.Contains(err.Error(), "unauthorized reviewer") {
		t.Fatalf("expected wrong reviewer rejection, got %v", err)
	}
	assignment, _ := store.GetAssignmentByEventID(ctx, assignmentID)
	if assignment.Status != "accepted" {
		t.Fatalf("assignment changed to %q", assignment.Status)
	}
	if _, err := store.GetMarketplacePayout(ctx, assignment.ID); err == nil {
		t.Fatal("unauthorized completion allocated payout")
	}
	submits, _ := executor.counts()
	if submits != 0 {
		t.Fatalf("unauthorized completion submitted %d payouts", submits)
	}
}

func TestCompletionIsIdempotentAndSettlesPayout(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	reviewerSK := nostr.Generate()
	assignmentID, reviewID := seedAcceptedPaidCompletion(t, ctx, store, reviewerSK)
	executor := settledPayoutExecutor()
	router := NewRouter(RouterConfig{}, NewRegistry(store, slog.Default()), store, nil, nil, executor, slog.Default())
	event := signedMarketplaceEvent(t, reviewerSK, 25910, ReviewCompletion{AssignmentID: assignmentID, ReviewEventID: reviewID})

	if err := router.HandleCompletion(ctx, event); err != nil {
		t.Fatalf("first completion: %v", err)
	}
	if err := router.HandleCompletion(ctx, event); err != nil {
		t.Fatalf("duplicate completion: %v", err)
	}
	assignment, _ := store.GetAssignmentByEventID(ctx, assignmentID)
	if assignment.Status != "completed" || assignment.CompletionEventID != event.ID.Hex() || assignment.ReviewEventID != reviewID {
		t.Fatalf("completion correlation not stored: %+v", assignment)
	}
	payout, err := store.GetMarketplacePayout(ctx, assignment.ID)
	if err != nil || payout.Status != "settled" || payout.AmountSats != 250 {
		t.Fatalf("payout=%+v err=%v", payout, err)
	}
	submits, _ := executor.counts()
	if submits != 1 {
		t.Fatalf("submit calls=%d, want 1", submits)
	}
	audit, err := store.ListMarketplacePayoutAudit(ctx, assignment.ID)
	if err != nil || len(audit) != 3 {
		t.Fatalf("audit=%+v err=%v", audit, err)
	}
}

func TestAmbiguousPayoutStaysSubmittedForReconciliation(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	reviewerSK := nostr.Generate()
	assignmentID, reviewID := seedAcceptedPaidCompletion(t, ctx, store, reviewerSK)
	executor := &fakePayoutExecutor{
		submitErr: &payment.PayoutSubmissionError{MayHaveSubmitted: true, Err: errors.New("wallet timeout")},
		lookupErr: errors.New("wallet has not indexed payment yet"),
	}
	router := NewRouter(RouterConfig{}, NewRegistry(store, slog.Default()), store, nil, nil, executor, slog.Default())
	event := signedMarketplaceEvent(t, reviewerSK, 25910, ReviewCompletion{AssignmentID: assignmentID, ReviewEventID: reviewID})

	if err := router.HandleCompletion(ctx, event); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous error, got %v", err)
	}
	assignment, _ := store.GetAssignmentByEventID(ctx, assignmentID)
	payout, _ := store.GetMarketplacePayout(ctx, assignment.ID)
	if payout.Status != "submitted" {
		t.Fatalf("ambiguous payout status=%q, want submitted", payout.Status)
	}
	if err := router.HandleCompletion(ctx, event); err == nil || !strings.Contains(err.Error(), "inconclusive") {
		t.Fatalf("expected inconclusive reconciliation, got %v", err)
	}
	submits, lookups := executor.counts()
	if submits != 1 || lookups != 1 {
		t.Fatalf("calls submit=%d lookup=%d, want 1/1", submits, lookups)
	}
	payout, _ = store.GetMarketplacePayout(ctx, assignment.ID)
	if payout.Status != "submitted" {
		t.Fatalf("unsettled reconciliation changed status to %q", payout.Status)
	}
}

func TestConcurrentCompletionNeverDoublePays(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	reviewerSK := nostr.Generate()
	assignmentID, reviewID := seedAcceptedPaidCompletion(t, ctx, store, reviewerSK)
	executor := settledPayoutExecutor()
	router := NewRouter(RouterConfig{}, NewRegistry(store, slog.Default()), store, nil, nil, executor, slog.Default())
	event := signedMarketplaceEvent(t, reviewerSK, 25910, ReviewCompletion{AssignmentID: assignmentID, ReviewEventID: reviewID})

	const workers = 12
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- router.HandleCompletion(ctx, event)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent completion: %v", err)
		}
	}
	submits, _ := executor.counts()
	if submits != 1 {
		t.Fatalf("external payout submitted %d times, want once", submits)
	}
}

func settledPayoutExecutor() *fakePayoutExecutor {
	preimage := strings.Repeat("22", 32)
	preimageBytes, _ := hex.DecodeString(preimage)
	hash := sha256.Sum256(preimageBytes)
	evidence := payment.PayoutEvidence{
		PaymentHash: hex.EncodeToString(hash[:]),
		Preimage:    preimage,
		SettledAt:   time.Now().Unix(),
		Settled:     true,
	}
	return &fakePayoutExecutor{submitResult: evidence, lookupResult: evidence}
}

func seedAcceptedPaidCompletion(t *testing.T, ctx context.Context, store *db.Store, reviewerSK nostr.SecretKey) (string, string) {
	t.Helper()
	reviewer := nostr.GetPublicKey(reviewerSK).Hex()
	if err := store.UpsertReviewerProfile(ctx, db.ReviewerProfile{
		Pubkey: reviewer, Availability: "available", PayoutDestination: "lnbc1reviewerpayout",
	}, "profile-"+reviewer[:8]); err != nil {
		t.Fatalf("UpsertReviewerProfile: %v", err)
	}
	assignmentID := "assignment-" + reviewer[:12]
	patchID := "patch-" + reviewer[:12]
	repoID := "repo-1"
	if err := store.CreateAssignment(ctx, db.ReviewAssignment{
		PatchEventID: patchID, RepoID: repoID, ReviewerPubkey: reviewer,
		RequesterPubkey: testPubKey().Hex(), Status: "accepted", PriceSats: 250,
		AssignmentEventID: assignmentID, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("CreateAssignment: %v", err)
	}
	review := nostr.Event{
		Kind: nostr.KindComment, CreatedAt: nostr.Now(),
		Tags: nostr.Tags{{"e", patchID}}, Content: "published marketplace review",
	}
	if err := review.Sign(reviewerSK); err != nil {
		t.Fatalf("sign review: %v", err)
	}
	if err := store.InsertReviewEvent(ctx, review, patchID, repoID); err != nil {
		t.Fatalf("InsertReviewEvent: %v", err)
	}
	return assignmentID, review.ID.Hex()
}
