package pipeline

import (
	"context"
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
	"drydock/internal/metrics"
	"drydock/internal/publisher"
	"drydock/internal/repo"
	"drydock/internal/repoconfig"
	"drydock/internal/reviewengine"
	"drydock/internal/testutil"
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

func TestProcessEndToEndPersistsAndPublishesReview(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	patchID, repoID := seedIntegrationDB(t, ctx, store)
	logger := testLogger()

	cacheDir := filepath.Join(t.TempDir(), "repos")
	initRepoInCanonicalCache(t, cacheDir, repoID)
	repoMgr := repo.NewManager(cacheDir, logger)
	repoSvc := repo.NewService(store, repoMgr, logger)

	fakeLLM := &testutil.FakeLLM{
		Responses: []string{
			`{"change_type":"bugfix","risk_areas":["correctness"],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`,
			`{"summary":"Runner process found a real issue","findings":[{"severity":"high","category":"correctness","file":"main.go","line":2,"evidence":"reviewed comment","explanation":"The runner passed assembled context into the reviewer.","suggestion":"Keep the review path wired.","confidence":0.85}],"needs_more_context":[]}`,
			`{"walkthrough":"The patch adds a reviewed marker comment.","file_summaries":[{"file":"main.go","summary":"Adds a comment below the package declaration"}]}`,
		},
	}
	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "planner"},
		Coder32B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder32b"},
		LLM70B:   reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "llm70b"},
		Coder14B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder14b"},
	}, fakeLLM, logger)

	relayPub := &collectingRelayPublisher{}
	pubSvc := publisher.New(publisher.Config{
		DefaultRelays:       []string{"wss://relay.test"},
		DetailSeverityFloor: "high",
		DefaultTTL:          90 * 24 * time.Hour,
		SupersededTTL:       7 * 24 * time.Hour,
	}, store, testSigner{sk: nostr.Generate()}, relayPub, logger)

	runner := New(Config{Workers: 1}, store, repoSvc, contextbuilder.NewDefault(), engine, pubSvc, nil, make(chan db.ReviewTask), logger)
	if err := runner.process(ctx, db.ReviewTask{PatchEventID: patchID, RepoID: repoID}); err != nil {
		t.Fatalf("process failed: %v", err)
	}

	if len(fakeLLM.Requests) != 3 {
		t.Fatalf("expected planner, reviewer, walkthrough LLM calls; got %d", len(fakeLLM.Requests))
	}
	if !strings.Contains(fakeLLM.Requests[0].User, "+// reviewed") || !strings.Contains(fakeLLM.Requests[1].User, "+// reviewed") {
		t.Fatalf("LLM prompts did not include assembled patch context: planner=%q reviewer=%q", fakeLLM.Requests[0].User, fakeLLM.Requests[1].User)
	}

	if len(relayPub.events) < 2 {
		t.Fatalf("expected summary and high-severity detail events, got %d", len(relayPub.events))
	}
	summaryEvt := relayPub.events[0]
	if summaryEvt.Kind != nostr.KindComment {
		t.Fatalf("summary kind = %d, want %d", summaryEvt.Kind, nostr.KindComment)
	}
	if !summaryEvt.CheckID() || !summaryEvt.VerifySignature() {
		t.Fatal("published summary event is not a valid signed nostr event")
	}
	if !strings.Contains(summaryEvt.Content, "Runner process found a real issue") || !strings.Contains(summaryEvt.Content, "context-hash:") {
		t.Fatalf("summary content missing review output/footer: %s", summaryEvt.Content)
	}
	if !strings.Contains(relayPub.events[1].Content, "The runner passed assembled context") {
		t.Fatalf("detail content missing finding explanation: %s", relayPub.events[1].Content)
	}

	status, err := store.GetReviewStatus(ctx, patchID, repoID)
	if err != nil {
		t.Fatalf("get review status: %v", err)
	}
	if status != "published" {
		t.Fatalf("review status = %q, want published", status)
	}
	storedReviewID, err := store.GetReviewEventID(ctx, patchID, repoID)
	if err != nil {
		t.Fatalf("get review event id: %v", err)
	}
	if storedReviewID != summaryEvt.ID.Hex() {
		t.Fatalf("stored review event id = %q, want published summary %q", storedReviewID, summaryEvt.ID.Hex())
	}
}

func TestCheckReviewStatusForceBypassesDraftAndClosed(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	runner := &Runner{store: store, logger: testLogger()}
	const rootID = "root"
	const repoID = "owner:repo"

	for _, kind := range []int{int(nostr.KindStatusDraft), int(nostr.KindStatusClosed)} {
		if _, err := store.DB().ExecContext(ctx, `DELETE FROM root_statuses`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().ExecContext(ctx, `INSERT INTO root_statuses
			(root_event_id, repo_id, status_kind, status_event_id, author_pubkey, created_at, updated_at)
			VALUES (?, ?, ?, 'status-event', 'author', 1, 1)`, rootID, repoID, kind); err != nil {
			t.Fatal(err)
		}

		forced := db.ReviewTask{PatchEventID: "patch", RepoID: repoID, Force: true}
		if err := runner.checkReviewStatus(ctx, forced, rootID, []string{"open"}); err != nil {
			t.Fatalf("forced status %d was denied: %v", kind, err)
		}
		normal := db.ReviewTask{PatchEventID: "patch", RepoID: repoID}
		if err := runner.checkReviewStatus(ctx, normal, rootID, []string{"open"}); err == nil || !strings.HasPrefix(err.Error(), "status_skipped:") {
			t.Fatalf("ordinary status %d error = %v, want status_skipped", kind, err)
		}
	}
}

func TestPipelinePureHelpers(t *testing.T) {
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

	t.Run("reviewStatusAllowed", func(t *testing.T) {
		cases := []struct {
			name      string
			kind      int
			hasStatus bool
			allowed   []string
			want      bool
		}{
			{"no status counts as open", 0, false, []string{"open"}, true},
			{"open allowed", 1630, true, []string{"open"}, true},
			{"draft not allowed by default", 1633, true, []string{"open"}, false},
			{"draft allowed when configured", 1633, true, []string{"open", "draft"}, true},
			{"merged never allowed", 1631, true, []string{"open", "draft"}, false},
			{"closed never allowed", 1632, true, []string{"open", "draft"}, false},
			{"no status but only draft configured", 0, false, []string{"draft"}, false},
			{"unknown status kind rejected", 9999, true, []string{"open", "draft"}, false},
		}
		for _, tc := range cases {
			reason, got := reviewStatusAllowed(tc.kind, tc.hasStatus, tc.allowed)
			if got != tc.want {
				t.Fatalf("%s: allowed=%v (reason %q), want %v", tc.name, got, reason, tc.want)
			}
			if !got && reason == "" {
				t.Fatalf("%s: disallowed result must carry a reason", tc.name)
			}
		}
	})

	t.Run("modelName_nilEngineFallsBackToRoute", func(t *testing.T) {
		name := modelName(reviewengine.RunOutput{Route: reviewengine.RouteCoder32B}, nil)
		if name != "coder32b" {
			t.Fatalf("expected 'coder32b', got %s", name)
		}
	})

	t.Run("modelName_prefersServedModel", func(t *testing.T) {
		out := reviewengine.RunOutput{Route: reviewengine.RouteCoder32B, ServedModel: "gemma-4-26b"}
		if name := modelName(out, nil); name != "gemma-4-26b" {
			t.Fatalf("expected per-run served model, got %s", name)
		}
	})

	t.Run("modelName_resolvesConfiguredModel", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		engine := reviewengine.New(reviewengine.Config{
			Coder32B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "qwen2.5-coder-32b-instruct"},
		}, nil, logger)
		out := reviewengine.RunOutput{Route: reviewengine.RouteCoder32B}
		if name := modelName(out, engine); name != "qwen2.5-coder-32b-instruct" {
			t.Fatalf("expected configured model name, got %s", name)
		}
		// Route with no configured model falls back to the route alias.
		if name := modelName(reviewengine.RunOutput{Route: reviewengine.RouteLLM70B}, engine); name != "llm70b" {
			t.Fatalf("expected route alias fallback, got %s", name)
		}
	})
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

func TestCheckPatchSupersededFailsClosedAfterRetry(t *testing.T) {
	calls := 0
	runner := &Runner{
		logger: testLogger(),
		isPatchSuperseded: func(context.Context, string, string, string) (bool, error) {
			calls++
			return false, errors.New("database unavailable")
		},
	}

	_, err := runner.checkPatchSuperseded(context.Background(), "patch", "root", "repo")
	if err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("expected supersession lookup error, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("supersession lookup calls = %d, want 2", calls)
	}
}

func TestPublishReviewStatusReturnsFailure(t *testing.T) {
	wantErr := errors.New("relay rejected status")
	runner := &Runner{
		publishStatus: func(context.Context, publisher.PublishStatusInput) (publisher.PublishStatusResult, error) {
			return publisher.PublishStatusResult{}, wantErr
		},
	}

	_, err := runner.publishReviewStatus(context.Background(), publisher.PublishStatusInput{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("status publication error = %v, want %v", err, wantErr)
	}
}

func TestTryAutoFixRecordsSynthesisFailure(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	patchID, repoID := seedPatchForPipeline(t, ctx, store)
	beforeFailures := metrics.AutoFixPublishFailures.Value()

	runner := &Runner{
		store:  store,
		logger: testLogger(),
		buildAutoFixPatch: func(context.Context, string, []repo.AutoFixSuggestion) (repo.AutoFixResult, error) {
			return repo.AutoFixResult{}, errors.New("git apply failed")
		},
	}
	cfg := repoconfig.Default()
	cfg.AutoFix.MinConfidence = 0.5
	cfg.AutoFix.MaxFindings = 3
	review := reviewengine.ReviewerOutput{Findings: []reviewengine.Finding{{
		File:          "main.go",
		Confidence:    0.9,
		SuggestedDiff: "diff --git a/main.go b/main.go",
	}}}

	result := runner.tryAutoFix(ctx, db.ReviewTask{PatchEventID: patchID, RepoID: repoID}, repo.PrepareResult{RepoPath: "/repo"}, review, cfg, "review-event", "model")
	if result == nil || !result.Attempted || result.Published {
		t.Fatalf("unexpected autofix result: %#v", result)
	}
	if metrics.AutoFixPublishFailures.Value() != beforeFailures+1 {
		t.Fatalf("autofix failure metric did not increment")
	}
	note, err := store.GetReviewNote(ctx, patchID, repoID)
	if err != nil {
		t.Fatalf("get review note: %v", err)
	}
	if !strings.Contains(note, "autofix failed") || !strings.Contains(note, "git apply failed") {
		t.Fatalf("autofix failure note = %q", note)
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
