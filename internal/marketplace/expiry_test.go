package marketplace

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"drydock/internal/db"
)

func TestExpiryService_ExpireNow(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Create some assignments - some expired, some not
	now := time.Now()
	
	// Expired assignment (expires_at in the past)
	expired := db.ReviewAssignment{
		PatchEventID:      "patch1",
		RepoID:            "repo1",
		ReviewerPubkey:    "reviewer1",
		RequesterPubkey:   "requester1",
		Status:            "pending",
		Priority:          2,
		AssignmentEventID: "assign1",
		ExpiresAt:         now.Add(-1 * time.Hour).Unix(), // Expired 1 hour ago
	}
	if err := store.CreateAssignment(ctx, expired); err != nil {
		t.Fatalf("CreateAssignment failed: %v", err)
	}

	// Non-expired assignment
	valid := db.ReviewAssignment{
		PatchEventID:      "patch2",
		RepoID:            "repo1",
		ReviewerPubkey:    "reviewer2",
		RequesterPubkey:   "requester1",
		Status:            "pending",
		Priority:          2,
		AssignmentEventID: "assign2",
		ExpiresAt:         now.Add(1 * time.Hour).Unix(), // Expires in 1 hour
	}
	if err := store.CreateAssignment(ctx, valid); err != nil {
		t.Fatalf("CreateAssignment failed: %v", err)
	}

	// Already accepted assignment (should not be expired even if past time)
	accepted := db.ReviewAssignment{
		PatchEventID:      "patch3",
		RepoID:            "repo1",
		ReviewerPubkey:    "reviewer3",
		RequesterPubkey:   "requester1",
		Status:            "accepted",
		Priority:          2,
		AssignmentEventID: "assign3",
		ExpiresAt:         now.Add(-1 * time.Hour).Unix(),
	}
	if err := store.CreateAssignment(ctx, accepted); err != nil {
		t.Fatalf("CreateAssignment failed: %v", err)
	}

	// Run expiry
	svc := NewExpiryService(DefaultExpiryConfig(), store, nil)
	count, err := svc.ExpireNow(ctx)
	if err != nil {
		t.Fatalf("ExpireNow failed: %v", err)
	}

	// Should expire exactly 1 (the pending one past expiry)
	if count != 1 {
		t.Errorf("ExpireNow returned %d, want 1", count)
	}

	// Verify the assignment is now expired
	assignment, err := store.GetAssignmentByEventID(ctx, "assign1")
	if err != nil {
		t.Fatalf("GetAssignmentByEventID failed: %v", err)
	}
	if assignment.Status != "expired" {
		t.Errorf("assignment status = %q, want %q", assignment.Status, "expired")
	}

	// Verify valid assignment is still pending
	validAssign, err := store.GetAssignmentByEventID(ctx, "assign2")
	if err != nil {
		t.Fatalf("GetAssignmentByEventID failed: %v", err)
	}
	if validAssign.Status != "pending" {
		t.Errorf("valid assignment status = %q, want %q", validAssign.Status, "pending")
	}

	// Verify accepted assignment is still accepted
	acceptedAssign, err := store.GetAssignmentByEventID(ctx, "assign3")
	if err != nil {
		t.Fatalf("GetAssignmentByEventID failed: %v", err)
	}
	if acceptedAssign.Status != "accepted" {
		t.Errorf("accepted assignment status = %q, want %q", acceptedAssign.Status, "accepted")
	}
}

func TestDefaultExpiryConfig(t *testing.T) {
	cfg := DefaultExpiryConfig()
	
	if cfg.CheckInterval != 5*time.Minute {
		t.Errorf("CheckInterval = %v, want 5m", cfg.CheckInterval)
	}
	if cfg.BatchSize != 100 {
		t.Errorf("BatchSize = %d, want 100", cfg.BatchSize)
	}
}

func TestNewExpiryService_DefaultsApplied(t *testing.T) {
	store := &db.Store{} // Won't be used in this test
	
	// Test with zero config - should apply defaults
	svc := NewExpiryService(ExpiryConfig{}, store, nil)
	
	if svc.cfg.CheckInterval != 5*time.Minute {
		t.Errorf("CheckInterval = %v, want 5m", svc.cfg.CheckInterval)
	}
	if svc.cfg.BatchSize != 100 {
		t.Errorf("BatchSize = %d, want 100", svc.cfg.BatchSize)
	}
}

// mustOpenStore is already defined in marketplace_test.go but we need it here too
func mustOpenStoreForExpiry(t *testing.T, ctx context.Context) *db.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}
