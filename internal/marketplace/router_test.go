package marketplace

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"drydock/internal/db"

	"fiatjaf.com/nostr"
)

// mockSigner is a test signer that records signed events.
type mockSigner struct {
	pubkey nostr.PubKey
	signed []*nostr.Event
}

func (m *mockSigner) GetPublicKey(ctx context.Context) (nostr.PubKey, error) {
	return m.pubkey, nil
}

func (m *mockSigner) SignEvent(ctx context.Context, evt *nostr.Event) error {
	// Generate a proper signature using the library
	sk := nostr.Generate()
	evt.Sign(sk)
	m.signed = append(m.signed, evt)
	return nil
}

// mockPublisher records published events.
type mockPublisher struct {
	published []nostr.Event
}

func (m *mockPublisher) Publish(ctx context.Context, relays []string, event nostr.Event) error {
	m.published = append(m.published, event)
	return nil
}

func TestRouter_RoutePatch_RecordFailureDoesNotReportAssignmentSuccess(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	logger := slog.Default()

	registry := NewRegistry(store, logger)
	signer := &mockSigner{pubkey: testPubKey()}
	publisher := &mockPublisher{}
	router := NewRouter(
		RouterConfig{
			DefaultRelays:        []string{"wss://test.relay"},
			MaxReviewersPerPatch: 1,
			DefaultDeadline:      24 * time.Hour,
		},
		registry,
		store,
		signer,
		publisher,
		logger,
	)

	reviewerPubkey := testPubKey().Hex()
	if err := registry.RegisterReviewer(ctx, ReviewerProfile{
		Pubkey:         reviewerPubkey,
		Languages:      []string{"go"},
		Availability:   AvailabilityAvailable,
		PricePerReview: 0,
	}, "reviewer-event-record-failure"); err != nil {
		t.Fatalf("RegisterReviewer: %v", err)
	}

	if _, err := store.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_route_patch_assignment_insert
		BEFORE INSERT ON review_assignments
		BEGIN
			SELECT RAISE(ABORT, 'forced assignment persistence failure');
		END;
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	result, err := router.RoutePatch(ctx, PatchInfo{
		PatchEventID: "patch-record-failure",
		RepoID:       "repo-record-failure",
		AuthorPubkey: testPubKey().Hex(),
		ChangedFiles: []string{"main.go"},
		PriceSats:    100,
	})
	if err == nil {
		t.Fatal("RoutePatch succeeded despite assignment persistence failure")
	}
	if result == nil {
		t.Fatal("RoutePatch returned nil result; want partial result showing no successful assignment")
	}
	if result.AssignedCount != 0 {
		t.Fatalf("AssignedCount = %d, want 0", result.AssignedCount)
	}
	if len(result.Assignments) != 0 {
		t.Fatalf("len(Assignments) = %d, want 0", len(result.Assignments))
	}
	if len(publisher.published) != 0 {
		t.Fatalf("published %d assignments despite persistence failure; want 0", len(publisher.published))
	}
}

func TestRouter_RoutePatch_RejectsReviewerAbovePatchPriceBeforeAssignment(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	logger := slog.Default()

	registry := NewRegistry(store, logger)
	publisher := &mockPublisher{}
	router := NewRouter(
		RouterConfig{
			DefaultRelays:        []string{"wss://test.relay"},
			MaxReviewersPerPatch: 1,
			DefaultDeadline:      24 * time.Hour,
		},
		registry,
		store,
		&mockSigner{pubkey: testPubKey()},
		publisher,
		logger,
	)

	reviewerPubkey := testPubKey().Hex()
	if err := registry.RegisterReviewer(ctx, ReviewerProfile{
		Pubkey:         reviewerPubkey,
		Languages:      []string{"go"},
		Availability:   AvailabilityAvailable,
		PricePerReview: 100,
	}, "reviewer-event-over-price"); err != nil {
		t.Fatalf("RegisterReviewer: %v", err)
	}

	result, err := router.RoutePatch(ctx, PatchInfo{
		PatchEventID: "patch-free-review",
		RepoID:       "repo-free-review",
		AuthorPubkey: testPubKey().Hex(),
		ChangedFiles: []string{"main.go"},
		PriceSats:    0,
	})
	if err != nil {
		t.Fatalf("RoutePatch returned error: %v", err)
	}
	if result.AssignedCount != 0 {
		t.Fatalf("AssignedCount = %d, want 0", result.AssignedCount)
	}
	if len(result.Assignments) != 0 {
		t.Fatalf("len(Assignments) = %d, want 0", len(result.Assignments))
	}
	if len(publisher.published) != 0 {
		t.Fatalf("published %d assignments despite price cap; want 0", len(publisher.published))
	}
	assignments, err := store.ListAssignmentsForPatch(ctx, "patch-free-review")
	if err != nil {
		t.Fatalf("ListAssignmentsForPatch: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("stored %d assignments despite price cap; want 0", len(assignments))
	}
}

func TestRouter_HandleRejection_TriggersReassignment(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	logger := slog.Default()

	registry := NewRegistry(store, logger)
	signer := &mockSigner{pubkey: nostr.PubKey{}}
	publisher := &mockPublisher{}

	router := NewRouter(
		RouterConfig{
			DefaultRelays:        []string{"wss://test.relay"},
			MaxReviewersPerPatch: 2,
			DefaultDeadline:      24 * time.Hour,
		},
		registry,
		store,
		signer,
		publisher,
		logger,
	)

	// Register two reviewers
	reviewer1SK := nostr.Generate()
	reviewer1Pubkey := nostr.GetPublicKey(reviewer1SK).Hex()
	reviewer2Pubkey := testPubKey().Hex()
	reviewer1 := ReviewerProfile{
		Pubkey:       reviewer1Pubkey,
		Languages:    []string{"go"},
		Availability: AvailabilityAvailable,
	}
	reviewer2 := ReviewerProfile{
		Pubkey:       reviewer2Pubkey,
		Languages:    []string{"go"},
		Availability: AvailabilityAvailable,
	}
	registry.RegisterReviewer(ctx, reviewer1, "event1")
	registry.RegisterReviewer(ctx, reviewer2, "event2")

	// Create an initial assignment to reviewer1
	assignment := db.ReviewAssignment{
		PatchEventID:      "patch123",
		RepoID:            "repo1",
		ReviewerPubkey:    reviewer1Pubkey,
		RequesterPubkey:   "author1",
		Status:            "pending",
		Priority:          2,
		PriceSats:         1000,
		AssignmentEventID: "assign-r1",
		ExpiresAt:         time.Now().Add(24 * time.Hour).Unix(),
	}
	if err := store.CreateAssignment(ctx, assignment); err != nil {
		t.Fatalf("CreateAssignment failed: %v", err)
	}

	// Simulate reviewer1 rejecting
	rejectionEvent := signedMarketplaceEvent(t, reviewer1SK, KindReviewRejection, ReviewRejection{
		AssignmentID: "assign-r1",
		Reason:       "too busy",
	})

	// Handle the rejection
	err := router.HandleRejection(ctx, rejectionEvent)
	if err != nil {
		t.Fatalf("HandleRejection failed: %v", err)
	}

	// Check that a new assignment was published
	if len(publisher.published) == 0 {
		t.Error("expected a reassignment to be published")
	}

	// Verify the new assignment is to reviewer2 (not reviewer1)
	if len(publisher.published) > 0 {
		var newAssignment ReviewAssignment
		json.Unmarshal([]byte(publisher.published[0].Content), &newAssignment)

		if newAssignment.ReviewerPubkey == reviewer1Pubkey {
			t.Error("reassignment should not be to the rejecting reviewer")
		}
	}
}

func TestRouter_HandleRejection_NoAlternatives(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	logger := slog.Default()

	registry := NewRegistry(store, logger)
	signer := &mockSigner{pubkey: nostr.PubKey{}}
	publisher := &mockPublisher{}

	router := NewRouter(
		RouterConfig{
			DefaultRelays:        []string{"wss://test.relay"},
			MaxReviewersPerPatch: 2,
			DefaultDeadline:      24 * time.Hour,
		},
		registry,
		store,
		signer,
		publisher,
		logger,
	)

	// Register only one reviewer
	reviewer1SK := nostr.Generate()
	reviewer1Pubkey := nostr.GetPublicKey(reviewer1SK).Hex()
	reviewer1 := ReviewerProfile{
		Pubkey:       reviewer1Pubkey,
		Languages:    []string{"go"},
		Availability: AvailabilityAvailable,
	}
	registry.RegisterReviewer(ctx, reviewer1, "event1")

	// Create an initial assignment
	assignment := db.ReviewAssignment{
		PatchEventID:      "patch456",
		RepoID:            "repo1",
		ReviewerPubkey:    reviewer1Pubkey,
		RequesterPubkey:   "author1",
		Status:            "pending",
		Priority:          2,
		AssignmentEventID: "assign-only",
		ExpiresAt:         time.Now().Add(24 * time.Hour).Unix(),
	}
	store.CreateAssignment(ctx, assignment)

	// Simulate rejection
	rejectionEvent := signedMarketplaceEvent(t, reviewer1SK, KindReviewRejection, ReviewRejection{
		AssignmentID: "assign-only",
		Reason:       "no time",
	})

	// Handle the rejection - should not error, but no reassignment
	err := router.HandleRejection(ctx, rejectionEvent)
	if err != nil {
		t.Fatalf("HandleRejection failed: %v", err)
	}

	// No new assignment should be published (no alternatives)
	if len(publisher.published) != 0 {
		t.Errorf("expected no reassignment, but got %d published", len(publisher.published))
	}
}

func TestRouter_DetectLanguages(t *testing.T) {
	router := &Router{}

	tests := []struct {
		files    []string
		expected []string
	}{
		{
			files:    []string{"main.go", "utils.go"},
			expected: []string{"go"},
		},
		{
			files:    []string{"src/main.rs", "lib.rs"},
			expected: []string{"rust"},
		},
		{
			files:    []string{"app.ts", "utils.js"},
			expected: []string{"typescript", "javascript"},
		},
	}

	for _, tc := range tests {
		langs := router.detectLanguages(tc.files)
		if len(langs) != len(tc.expected) {
			t.Errorf("detectLanguages(%v) returned %d languages, want %d",
				tc.files, len(langs), len(tc.expected))
			continue
		}

		langSet := make(map[string]bool)
		for _, l := range langs {
			langSet[l] = true
		}
		for _, exp := range tc.expected {
			if !langSet[exp] {
				t.Errorf("detectLanguages(%v) missing %s", tc.files, exp)
			}
		}
	}
}
