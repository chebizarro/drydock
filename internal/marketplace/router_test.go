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
	reviewer1 := ReviewerProfile{
		Pubkey:       "reviewer1",
		Languages:    []string{"go"},
		Availability: AvailabilityAvailable,
	}
	reviewer2 := ReviewerProfile{
		Pubkey:       "reviewer2",
		Languages:    []string{"go"},
		Availability: AvailabilityAvailable,
	}
	registry.RegisterReviewer(ctx, reviewer1, "event1")
	registry.RegisterReviewer(ctx, reviewer2, "event2")

	// Create an initial assignment to reviewer1
	assignment := db.ReviewAssignment{
		PatchEventID:      "patch123",
		RepoID:            "repo1",
		ReviewerPubkey:    "reviewer1",
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
	rejection := ReviewRejection{
		AssignmentID:   "assign-r1",
		ReviewerPubkey: "reviewer1",
		Reason:         "too busy",
	}
	rejectionJSON, _ := json.Marshal(rejection)

	rejectionEvent := nostr.Event{
		Kind:      nostr.Kind(KindReviewRejection),
		Content:   string(rejectionJSON),
		PubKey:    nostr.PubKey{}, // Will be overwritten
		CreatedAt: nostr.Now(),
	}

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
		
		if newAssignment.ReviewerPubkey == "reviewer1" {
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
	reviewer1 := ReviewerProfile{
		Pubkey:       "reviewer1",
		Languages:    []string{"go"},
		Availability: AvailabilityAvailable,
	}
	registry.RegisterReviewer(ctx, reviewer1, "event1")

	// Create an initial assignment
	assignment := db.ReviewAssignment{
		PatchEventID:      "patch456",
		RepoID:            "repo1",
		ReviewerPubkey:    "reviewer1",
		RequesterPubkey:   "author1",
		Status:            "pending",
		Priority:          2,
		AssignmentEventID: "assign-only",
		ExpiresAt:         time.Now().Add(24 * time.Hour).Unix(),
	}
	store.CreateAssignment(ctx, assignment)

	// Simulate rejection
	rejection := ReviewRejection{
		AssignmentID: "assign-only",
		Reason:       "no time",
	}
	rejectionJSON, _ := json.Marshal(rejection)

	rejectionEvent := nostr.Event{
		Kind:      nostr.Kind(KindReviewRejection),
		Content:   string(rejectionJSON),
		CreatedAt: nostr.Now(),
	}

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
