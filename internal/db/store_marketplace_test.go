package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestUpsertAndGetReviewerProfile(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	profile := ReviewerProfile{
		Pubkey:         "pubkey123",
		DisplayName:    "Test Reviewer",
		Languages:      []string{"go", "rust"},
		Domains:        []string{"backend", "security"},
		Availability:   "available",
		PricePerReview: 1000,
		MaxConcurrent:  3,
	}

	// Insert profile
	if err := store.UpsertReviewerProfile(ctx, profile, "event-1"); err != nil {
		t.Fatalf("UpsertReviewerProfile: %v", err)
	}

	// Get profile back
	got, err := store.GetReviewerProfile(ctx, "pubkey123")
	if err != nil {
		t.Fatalf("GetReviewerProfile: %v", err)
	}

	if got.DisplayName != "Test Reviewer" {
		t.Errorf("expected DisplayName 'Test Reviewer', got %q", got.DisplayName)
	}
	if got.Availability != "available" {
		t.Errorf("expected Availability 'available', got %q", got.Availability)
	}
	if got.PricePerReview != 1000 {
		t.Errorf("expected PricePerReview 1000, got %d", got.PricePerReview)
	}
	if len(got.Languages) != 2 {
		t.Errorf("expected 2 languages, got %d", len(got.Languages))
	}

	// Update profile
	profile.DisplayName = "Updated Reviewer"
	profile.Availability = "limited"
	if err := store.UpsertReviewerProfile(ctx, profile, "event-2"); err != nil {
		t.Fatalf("UpsertReviewerProfile (update): %v", err)
	}

	got, _ = store.GetReviewerProfile(ctx, "pubkey123")
	if got.DisplayName != "Updated Reviewer" {
		t.Errorf("expected DisplayName 'Updated Reviewer', got %q", got.DisplayName)
	}
	if got.Availability != "limited" {
		t.Errorf("expected Availability 'limited', got %q", got.Availability)
	}
}

func TestGetReviewerProfile_NotFound(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	_, err := store.GetReviewerProfile(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent profile")
	}
}

func TestListAvailableReviewers(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Insert available reviewer
	if err := store.UpsertReviewerProfile(ctx, ReviewerProfile{
		Pubkey:        "reviewer1",
		Languages:     []string{"go"},
		Domains:       []string{"backend"},
		Availability:  "available",
		MaxConcurrent: 5,
	}, "evt-1"); err != nil {
		t.Fatal(err)
	}

	// Insert unavailable reviewer
	if err := store.UpsertReviewerProfile(ctx, ReviewerProfile{
		Pubkey:        "reviewer2",
		Languages:     []string{"go"},
		Domains:       []string{"backend"},
		Availability:  "unavailable",
		MaxConcurrent: 5,
	}, "evt-2"); err != nil {
		t.Fatal(err)
	}

	// Insert limited reviewer (still listed)
	if err := store.UpsertReviewerProfile(ctx, ReviewerProfile{
		Pubkey:        "reviewer3",
		Languages:     []string{"rust"},
		Domains:       []string{"security"},
		Availability:  "limited",
		MaxConcurrent: 3,
	}, "evt-3"); err != nil {
		t.Fatal(err)
	}

	// List all available (excludes unavailable)
	reviewers, err := store.ListAvailableReviewers(ctx)
	if err != nil {
		t.Fatalf("ListAvailableReviewers: %v", err)
	}
	if len(reviewers) != 2 {
		t.Errorf("expected 2 available/limited reviewers, got %d", len(reviewers))
	}
}

func TestUpdateReviewerAvailability(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Insert profile
	if err := store.UpsertReviewerProfile(ctx, ReviewerProfile{
		Pubkey:       "reviewer1",
		Availability: "available",
	}, "evt-1"); err != nil {
		t.Fatal(err)
	}

	// Update availability
	if err := store.UpdateReviewerAvailability(ctx, "reviewer1", "limited"); err != nil {
		t.Fatalf("UpdateReviewerAvailability: %v", err)
	}

	profile, _ := store.GetReviewerProfile(ctx, "reviewer1")
	if profile.Availability != "limited" {
		t.Errorf("expected availability 'limited', got %q", profile.Availability)
	}
}

func TestCreateAndGetAssignment(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	seedAuthorizedReviewPayment(t, ctx, store, "patch-123", "repo-1", "requester-1", 500)
	assignment := ReviewAssignment{
		PatchEventID:      "patch-123",
		RepoID:            "repo-1",
		ReviewerPubkey:    "reviewer-1",
		RequesterPubkey:   "requester-1",
		Status:            "pending",
		Priority:          2,
		PriceSats:         500,
		AssignmentEventID: "assign-evt-1",
		ExpiresAt:         time.Now().Add(24 * time.Hour).Unix(),
	}

	err := store.CreateAssignment(ctx, assignment)
	if err != nil {
		t.Fatalf("CreateAssignment: %v", err)
	}

	// Get by event ID
	got, err := store.GetAssignmentByEventID(ctx, "assign-evt-1")
	if err != nil {
		t.Fatalf("GetAssignmentByEventID: %v", err)
	}
	if got.PatchEventID != "patch-123" {
		t.Errorf("expected PatchEventID 'patch-123', got %q", got.PatchEventID)
	}
	if got.Status != "pending" {
		t.Errorf("expected Status 'pending', got %q", got.Status)
	}

	// Get by ID
	gotByID, err := store.GetAssignmentByID(ctx, got.ID)
	if err != nil {
		t.Fatalf("GetAssignmentByID: %v", err)
	}
	if gotByID.AssignmentEventID != "assign-evt-1" {
		t.Errorf("expected AssignmentEventID 'assign-evt-1', got %q", gotByID.AssignmentEventID)
	}
}

func TestPaidAssignmentRejectsOverAllocationAndMismatches(t *testing.T) {
	ctx := context.Background()

	t.Run("over allocation", func(t *testing.T) {
		store := mustOpenStore(t, ctx)
		seedAuthorizedReviewPayment(t, ctx, store, "patch-funded", "repo-funded", "requester-funded", 1000)
		first := ReviewAssignment{PatchEventID: "patch-funded", RepoID: "repo-funded", ReviewerPubkey: "reviewer-1",
			RequesterPubkey: "requester-funded", Status: "pending", Priority: 2, PriceSats: 700,
			AssignmentEventID: "assignment-funded-1", ExpiresAt: time.Now().Add(time.Hour).Unix()}
		if err := store.CreateAssignment(ctx, first); err != nil {
			t.Fatalf("first allocation: %v", err)
		}
		second := first
		second.ReviewerPubkey = "reviewer-2"
		second.AssignmentEventID = "assignment-funded-2"
		second.PriceSats = 400
		if err := store.CreateAssignment(ctx, second); !errors.Is(err, ErrAssignmentEscrow) {
			t.Fatalf("expected escrow over-allocation rejection, got %v", err)
		}
		var assignments int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_assignments WHERE patch_event_id=?`, first.PatchEventID).Scan(&assignments); err != nil {
			t.Fatal(err)
		}
		if assignments != 1 {
			t.Fatalf("over-allocated assignment persisted; count=%d", assignments)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE review_payments SET settled_amount_sats=2000 WHERE patch_event_id=?`, first.PatchEventID); err == nil {
			t.Fatal("escrow-backed payment amount was mutable")
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE review_assignments SET price_sats=1 WHERE assignment_event_id=?`, first.AssignmentEventID); err == nil {
			t.Fatal("escrow-backed assignment price was mutable")
		}
	})

	for _, tc := range []struct {
		name, repo, requester string
	}{
		{name: "repo mismatch", repo: "wrong-repo", requester: "requester-match"},
		{name: "requester mismatch", repo: "repo-match", requester: "wrong-requester"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := mustOpenStore(t, ctx)
			seedAuthorizedReviewPayment(t, ctx, store, "patch-match", "repo-match", "requester-match", 100)
			err := store.CreateAssignment(ctx, ReviewAssignment{PatchEventID: "patch-match", RepoID: tc.repo,
				ReviewerPubkey: "reviewer-match", RequesterPubkey: tc.requester, Status: "pending", Priority: 2,
				PriceSats: 100, AssignmentEventID: "assignment-" + tc.name, ExpiresAt: time.Now().Add(time.Hour).Unix()})
			if !errors.Is(err, ErrAssignmentEscrow) {
				t.Fatalf("expected mismatch rejection, got %v", err)
			}
		})
	}
}

func TestConcurrentPaidAssignmentsCannotDoubleClaimPayment(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	seedAuthorizedReviewPayment(t, ctx, store, "patch-race", "repo-race", "requester-race", 100)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- store.CreateAssignment(ctx, ReviewAssignment{PatchEventID: "patch-race", RepoID: "repo-race",
				ReviewerPubkey: fmt.Sprintf("reviewer-race-%d", i), RequesterPubkey: "requester-race",
				Status: "pending", Priority: 2, PriceSats: 100, AssignmentEventID: fmt.Sprintf("assignment-race-%d", i),
				ExpiresAt: time.Now().Add(time.Hour).Unix()})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	var succeeded, rejected int
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrAssignmentEscrow):
			rejected++
		default:
			t.Fatalf("unexpected concurrent assignment error: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent results succeeded=%d rejected=%d, want 1/1", succeeded, rejected)
	}
	var allocations, allocated int64
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(amount_sats),0)
		FROM marketplace_escrow_allocations WHERE payment_patch_event_id='patch-race'`).Scan(&allocations, &allocated); err != nil {
		t.Fatal(err)
	}
	if allocations != 1 || allocated != 100 {
		t.Fatalf("allocations=%d amount=%d, want 1/100", allocations, allocated)
	}
}

func TestUpsertAssignmentReceiptIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	assignment := ReviewAssignment{
		PatchEventID:      "patch-upsert",
		RepoID:            "repo-upsert",
		ReviewerPubkey:    "reviewer-upsert",
		RequesterPubkey:   "requester-real",
		Status:            "pending",
		Priority:          2,
		AssignmentEventID: "assignment-upsert",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	}
	if err := store.CreateAssignment(ctx, assignment); err != nil {
		t.Fatalf("CreateAssignment: %v", err)
	}
	if err := store.UpsertAssignmentReceipt(ctx, assignment); err != nil {
		t.Fatalf("UpsertAssignmentReceipt: %v", err)
	}

	got, err := store.GetAssignmentByEventID(ctx, assignment.AssignmentEventID)
	if err != nil {
		t.Fatalf("GetAssignmentByEventID: %v", err)
	}
	if got.RequesterPubkey != assignment.RequesterPubkey {
		t.Fatalf("requester pubkey = %q, want %q", got.RequesterPubkey, assignment.RequesterPubkey)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_assignments WHERE assignment_event_id=?`, assignment.AssignmentEventID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("assignment count = %d, want 1", count)
	}
}

func TestRecordFeedbackConcurrentDuplicateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	if err := store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID:      "patch-feedback-race",
		RepoID:            "repo-feedback-race",
		ReviewerPubkey:    "reviewer-feedback-race",
		RequesterPubkey:   "rater-feedback-race",
		Status:            "completed",
		AssignmentEventID: "assignment-feedback-race",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	assignment, err := store.GetAssignmentByEventID(ctx, "assignment-feedback-race")
	if err != nil {
		t.Fatal(err)
	}

	const attempts = 8
	var wg sync.WaitGroup
	errs := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- store.RecordFeedback(ctx, ReviewFeedback{
				AssignmentID: assignment.ID, ReviewerPubkey: assignment.ReviewerPubkey,
				RaterPubkey: assignment.RequesterPubkey, Rating: 5,
				EventID: fmt.Sprintf("feedback-race-%d", i),
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("RecordFeedback: %v", err)
		}
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_feedback WHERE assignment_id=? AND rater_pubkey=?`, assignment.ID, assignment.RequesterPubkey).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("feedback count = %d, want 1", count)
	}
}

func TestUpdateAssignmentStatus(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID:      "patch-1",
		RepoID:            "repo-1",
		ReviewerPubkey:    "reviewer-1",
		RequesterPubkey:   "requester-1",
		Status:            "pending",
		AssignmentEventID: "evt-1",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	})

	got, _ := store.GetAssignmentByEventID(ctx, "evt-1")

	// Accept the assignment
	if err := store.UpdateAssignmentStatus(ctx, got.ID, "accepted", "accept-evt-1"); err != nil {
		t.Fatalf("UpdateAssignmentStatus: %v", err)
	}

	got, _ = store.GetAssignmentByID(ctx, got.ID)
	if got.Status != "accepted" {
		t.Errorf("expected status 'accepted', got %q", got.Status)
	}
	if got.AcceptanceEventID != "accept-evt-1" {
		t.Errorf("expected AcceptanceEventID 'accept-evt-1', got %q", got.AcceptanceEventID)
	}
}

func TestListPendingAssignments(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Insert reviewer profile
	store.UpsertReviewerProfile(ctx, ReviewerProfile{
		Pubkey:       "reviewer-1",
		Availability: "available",
	}, "evt-1")

	// Create pending assignment
	store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID:      "patch-1",
		RepoID:            "repo-1",
		ReviewerPubkey:    "reviewer-1",
		RequesterPubkey:   "requester-1",
		Status:            "pending",
		AssignmentEventID: "assign-1",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	})

	// Create accepted assignment (not pending)
	store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID:      "patch-2",
		RepoID:            "repo-1",
		ReviewerPubkey:    "reviewer-1",
		RequesterPubkey:   "requester-1",
		Status:            "accepted",
		AssignmentEventID: "assign-2",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	})

	pending, err := store.ListPendingAssignments(ctx, "reviewer-1")
	if err != nil {
		t.Fatalf("ListPendingAssignments: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("expected 1 pending assignment, got %d", len(pending))
	}
}

func TestExpireStaleAssignments(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	now := time.Now().Unix()

	// Create expired assignment
	store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID:      "patch-1",
		RepoID:            "repo-1",
		ReviewerPubkey:    "reviewer-1",
		RequesterPubkey:   "requester-1",
		Status:            "pending",
		AssignmentEventID: "assign-1",
		ExpiresAt:         now - 3600, // expired 1 hour ago
	})

	// Create non-expired assignment
	store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID:      "patch-2",
		RepoID:            "repo-1",
		ReviewerPubkey:    "reviewer-1",
		RequesterPubkey:   "requester-1",
		Status:            "pending",
		AssignmentEventID: "assign-2",
		ExpiresAt:         now + 3600, // expires in 1 hour
	})

	expired, err := store.ExpireStaleAssignments(ctx)
	if err != nil {
		t.Fatalf("ExpireStaleAssignments: %v", err)
	}
	if expired != 1 {
		t.Errorf("expected 1 expired, got %d", expired)
	}

	// Verify assignment was expired
	assignment, _ := store.GetAssignmentByEventID(ctx, "assign-1")
	if assignment.Status != "expired" {
		t.Errorf("expected status 'expired', got %q", assignment.Status)
	}
}

func TestUpsertAndGetReviewerReputation(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	rep := ReputationScore{
		Pubkey:         "reviewer-1",
		OverallScore:   0.85,
		TotalReviews:   10,
		AcceptedCount:  8,
		RejectedCount:  2,
		AverageRating:  4.2,
		AcceptanceRate: 0.8,
	}

	if err := store.UpsertReviewerReputation(ctx, rep); err != nil {
		t.Fatalf("UpsertReviewerReputation: %v", err)
	}

	got, err := store.GetReviewerReputation(ctx, "reviewer-1")
	if err != nil {
		t.Fatalf("GetReviewerReputation: %v", err)
	}

	if got.OverallScore != 0.85 {
		t.Errorf("expected OverallScore 0.85, got %f", got.OverallScore)
	}
	if got.TotalReviews != 10 {
		t.Errorf("expected TotalReviews 10, got %d", got.TotalReviews)
	}
}

func TestCountActiveAssignments(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Create active (accepted) assignments
	store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID:      "patch-1",
		RepoID:            "repo-1",
		ReviewerPubkey:    "reviewer-1",
		RequesterPubkey:   "requester-1",
		Status:            "accepted",
		AssignmentEventID: "assign-1",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	})

	store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID:      "patch-2",
		RepoID:            "repo-1",
		ReviewerPubkey:    "reviewer-1",
		RequesterPubkey:   "requester-1",
		Status:            "accepted",
		AssignmentEventID: "assign-2",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	})

	// Completed assignment (not counted as active)
	store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID:      "patch-3",
		RepoID:            "repo-1",
		ReviewerPubkey:    "reviewer-1",
		RequesterPubkey:   "requester-1",
		Status:            "completed",
		AssignmentEventID: "assign-3",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	})

	count, err := store.CountActiveAssignments(ctx, "reviewer-1")
	if err != nil {
		t.Fatalf("CountActiveAssignments: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 active assignments, got %d", count)
	}
}

func TestRecordFeedback(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Create assignment first
	store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID:      "patch-1",
		RepoID:            "repo-1",
		ReviewerPubkey:    "reviewer-1",
		RequesterPubkey:   "requester-1",
		Status:            "completed",
		AssignmentEventID: "assign-1",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	})

	a, _ := store.GetAssignmentByEventID(ctx, "assign-1")

	feedback := ReviewFeedback{
		AssignmentID:   a.ID,
		ReviewerPubkey: "reviewer-1",
		RaterPubkey:    "requester-1",
		Rating:         5,
		Comment:        "Great review!",
		EventID:        "feedback-evt-1",
	}

	if err := store.RecordFeedback(ctx, feedback); err != nil {
		t.Fatalf("RecordFeedback: %v", err)
	}
}

func seedAuthorizedReviewPayment(t *testing.T, ctx context.Context, store *Store, patch, repo, author string, settled int64) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO review_payments (
		patch_event_id, repo_id, author_pubkey, status, access_kind, requested_mode,
		settled_amount_sats, created_at, updated_at
	) VALUES (?, ?, ?, 'authorized', 'cashu_review', 'review', ?, ?, ?)`, patch, repo, author, settled, now, now); err != nil {
		t.Fatalf("seed authorized review payment: %v", err)
	}
}

func TestCountAvailableReviewers(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Insert reviewers
	store.UpsertReviewerProfile(ctx, ReviewerProfile{
		Pubkey:       "r1",
		Availability: "available",
	}, "evt-1")

	store.UpsertReviewerProfile(ctx, ReviewerProfile{
		Pubkey:       "r2",
		Availability: "available",
	}, "evt-2")

	store.UpsertReviewerProfile(ctx, ReviewerProfile{
		Pubkey:       "r3",
		Availability: "unavailable",
	}, "evt-3")

	count, err := store.CountAvailableReviewers(ctx)
	if err != nil {
		t.Fatalf("CountAvailableReviewers: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 available reviewers, got %d", count)
	}
}

func TestGetReviewerStats(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Create some assignments
	store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID:      "patch-1",
		RepoID:            "repo-1",
		ReviewerPubkey:    "reviewer-1",
		RequesterPubkey:   "requester-1",
		Status:            "completed",
		AssignmentEventID: "assign-1",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	})

	store.CreateAssignment(ctx, ReviewAssignment{
		PatchEventID:      "patch-2",
		RepoID:            "repo-1",
		ReviewerPubkey:    "reviewer-1",
		RequesterPubkey:   "requester-1",
		Status:            "rejected",
		AssignmentEventID: "assign-2",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	})

	a, _ := store.GetAssignmentByEventID(ctx, "assign-1")

	// Add feedback
	store.RecordFeedback(ctx, ReviewFeedback{
		AssignmentID:   a.ID,
		ReviewerPubkey: "reviewer-1",
		RaterPubkey:    "requester-1",
		Rating:         4,
		EventID:        "fb-1",
	})

	stats, err := store.GetReviewerStats(ctx, "reviewer-1")
	if err != nil {
		t.Fatalf("GetReviewerStats: %v", err)
	}

	if stats.TotalAssignments != 2 {
		t.Errorf("expected 2 total assignments, got %d", stats.TotalAssignments)
	}
	if stats.CompletedReviews != 1 {
		t.Errorf("expected 1 completed review, got %d", stats.CompletedReviews)
	}
	if stats.RejectedAssignments != 1 {
		t.Errorf("expected 1 rejected assignment, got %d", stats.RejectedAssignments)
	}
}
