package marketplace

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/contextvm"
	"git.sharegap.net/cascadia/drydock/internal/db"

	"fiatjaf.com/nostr"
)

func TestRouterRejectsAcceptanceAndRejectionFromNonReviewer(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		method string
		params func(assignmentID string) any
		handle func(*Handler, context.Context, contextvm.Request) (any, *contextvm.Error)
	}{
		{
			name:   "acceptance",
			method: MethodAccept,
			params: func(id string) any { return ReviewAcceptance{AssignmentID: id} },
			handle: (*Handler).handleContextVMAcceptance,
		},
		{
			name:   "rejection",
			method: MethodReject,
			params: func(id string) any { return ReviewRejection{AssignmentID: id, Reason: "malicious"} },
			handle: (*Handler).handleContextVMRejection,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := mustOpenStore(t, ctx)
			registry := NewRegistry(store, slog.Default())
			router := NewRouter(RouterConfig{}, registry, store, &mockSigner{pubkey: testPubKey()}, nil, nil, slog.Default())
			handler := NewHandler(registry, router, store, slog.Default())
			reviewer := testPubKey().Hex()
			attacker := newIntegrationSigner()
			assignmentID := "assign-non-reviewer-" + tc.name
			seedAssignment(t, ctx, store, db.ReviewAssignment{
				PatchEventID:      "patch-" + tc.name,
				RepoID:            "repo-1",
				ReviewerPubkey:    reviewer,
				RequesterPubkey:   testPubKey().Hex(),
				Status:            "pending",
				AssignmentEventID: assignmentID,
				ExpiresAt:         time.Now().Add(time.Hour).Unix(),
			})

			// A genuinely signed intent from an attacker (not the assigned
			// reviewer) travels the live ContextVM path: verifySignedIntent
			// accepts the valid signature, and the registry must still reject
			// the reviewer-identity mismatch. This is the path production runs.
			req := contextVMRequest(t, attacker, tc.method, "intent-"+tc.name, tc.params(assignmentID), nil)
			_, rpcErr := tc.handle(handler, ctx, req)
			if rpcErr == nil || !strings.Contains(rpcErr.Message, "unauthorized reviewer") {
				t.Fatalf("expected unauthorized reviewer error, got %v", rpcErr)
			}
			assignment, err := store.GetAssignmentByEventID(ctx, assignmentID)
			if err != nil {
				t.Fatalf("GetAssignmentByEventID: %v", err)
			}
			if assignment.Status != "pending" {
				t.Fatalf("assignment status changed to %q, want pending", assignment.Status)
			}
		})
	}
}

func TestRouterRejectsNonPendingOrExpiredAssignmentTransition(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name          string
		status        string
		expiresAt     int64
		wantSubstring string
	}{
		{
			name:          "already accepted",
			status:        "accepted",
			expiresAt:     time.Now().Add(time.Hour).Unix(),
			wantSubstring: "not pending",
		},
		{
			name:          "expired pending",
			status:        "pending",
			expiresAt:     time.Now().Add(-time.Hour).Unix(),
			wantSubstring: "expired",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := mustOpenStore(t, ctx)
			registry := NewRegistry(store, slog.Default())
			router := NewRouter(RouterConfig{}, registry, store, &mockSigner{pubkey: testPubKey()}, nil, nil, slog.Default())
			handler := NewHandler(registry, router, store, slog.Default())
			reviewerSigner := newIntegrationSigner()
			reviewer := reviewerSigner.pubkey().Hex()
			assignmentID := "assign-transition-" + strings.ReplaceAll(tc.name, " ", "-")
			seedAssignment(t, ctx, store, db.ReviewAssignment{
				PatchEventID:      "patch-transition-" + tc.name,
				RepoID:            "repo-1",
				ReviewerPubkey:    reviewer,
				RequesterPubkey:   testPubKey().Hex(),
				Status:            tc.status,
				AssignmentEventID: assignmentID,
				ExpiresAt:         tc.expiresAt,
			})

			// Route each transition through the live ContextVM handler: a
			// valid signature from the genuine reviewer passes
			// verifySignedIntent, so the not-pending / expired guard must come
			// from the registry on the path production actually runs.
			for _, action := range []struct {
				name   string
				method string
				params any
				handle func(*Handler, context.Context, contextvm.Request) (any, *contextvm.Error)
			}{
				{
					name:   "acceptance",
					method: MethodAccept,
					params: ReviewAcceptance{AssignmentID: assignmentID},
					handle: (*Handler).handleContextVMAcceptance,
				},
				{
					name:   "rejection",
					method: MethodReject,
					params: ReviewRejection{AssignmentID: assignmentID, Reason: "too late"},
					handle: (*Handler).handleContextVMRejection,
				},
			} {
				req := contextVMRequest(t, reviewerSigner, action.method, "intent-"+action.name+"-"+tc.name, action.params, nil)
				_, rpcErr := action.handle(handler, ctx, req)
				if rpcErr == nil || !strings.Contains(rpcErr.Message, tc.wantSubstring) {
					t.Fatalf("%s: expected %q error, got %v", action.name, tc.wantSubstring, rpcErr)
				}
				assignment, err := store.GetAssignmentByEventID(ctx, assignmentID)
				if err != nil {
					t.Fatalf("%s: GetAssignmentByEventID: %v", action.name, err)
				}
				if assignment.Status != tc.status {
					t.Fatalf("%s: assignment status changed to %q, want %q", action.name, assignment.Status, tc.status)
				}
			}
		})
	}
}

func TestHandlerRejectsAssignmentIntentFromNonAuthority(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	authorSK := nostr.Generate()
	patchID := seedPatchEvent(t, ctx, store, authorSK, "repo-1")
	authority := testPubKey()
	attackerSK := nostr.Generate()

	registry := NewRegistry(store, slog.Default())
	router := NewRouter(RouterConfig{}, registry, store, &mockSigner{pubkey: authority}, nil, nil, slog.Default())
	handler := NewHandler(registry, router, store, slog.Default())

	assignment := ReviewAssignment{
		AssignmentID:   "assign-forged",
		PatchEventID:   patchID,
		RepoID:         "repo-1",
		ReviewerPubkey: testPubKey().Hex(),
		PriceSats:      0,
		Deadline:       time.Now().Add(time.Hour).Unix(),
	}
	event := signedMarketplaceEvent(t, attackerSK, KindReviewAssignment, assignment)

	err := handler.handleAssignment(ctx, event)
	if err == nil || !strings.Contains(err.Error(), "unauthorized assignment intent") {
		t.Fatalf("expected unauthorized assignment intent error, got %v", err)
	}
	if _, err := store.GetAssignmentByEventID(ctx, assignment.AssignmentID); err == nil {
		t.Fatalf("forged assignment was stored")
	}
}

func TestHandlerRejectsPaymentAssignmentMismatches(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	authority := testPubKey()
	registry := NewRegistry(store, slog.Default())
	router := NewRouter(RouterConfig{}, registry, store, &mockSigner{pubkey: authority}, nil, nil, slog.Default())
	handler := NewHandler(registry, router, store, slog.Default())
	seedAuthorizedMarketplacePayment(t, ctx, store, "patch-paid", "repo-paid", "requester-paid", 100)

	for _, tc := range []struct {
		name, repo, requester string
		price                 int64
		want                  string
	}{
		{name: "repo", repo: "wrong-repo", requester: "requester-paid", price: 100, want: "payment repo"},
		{name: "requester", repo: "repo-paid", requester: "wrong-requester", price: 100, want: "payment requester"},
		{name: "amount", repo: "repo-paid", requester: "requester-paid", price: 101, want: "exceeds settled funds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := handler.authorizeAssignmentIntent(ctx, authority.Hex(), "patch-paid", tc.repo, tc.requester, tc.price)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q mismatch rejection, got %v", tc.want, err)
			}
		})
	}
}

func TestHandlerRejectsUnauthorizedAndDuplicateFeedback(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	registry := NewRegistry(store, slog.Default())
	handler := NewHandler(registry, NewRouter(RouterConfig{}, registry, store, &mockSigner{pubkey: testPubKey()}, nil, nil, slog.Default()), store, slog.Default())

	requesterSK := nostr.Generate()
	requester := nostr.GetPublicKey(requesterSK).Hex()
	attackerSK := nostr.Generate()
	reviewer := testPubKey().Hex()
	assignmentID := "assign-feedback-auth"
	reviewEventID := "review-feedback-auth"
	seedAssignment(t, ctx, store, db.ReviewAssignment{
		PatchEventID:      "patch-feedback-auth",
		RepoID:            "repo-1",
		ReviewerPubkey:    reviewer,
		RequesterPubkey:   requester,
		Status:            "completed",
		AssignmentEventID: assignmentID,
		CompletionEventID: reviewEventID,
		ReviewEventID:     reviewEventID,
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	})
	assignment, err := store.GetAssignmentByEventID(ctx, assignmentID)
	if err != nil {
		t.Fatalf("GetAssignmentByEventID: %v", err)
	}

	unauthorized := feedbackNotificationRequest(t, attackerSK, MarketplaceFeedbackParams{ReviewEventID: reviewEventID, Rating: 5, Comment: "fake"})
	if err := handler.handleContextVMFeedback(ctx, unauthorized); err != nil {
		t.Fatalf("unauthorized feedback should be a handled rejection, got %v", err)
	}
	if count := feedbackCount(t, ctx, store, assignment.ID); count != 0 {
		t.Fatalf("unauthorized feedback was stored; count=%d", count)
	}

	authorized := feedbackNotificationRequest(t, requesterSK, MarketplaceFeedbackParams{ReviewEventID: reviewEventID, Rating: 5, Comment: "legit"})
	if err := handler.handleContextVMFeedback(ctx, authorized); err != nil {
		t.Fatalf("authorized feedback rejected: %v", err)
	}
	if count := feedbackCount(t, ctx, store, assignment.ID); count != 1 {
		t.Fatalf("authorized feedback count=%d, want 1", count)
	}

	duplicate := feedbackNotificationRequest(t, requesterSK, MarketplaceFeedbackParams{ReviewEventID: reviewEventID, Rating: 4, Comment: "again"})
	if err := handler.handleContextVMFeedback(ctx, duplicate); err != nil {
		t.Fatalf("duplicate feedback should be idempotent, got %v", err)
	}
	if count := feedbackCount(t, ctx, store, assignment.ID); count != 1 {
		t.Fatalf("duplicate feedback changed count to %d, want 1", count)
	}
	var storedRating int
	if err := store.DB().QueryRowContext(ctx, `SELECT rating FROM review_feedback WHERE assignment_id = ?`, assignment.ID).Scan(&storedRating); err != nil {
		t.Fatalf("read stored feedback rating: %v", err)
	}
	if storedRating != 5 {
		t.Fatalf("duplicate overwrote first rating: got %d, want 5", storedRating)
	}
}

func feedbackNotificationRequest(t *testing.T, sk nostr.SecretKey, params MarketplaceFeedbackParams) contextvm.Request {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal feedback params: %v", err)
	}
	msg := contextvm.Message{JSONRPC: "2.0", Method: MethodFeedback, Params: raw}
	content, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal feedback notification: %v", err)
	}
	event := nostr.Event{
		Kind: nostr.Kind(25910), CreatedAt: nostr.Now(),
		Tags: nostr.Tags{{"method", MethodFeedback}, {"e", params.ReviewEventID}}, Content: string(content),
	}
	if err := event.Sign(sk); err != nil {
		t.Fatalf("sign feedback notification: %v", err)
	}
	return contextvm.Request{Event: event, Sender: event.PubKey, Msg: msg}
}

func seedAssignment(t *testing.T, ctx context.Context, store *db.Store, assignment db.ReviewAssignment) {
	t.Helper()
	if assignment.Priority == 0 {
		assignment.Priority = 2
	}
	if err := store.CreateAssignment(ctx, assignment); err != nil {
		t.Fatalf("CreateAssignment: %v", err)
	}
}

func seedPatchEvent(t *testing.T, ctx context.Context, store *db.Store, authorSK nostr.SecretKey, repoID string) string {
	t.Helper()
	event := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + testPubKey().Hex() + ":" + repoID},
		},
		Content: "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -0,0 +1 @@\n+package main\n",
	}
	if err := event.Sign(authorSK); err != nil {
		t.Fatalf("sign patch event: %v", err)
	}
	if err := store.InsertPatchEvent(ctx, event); err != nil {
		t.Fatalf("InsertPatchEvent: %v", err)
	}
	return event.ID.Hex()
}

// TestContextVMAcceptanceRejectsForgedEnvelope proves the acceptance intent
// handler authenticates the signed envelope. The prior implementation forged an
// event (mutating Content/PubKey after signing) and never verified it, so a
// tampered envelope was recorded as a genuine acceptance. The completion path
// already verified; this closes that asymmetry.
func TestContextVMAcceptanceRejectsForgedEnvelope(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	registry := NewRegistry(store, slog.Default())
	router := NewRouter(RouterConfig{}, registry, store, nil, nil, nil, slog.Default())
	handler := NewHandler(registry, router, store, slog.Default())
	cv := contextvm.NewRouter()
	if err := handler.RegisterContextVMMethods(cv); err != nil {
		t.Fatalf("RegisterContextVMMethods: %v", err)
	}
	reviewer := newIntegrationSigner()

	if err := store.CreateAssignment(ctx, db.ReviewAssignment{
		PatchEventID:      "patch-forged",
		RepoID:            "repo-1",
		ReviewerPubkey:    reviewer.pubkey().Hex(),
		RequesterPubkey:   testPubKey().Hex(),
		Status:            "pending",
		AssignmentEventID: "forged-assignment",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("CreateAssignment: %v", err)
	}

	req := contextVMRequest(t, reviewer, MethodAccept, "accept-forged",
		ReviewAcceptance{AssignmentID: "forged-assignment", EstimatedTime: "2h"}, nil)
	// Tamper the envelope after signing: mutating Content invalidates the event
	// id and signature, exactly the forgery the old handler performed itself.
	req.Event.Content = string(req.Msg.Params)

	resp, err := cv.Handle(ctx, req)
	if err != nil {
		t.Fatalf("handle acceptance: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != contextvm.ErrorInvalidRequest {
		t.Fatalf("expected invalid-request rejection, got %+v", resp.Error)
	}
	assignment, err := store.GetAssignmentByEventID(ctx, "forged-assignment")
	if err != nil {
		t.Fatalf("GetAssignmentByEventID: %v", err)
	}
	if assignment.Status != "pending" {
		t.Fatalf("forged acceptance changed status to %q, want pending", assignment.Status)
	}
}

func signedMarketplaceEvent(t *testing.T, sk nostr.SecretKey, kind int, content any) nostr.Event {
	t.Helper()
	payload, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	event := nostr.Event{
		Kind:      nostr.Kind(kind),
		CreatedAt: nostr.Now(),
		Content:   string(payload),
	}
	if err := event.Sign(sk); err != nil {
		t.Fatalf("sign marketplace event: %v", err)
	}
	return event
}

func testPubKey() nostr.PubKey {
	return nostr.GetPublicKey(nostr.Generate())
}

func seedAuthorizedMarketplacePayment(t *testing.T, ctx context.Context, store *db.Store, patch, repo, author string, settled int64) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO review_payments (
		patch_event_id, repo_id, author_pubkey, status, access_kind, requested_mode,
		settled_amount_sats, created_at, updated_at
	) VALUES (?, ?, ?, 'authorized', 'cashu_review', 'review', ?, ?, ?)`, patch, repo, author, settled, now, now); err != nil {
		t.Fatalf("seed authorized marketplace payment: %v", err)
	}
}

func feedbackCount(t *testing.T, ctx context.Context, store *db.Store, assignmentID int) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM review_feedback WHERE assignment_id = ?`, assignmentID).Scan(&count); err != nil {
		t.Fatalf("count feedback: %v", err)
	}
	return count
}
