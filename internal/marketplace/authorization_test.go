package marketplace

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"drydock/internal/db"

	"fiatjaf.com/nostr"
)

func TestRouterRejectsAcceptanceAndRejectionFromNonReviewer(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		handle func(context.Context, *Router, string, nostr.SecretKey) error
	}{
		{
			name: "acceptance",
			handle: func(ctx context.Context, router *Router, assignmentID string, attackerSK nostr.SecretKey) error {
				return router.HandleAcceptance(ctx, signedMarketplaceEvent(t, attackerSK, KindReviewAcceptance, ReviewAcceptance{AssignmentID: assignmentID}))
			},
		},
		{
			name: "rejection",
			handle: func(ctx context.Context, router *Router, assignmentID string, attackerSK nostr.SecretKey) error {
				return router.HandleRejection(ctx, signedMarketplaceEvent(t, attackerSK, KindReviewRejection, ReviewRejection{AssignmentID: assignmentID, Reason: "malicious"}))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := mustOpenStore(t, ctx)
			registry := NewRegistry(store, slog.Default())
			router := NewRouter(RouterConfig{}, registry, store, &mockSigner{pubkey: testPubKey()}, &mockPublisher{}, slog.Default())
			reviewer := testPubKey().Hex()
			attackerSK := nostr.Generate()
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

			err := tc.handle(ctx, router, assignmentID, attackerSK)
			if err == nil || !strings.Contains(err.Error(), "unauthorized reviewer") {
				t.Fatalf("expected unauthorized reviewer error, got %v", err)
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
			router := NewRouter(RouterConfig{}, registry, store, &mockSigner{pubkey: testPubKey()}, &mockPublisher{}, slog.Default())
			reviewerSK := nostr.Generate()
			reviewer := nostr.GetPublicKey(reviewerSK).Hex()
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

			for _, action := range []struct {
				name   string
				handle func(context.Context, nostr.Event) error
				event  nostr.Event
			}{
				{
					name:   "acceptance",
					handle: router.HandleAcceptance,
					event:  signedMarketplaceEvent(t, reviewerSK, KindReviewAcceptance, ReviewAcceptance{AssignmentID: assignmentID}),
				},
				{
					name:   "rejection",
					handle: router.HandleRejection,
					event:  signedMarketplaceEvent(t, reviewerSK, KindReviewRejection, ReviewRejection{AssignmentID: assignmentID, Reason: "too late"}),
				},
			} {
				err := action.handle(ctx, action.event)
				if err == nil || !strings.Contains(err.Error(), tc.wantSubstring) {
					t.Fatalf("%s: expected %q error, got %v", action.name, tc.wantSubstring, err)
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
	router := NewRouter(RouterConfig{}, registry, store, &mockSigner{pubkey: authority}, &mockPublisher{}, slog.Default())
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

func TestHandlerRejectsUnauthorizedAndDuplicateFeedback(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	registry := NewRegistry(store, slog.Default())
	handler := NewHandler(registry, NewRouter(RouterConfig{}, registry, store, &mockSigner{pubkey: testPubKey()}, &mockPublisher{}, slog.Default()), store, slog.Default())

	requesterSK := nostr.Generate()
	requester := nostr.GetPublicKey(requesterSK).Hex()
	attackerSK := nostr.Generate()
	reviewer := testPubKey().Hex()
	assignmentID := "assign-feedback-auth"
	seedAssignment(t, ctx, store, db.ReviewAssignment{
		PatchEventID:      "patch-feedback-auth",
		RepoID:            "repo-1",
		ReviewerPubkey:    reviewer,
		RequesterPubkey:   requester,
		Status:            "completed",
		AssignmentEventID: assignmentID,
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	})
	assignment, err := store.GetAssignmentByEventID(ctx, assignmentID)
	if err != nil {
		t.Fatalf("GetAssignmentByEventID: %v", err)
	}

	unauthorized := signedMarketplaceEvent(t, attackerSK, KindReviewFeedback, ReviewFeedback{AssignmentID: assignment.ID, Rating: 5, Comment: "fake"})
	if err := handler.handleFeedback(ctx, unauthorized); err == nil || !strings.Contains(err.Error(), "unauthorized feedback rater") {
		t.Fatalf("expected unauthorized feedback error, got %v", err)
	}
	if count := feedbackCount(t, ctx, store, assignment.ID); count != 0 {
		t.Fatalf("unauthorized feedback was stored; count=%d", count)
	}

	authorized := signedMarketplaceEvent(t, requesterSK, KindReviewFeedback, ReviewFeedback{AssignmentID: assignment.ID, Rating: 5, Comment: "legit"})
	if err := handler.handleFeedback(ctx, authorized); err != nil {
		t.Fatalf("authorized feedback rejected: %v", err)
	}
	if count := feedbackCount(t, ctx, store, assignment.ID); count != 1 {
		t.Fatalf("authorized feedback count=%d, want 1", count)
	}

	duplicate := signedMarketplaceEvent(t, requesterSK, KindReviewFeedback, ReviewFeedback{AssignmentID: assignment.ID, Rating: 4, Comment: "again"})
	if err := handler.handleFeedback(ctx, duplicate); err == nil || !strings.Contains(err.Error(), "duplicate feedback") {
		t.Fatalf("expected duplicate feedback error, got %v", err)
	}
	if count := feedbackCount(t, ctx, store, assignment.ID); count != 1 {
		t.Fatalf("duplicate feedback changed count to %d, want 1", count)
	}
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

func feedbackCount(t *testing.T, ctx context.Context, store *db.Store, assignmentID int) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM review_feedback WHERE assignment_id = ?`, assignmentID).Scan(&count); err != nil {
		t.Fatalf("count feedback: %v", err)
	}
	return count
}
