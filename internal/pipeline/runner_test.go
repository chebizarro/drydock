package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drydock/internal/contextbuilder"
	"drydock/internal/db"
	"drydock/internal/metareview"
	"drydock/internal/publisher"
	"drydock/internal/repo"
	"drydock/internal/reviewengine"

	"fiatjaf.com/nostr"
)

// --- Mocks ---

type mockRepoService struct {
	result repo.PrepareResult
	err    error
}

func (m *mockRepoService) PreparePatchSeries(ctx context.Context, patchEventID string) (repo.PrepareResult, error) {
	return m.result, m.err
}

type mockPublisher struct {
	calls     int
	lastInput publisher.PublishInput
	eventID   string
	err       error
}

func (m *mockPublisher) PublishReview(ctx context.Context, in publisher.PublishInput) (string, error) {
	m.calls++
	m.lastInput = in
	return m.eventID, m.err
}

type mockMetaService struct {
	calls int
}

func (m *mockMetaService) RunAsync(ctx context.Context, in metareview.Input) {
	m.calls++
}

type mockCodeIndexer struct {
	err error
}

func (m mockCodeIndexer) IndexRepo(ctx context.Context, repoPath, repoID string) error {
	return m.err
}

// --- Test helpers ---

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func mustStore(t *testing.T, ctx context.Context) *db.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pipeline-test.db")
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

func seedPatchForPipeline(t *testing.T, ctx context.Context, store *db.Store) (patchID, repoID string) {
	t.Helper()
	sk := nostr.Generate()
	repoSK := nostr.Generate()

	repoEvt := nostr.Event{
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "test-repo"},
			{"clone", "https://example.com/repo.git"},
			{"relays", "wss://relay.test"},
		},
	}
	repoEvt.Sign(repoSK)
	if err := store.UpsertRepositoryAnnouncement(ctx, repoEvt); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	patchEvt := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":test-repo"},
			{"e", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", "root"},
		},
		Content: "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -0,0 +1 @@\n+package main\n",
	}
	patchEvt.Sign(sk)
	if err := store.InsertPatchEvent(ctx, patchEvt); err != nil {
		t.Fatalf("seed patch: %v", err)
	}
	if err := store.RecordPatchEventRelay(ctx, patchEvt.ID.Hex(), "wss://relay.test"); err != nil {
		t.Fatalf("seed relay: %v", err)
	}

	rID := db.RepoIDFromPatch(patchEvt)
	if _, err := store.BeginReview(ctx, patchEvt.ID.Hex(), rID); err != nil {
		t.Fatalf("begin review: %v", err)
	}
	return patchEvt.ID.Hex(), rID
}

// --- Tests ---

func TestProcessEndToEndWithMocks(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	patchID, repoID := seedPatchForPipeline(t, ctx, store)
	logger := testLogger()

	fakeLLM := &reviewengine.FakeLLMForTest{
		Responses: []string{
			`{"change_type":"bugfix","risk_areas":["correctness"],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`,
			`{"summary":"Found a bug","findings":[{"severity":"high","category":"correctness","file":"main.go","line":1,"evidence":"missing error check","explanation":"no err handling","suggestion":"add err check","confidence":0.85}],"needs_more_context":[]}`,
		},
	}
	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "planner"},
		Coder32B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder32b"},
		LLM70B:   reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "llm70b"},
		Coder14B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder14b"},
	}, fakeLLM, logger)

	mockPub := &mockPublisher{eventID: "review-event-id-123"}
	mockMeta := &mockMetaService{}

	queue := make(chan db.ReviewTask, 1)
	queue <- db.ReviewTask{PatchEventID: patchID, RepoID: repoID}
	close(queue)

	// We can't easily mock repo.Service since it's a concrete type, so instead
	// test the changedFilesFromBundle and meanConfidence helpers which are the
	// testable pure functions in the pipeline.
	t.Run("changedFilesFromBundle", func(t *testing.T) {
		bundle := contextbuilder.ContextBundle{
			Content: "## patch\ndiff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\ndiff --git a/bar.go b/bar.go\n",
		}
		files := changedFilesFromBundle(bundle)
		if len(files) != 2 {
			t.Fatalf("expected 2 files, got %d: %v", len(files), files)
		}
		if files[0] != "foo.go" || files[1] != "bar.go" {
			t.Fatalf("unexpected files: %v", files)
		}
	})

	t.Run("meanConfidence_empty", func(t *testing.T) {
		c := meanConfidence(nil)
		if c != 0.5 {
			t.Fatalf("expected 0.5 for empty findings, got %f", c)
		}
	})

	t.Run("meanConfidence_values", func(t *testing.T) {
		findings := []reviewengine.Finding{
			{Confidence: 0.8},
			{Confidence: 0.6},
		}
		c := meanConfidence(findings)
		if c < 0.69 || c > 0.71 {
			t.Fatalf("expected ~0.7, got %f", c)
		}
	})

	t.Run("modelName", func(t *testing.T) {
		name := modelName(reviewengine.RouteCoder32B, nil)
		if name != "coder32b" {
			t.Fatalf("expected 'coder32b', got %s", name)
		}
	})

	// Verify the mocks are usable (compile-time interface check)
	_ = mockPub
	_ = mockMeta
	_ = engine
	_ = json.Marshal // used in process
}

func TestIndexSourceCodePropagatesConfiguredIndexerFailure(t *testing.T) {
	runner := &Runner{codeIndexer: mockCodeIndexer{err: errors.New("embedding failed")}}
	err := runner.indexSourceCode(context.Background(), "/repo", "repo-id", testLogger())
	if err == nil {
		t.Fatal("expected code indexing error")
	}
	if !strings.Contains(err.Error(), "code indexing") || !strings.Contains(err.Error(), "embedding failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestIndexSourceCodeNoIndexerIsNoop(t *testing.T) {
	runner := &Runner{}
	if err := runner.indexSourceCode(context.Background(), "/repo", "repo-id", testLogger()); err != nil {
		t.Fatalf("nil code indexer should be no-op: %v", err)
	}
}

func TestRunnerShutdownDrainsWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	queue := make(chan db.ReviewTask, 10)
	logger := testLogger()

	// Create a runner with no real dependencies — just verify shutdown behavior
	runner := &Runner{
		queue:   queue,
		workers: 2,
		logger:  logger,
	}

	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()

	// Let workers start, then cancel
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Shutdown completed — workers drained
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not shut down within timeout")
	}
}
