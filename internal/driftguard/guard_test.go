package driftguard

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/db"
)

func testStore(t *testing.T) *db.Store {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	store, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func seedMetaReviews(t *testing.T, ctx context.Context, store *db.Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		err := store.InsertMetaReviewLog(ctx, "patch-"+string(rune('a'+i)), "repo-1", "hash-1",
			[]string{"file.go"}, "low-confidence",
			`{"missed_findings":[],"false_positives":[],"reasoning_quality":0.8,"context_utilization":0.7,"prompt_gaps":["gap-`+string(rune('a'+i))+`"],"suggested_few_shot":false}`,
		)
		if err != nil {
			t.Fatalf("seed meta review %d: %v", i, err)
		}
	}
}

func TestExportSampleEmpty(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	svc := NewService(store, testLogger())

	var buf bytes.Buffer
	n, err := svc.ExportSample(ctx, &buf, 10)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 exported, got %d", n)
	}
	if !strings.Contains(buf.String(), "No meta-reviews found") {
		t.Error("expected empty message")
	}
}

func TestExportSampleReturnsReviews(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedMetaReviews(t, ctx, store, 5)

	svc := NewService(store, testLogger())

	var buf bytes.Buffer
	n, err := svc.ExportSample(ctx, &buf, 3)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 exported, got %d", n)
	}
	output := buf.String()
	if !strings.Contains(output, "Convention Drift Guard") {
		t.Error("expected header in output")
	}
	if !strings.Contains(output, "Review #1") {
		t.Error("expected Review #1 in output")
	}
	if !strings.Contains(output, "drift-guard") {
		t.Error("expected flag instructions in output")
	}
}

func TestFlagReview(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedMetaReviews(t, ctx, store, 1)

	svc := NewService(store, testLogger())

	// Flag meta-review ID 1
	err := svc.FlagReview(ctx, 1, "recommends var naming that conflicts with project style")
	if err != nil {
		t.Fatalf("flag: %v", err)
	}

	// Verify it shows up in list
	var buf bytes.Buffer
	n, err := svc.ListFlagged(ctx, &buf)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 flagged, got %d", n)
	}
	if !strings.Contains(buf.String(), "Meta-Review ID: 1") {
		t.Error("expected meta-review ID in output")
	}
	if !strings.Contains(buf.String(), "naming that conflicts") {
		t.Error("expected notes in output")
	}
}

func TestListFlaggedEmpty(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	svc := NewService(store, testLogger())

	var buf bytes.Buffer
	n, err := svc.ListFlagged(ctx, &buf)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
	if !strings.Contains(buf.String(), "No drift-flagged") {
		t.Error("expected empty message")
	}
}

func TestNegativeExamplesForPrompt(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedMetaReviews(t, ctx, store, 2)

	svc := NewService(store, testLogger())

	// No flags yet → empty string
	examples, err := svc.NegativeExamplesForPrompt(ctx, 10)
	if err != nil {
		t.Fatalf("negative examples: %v", err)
	}
	if examples != "" {
		t.Errorf("expected empty, got %q", examples)
	}

	// Flag one review
	err = svc.FlagReview(ctx, 1, "generic advice conflicts with Go style guide")
	if err != nil {
		t.Fatalf("flag: %v", err)
	}

	examples, err = svc.NegativeExamplesForPrompt(ctx, 10)
	if err != nil {
		t.Fatalf("negative examples: %v", err)
	}
	if !strings.Contains(examples, "Convention drift examples") {
		t.Error("expected drift header")
	}
	if !strings.Contains(examples, "DO NOT recommend") {
		t.Error("expected negative instruction")
	}
	if !strings.Contains(examples, "Go style guide") {
		t.Error("expected notes in examples")
	}
}

func TestNegativeExamplesForPromptMultiple(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedMetaReviews(t, ctx, store, 3)

	svc := NewService(store, testLogger())

	svc.FlagReview(ctx, 1, "conflicts with project naming")
	svc.FlagReview(ctx, 2, "generic error handling advice")

	examples, err := svc.NegativeExamplesForPrompt(ctx, 10)
	if err != nil {
		t.Fatalf("negative examples: %v", err)
	}
	if !strings.Contains(examples, "Drift example 1") {
		t.Error("expected example 1")
	}
	if !strings.Contains(examples, "Drift example 2") {
		t.Error("expected example 2")
	}
}
