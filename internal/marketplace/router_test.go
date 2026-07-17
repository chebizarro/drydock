package marketplace

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"drydock/internal/db"

	"fiatjaf.com/nostr"
)

func TestConcurrentAcceptRejectOnlyOneTransitionWins(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	registry := NewRegistry(store, slog.Default())
	if err := store.CreateAssignment(ctx, db.ReviewAssignment{
		PatchEventID:      "patch-transition-race",
		RepoID:            "repo-transition-race",
		ReviewerPubkey:    reviewer1Pubkey,
		RequesterPubkey:   "requester-transition-race",
		Status:            "pending",
		Priority:          2,
		AssignmentEventID: "assignment-transition-race",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- registry.RecordAcceptance(ctx, ReviewAcceptance{
			AssignmentID: "assignment-transition-race", ReviewerPubkey: reviewer1Pubkey, EventID: "accept-transition-event",
		})
	}()
	go func() {
		defer wg.Done()
		errs <- registry.RecordRejection(ctx, ReviewRejection{
			AssignmentID: "assignment-transition-race", ReviewerPubkey: reviewer1Pubkey, EventID: "reject-transition-event",
		})
	}()
	wg.Wait()
	close(errs)

	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful transitions = %d, want exactly 1", successes)
	}

	assignment, err := store.GetAssignmentByEventID(ctx, "assignment-transition-race")
	if err != nil {
		t.Fatal(err)
	}
	switch assignment.Status {
	case "accepted":
		err = registry.RecordAcceptance(ctx, ReviewAcceptance{AssignmentID: assignment.AssignmentEventID, ReviewerPubkey: reviewer1Pubkey, EventID: "accept-transition-event"})
	case "rejected":
		err = registry.RecordRejection(ctx, ReviewRejection{AssignmentID: assignment.AssignmentEventID, ReviewerPubkey: reviewer1Pubkey, EventID: "reject-transition-event"})
	default:
		t.Fatalf("final assignment status = %q", assignment.Status)
	}
	if err != nil {
		t.Fatalf("winning transition replay should be idempotent: %v", err)
	}
}

const (
	reviewer1Pubkey = "1111111111111111111111111111111111111111111111111111111111111111"
	reviewer2Pubkey = "2222222222222222222222222222222222222222222222222222222222222222"
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

type mockContextVMTransport struct {
	calls []contextVMCall
}

type contextVMCall struct {
	id         string
	method     string
	params     any
	recipients []nostr.PubKey
}

func (m *mockContextVMTransport) SendWithID(ctx context.Context, id, method string, params any, recipients ...nostr.PubKey) (string, error) {
	m.calls = append(m.calls, contextVMCall{id: id, method: method, params: params, recipients: append([]nostr.PubKey(nil), recipients...)})
	return id, nil
}

func TestRouter_HandleRejection_TriggersReassignment(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	logger := slog.Default()

	registry := NewRegistry(store, logger)
	signer := &mockSigner{pubkey: nostr.PubKey{}}
	publisher := &mockPublisher{}
	contextVMTransport := &mockContextVMTransport{}

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
		contextVMTransport,
		logger,
	)

	// Register two reviewers
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
		PriceSats:         0,
		AssignmentEventID: "assign-r1",
		ExpiresAt:         time.Now().Add(24 * time.Hour).Unix(),
	}
	if err := store.CreateAssignment(ctx, assignment); err != nil {
		t.Fatalf("CreateAssignment failed: %v", err)
	}

	// Simulate reviewer1 rejecting
	rejection := ReviewRejection{
		AssignmentID:   "assign-r1",
		ReviewerPubkey: reviewer1Pubkey,
		Reason:         "too busy",
	}
	rejectionJSON, _ := json.Marshal(rejection)

	rejectionEvent := nostr.Event{
		Kind:      nostr.Kind(25910),
		Content:   string(rejectionJSON),
		PubKey:    nostr.PubKey{}, // Will be overwritten
		CreatedAt: nostr.Now(),
	}

	// Handle the rejection
	err := router.HandleRejection(ctx, rejectionEvent)
	if err != nil {
		t.Fatalf("HandleRejection failed: %v", err)
	}

	// Check that a new assignment was published via ContextVM
	if len(contextVMTransport.calls) == 0 {
		t.Fatal("expected a reassignment to be published")
	}

	// Verify the new assignment is to reviewer2 (not reviewer1)
	newAssignment := contextVMTransport.calls[0].params.(ReviewAssignment)
	if newAssignment.ReviewerPubkey == reviewer1Pubkey {
		t.Error("reassignment should not be to the rejecting reviewer")
	}
	if contextVMTransport.calls[0].method != MethodAssign {
		t.Errorf("method = %s, want %s", contextVMTransport.calls[0].method, MethodAssign)
	}
	if contextVMTransport.calls[0].id != newAssignment.AssignmentID {
		t.Errorf("id = %s, want %s", contextVMTransport.calls[0].id, newAssignment.AssignmentID)
	}
}

func TestRouter_HandleRejection_NoAlternatives(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	logger := slog.Default()

	registry := NewRegistry(store, logger)
	signer := &mockSigner{pubkey: nostr.PubKey{}}
	publisher := &mockPublisher{}
	contextVMTransport := &mockContextVMTransport{}

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
		contextVMTransport,
		logger,
	)

	// Register only one reviewer
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
	rejection := ReviewRejection{
		AssignmentID:   "assign-only",
		ReviewerPubkey: reviewer1Pubkey,
		Reason:         "no time",
	}
	rejectionJSON, _ := json.Marshal(rejection)

	rejectionEvent := nostr.Event{
		Kind:      nostr.Kind(25910),
		Content:   string(rejectionJSON),
		CreatedAt: nostr.Now(),
	}

	// Handle the rejection - should not error, but no reassignment
	err := router.HandleRejection(ctx, rejectionEvent)
	if err != nil {
		t.Fatalf("HandleRejection failed: %v", err)
	}

	// No new assignment should be published (no alternatives)
	if len(contextVMTransport.calls) != 0 {
		t.Errorf("expected no reassignment, but got %d published", len(contextVMTransport.calls))
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
