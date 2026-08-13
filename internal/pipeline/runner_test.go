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
	"drydock/internal/payment"
	"drydock/internal/publisher"
	"drydock/internal/repo"
	"drydock/internal/repoconfig"
	"drydock/internal/reviewengine"
	"drydock/internal/testutil"
	"fiatjaf.com/nostr"
)

// --- Mocks ---

type allowAllRegistry struct{}

func (allowAllRegistry) Contains(string) bool { return true }

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

	runner := New(Config{Workers: 1, AgenticReviewFallback: true}, store, repoSvc, contextbuilder.NewDefault(), engine, pubSvc, nil, make(chan db.ReviewTask), logger, WithMonitoringRegistry(allowAllRegistry{}))
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

func TestPatchDiffForReviewUsesSelectedRevisionContent(t *testing.T) {
	selected := "diff --git a/selected.go b/selected.go\n"
	cumulative := "diff --git a/root.go b/root.go\n" + selected

	got, err := patchDiffForReview("target-id", 1617, selected, cumulative)
	if err != nil {
		t.Fatalf("select revision diff: %v", err)
	}
	if got != selected {
		t.Fatalf("revision prompt diff = %q, want selected event diff %q", got, selected)
	}
}

func TestPublishApplyFailureNoticeNamesFailingMemberAndRequestedTarget(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	targetID, repoID := seedPatchForPipeline(t, ctx, store)
	failingID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hint := "patch " + failingID + " does not apply cleanly: conflict; requested target " + targetID

	relayPub := &collectingRelayPublisher{}
	pubSvc := publisher.New(publisher.Config{
		DefaultRelays: []string{"wss://relay.test"},
		DefaultTTL:    90 * 24 * time.Hour,
	}, store, testSigner{sk: nostr.Generate()}, relayPub, testLogger())
	runner := &Runner{store: store, pubSvc: pubSvc, logger: testLogger()}

	runner.publishApplyFailure(ctx, db.ReviewTask{PatchEventID: targetID, RepoID: repoID}, repo.PrepareFailureApply, hint)

	if len(relayPub.events) != 1 {
		t.Fatalf("published events = %d, want 1", len(relayPub.events))
	}
	content := relayPub.events[0].Content
	for _, want := range []string{failingID, targetID} {
		if !strings.Contains(content, want) {
			t.Fatalf("apply-failure publication %q does not name %s", content, want)
		}
	}
	if strings.Contains(content, "Automated review summary") || strings.Contains(content, "model: none") {
		t.Fatalf("apply failure was formatted as a review: %s", content)
	}
	noticeType := relayPub.events[0].Tags.Find("drydock-type")
	if noticeType == nil || len(noticeType) < 2 || noticeType[1] != publisher.FailureNoticeType {
		t.Fatalf("missing operational notice tag: %v", noticeType)
	}
	if reviewID, err := store.GetReviewEventID(ctx, targetID, repoID); err != nil || reviewID != "" {
		t.Fatalf("failure notice reserved an ordinary review id: id=%q err=%v", reviewID, err)
	}
}

func TestPublishApplyFailureCanBeSuppressed(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	targetID, repoID := seedPatchForPipeline(t, ctx, store)
	relayPub := &collectingRelayPublisher{}
	pubSvc := publisher.New(publisher.Config{
		DefaultRelays: []string{"wss://relay.test"},
	}, store, testSigner{sk: nostr.Generate()}, relayPub, testLogger())
	runner := &Runner{
		store: store, pubSvc: pubSvc, logger: testLogger(),
		applyFailurePublication: ApplyFailurePublicationSuppress,
	}
	before := metrics.FailureNoticesSuppressed.Value()

	runner.publishApplyFailure(ctx, db.ReviewTask{PatchEventID: targetID, RepoID: repoID}, repo.PrepareFailureApply, "conflict")

	if len(relayPub.events) != 0 {
		t.Fatalf("published events = %d, want 0", len(relayPub.events))
	}
	if got := metrics.FailureNoticesSuppressed.Value(); got != before+1 {
		t.Fatalf("suppressed metric = %d, want %d", got, before+1)
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

func TestRetryablePaymentPendingUsesOrdinaryRequeuePath(t *testing.T) {
	err := retryablePaymentError(payment.AuthorizeResult{
		Reason: payment.ReasonPaymentPending, Retryable: true,
	})
	if err == nil || err.Error() != payment.ReasonPaymentPending || errors.Is(err, errPaymentBlockPersisted) {
		t.Fatalf("retryable payment error = %v", err)
	}
	if err := retryablePaymentError(payment.AuthorizeResult{Reason: "no_payment"}); err != nil {
		t.Fatalf("terminal denial unexpectedly used retry path: %v", err)
	}

	ctx := context.Background()
	store := mustStore(t, ctx)
	if acquired, err := store.BeginReview(ctx, "payment-patch", "repo-1"); err != nil || !acquired {
		t.Fatalf("BeginReview: acquired=%v err=%v", acquired, err)
	}
	if err := store.MarkReviewFailed(ctx, "payment-patch", "repo-1", payment.ReasonPaymentPending); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE review_log SET updated_at=0 WHERE patch_event_id='payment-patch'`); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.RequeueFailedReviews(ctx, 0, 10)
	if err != nil || len(tasks) != 1 || tasks[0].PatchEventID != "payment-patch" {
		t.Fatalf("payment_pending was not requeued: tasks=%+v err=%v", tasks, err)
	}
}

type mutableMonitoringRegistry struct {
	allowed bool
}

func (r *mutableMonitoringRegistry) Contains(string) bool { return r.allowed }

func TestReactiveMonitoringGateFailsClosedAndRechecksBeforePublication(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	owner := nostr.GetPublicKey(nostr.Generate()).Hex()
	repoID := owner + ":repo"
	task := db.ReviewTask{
		PatchEventID: "reactive-patch",
		RepoID:       repoID,
		Invocation:   db.ReviewInvocationReactive,
	}
	acquired, err := store.BeginReviewWithClaim(ctx, task.PatchEventID, task.RepoID, db.ReviewClaim{Invocation: task.Invocation})
	if err != nil || !acquired {
		t.Fatalf("claim reactive review: acquired=%v err=%v", acquired, err)
	}

	registry := &mutableMonitoringRegistry{allowed: true}
	runner := &Runner{store: store, logger: testLogger(), monitoring: registry}
	if err := runner.requireReactiveMonitoring(ctx, task, "pipeline_start"); err != nil {
		t.Fatalf("monitored review rejected at start: %v", err)
	}

	registry.allowed = false
	if err := runner.requireReactiveMonitoring(ctx, task, "pre_publication"); !errors.Is(err, errReactiveReviewSkipped) {
		t.Fatalf("removed review error = %v", err)
	}
	if reason, ok, err := store.GetReviewSkip(ctx, task.PatchEventID, task.RepoID); err != nil || !ok || reason != "monitoring_removed" {
		t.Fatalf("pre-publication durable skip = %q ok=%v err=%v", reason, ok, err)
	}

	startupTask := db.ReviewTask{
		PatchEventID: "startup-without-list",
		RepoID:       repoID,
		Invocation:   db.ReviewInvocationReactive,
	}
	acquired, err = store.BeginReviewWithClaim(ctx, startupTask.PatchEventID, startupTask.RepoID, db.ReviewClaim{Invocation: startupTask.Invocation})
	if err != nil || !acquired {
		t.Fatalf("claim startup review: acquired=%v err=%v", acquired, err)
	}
	startupRunner := &Runner{store: store, logger: testLogger()}
	if err := startupRunner.requireReactiveMonitoring(ctx, startupTask, "pipeline_start"); !errors.Is(err, errReactiveReviewSkipped) {
		t.Fatalf("startup without list error = %v", err)
	}
	if reason, ok, err := store.GetReviewSkip(ctx, startupTask.PatchEventID, startupTask.RepoID); err != nil || !ok || reason != "monitoring_removed" {
		t.Fatalf("startup durable skip = %q ok=%v err=%v", reason, ok, err)
	}

	onDemand := db.ReviewTask{
		PatchEventID: "on-demand",
		RepoID:       repoID,
		Invocation:   db.ReviewInvocationContextVM,
	}
	if err := startupRunner.requireReactiveMonitoring(ctx, onDemand, "pipeline_start"); err != nil {
		t.Fatalf("on-demand review was incorrectly monitoring-gated: %v", err)
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

	t.Run("statusPublishParameters_security_gate", func(t *testing.T) {
		cfg := repoconfig.Default()
		cfg.Status.Enabled = true
		cfg.Status.OpenSeverityFloor = "critical"
		cfg.Status.MinConfidence = 0.99
		cfg.Security.GateSeverity = "high"
		cfg.Security.MinConfidence = 0.90
		confirmed := reviewengine.Finding{Category: "security", Severity: "high", Confidence: 0.95}
		ordinary := reviewengine.Finding{Category: "correctness", Severity: "critical", Confidence: 1}
		findings, confidence, policy := statusPublishParameters([]reviewengine.Finding{ordinary, confirmed}, []reviewengine.Finding{confirmed}, cfg)
		if len(findings) != 1 || findings[0] != confirmed {
			t.Fatalf("status findings = %#v, want confirmed security finding only", findings)
		}
		if confidence != 0.95 || policy.OpenSeverityFloor != "high" || policy.MinConfidence != 0.90 || !policy.Enabled {
			t.Fatalf("unexpected security status parameters: confidence=%v policy=%#v", confidence, policy)
		}
	})

	t.Run("statusPublishParameters_non_gating_security_remains_comment_only", func(t *testing.T) {
		cfg := repoconfig.Default()
		cfg.Status.Enabled = true
		cfg.Security.GateSeverity = "high"
		cfg.Security.MinConfidence = 0.90
		lowConfidence := reviewengine.Finding{Category: "security", Severity: "critical", Confidence: 0.89}
		lowSeverity := reviewengine.Finding{Category: "security", Severity: "medium", Confidence: 0.99}
		unverified := reviewengine.Finding{Category: "security", Severity: "critical", Confidence: 1}
		findings, _, policy := statusPublishParameters(
			[]reviewengine.Finding{lowConfidence, lowSeverity, unverified},
			[]reviewengine.Finding{lowConfidence, lowSeverity},
			cfg,
		)
		if len(findings) != 0 {
			t.Fatalf("non-gating security findings affected status: %#v", findings)
		}
		if policy.OpenSeverityFloor != cfg.Status.OpenSeverityFloor {
			t.Fatalf("policy = %#v, want ordinary status policy", policy)
		}
	})

	t.Run("statusPublishParameters_preserves_non_security_semantics", func(t *testing.T) {
		cfg := repoconfig.Default()
		cfg.Status.Enabled = true
		cfg.Status.OpenSeverityFloor = "high"
		cfg.Status.MinConfidence = 0.8
		securityComment := reviewengine.Finding{Category: "security", Severity: "critical", Confidence: 1}
		correctness := reviewengine.Finding{Category: "correctness", Severity: "high", Confidence: 0.9}
		findings, confidence, policy := statusPublishParameters([]reviewengine.Finding{securityComment, correctness}, nil, cfg)
		if len(findings) != 1 || findings[0] != correctness {
			t.Fatalf("ordinary status findings = %#v", findings)
		}
		if confidence != 0.9 || policy.OpenSeverityFloor != "high" || policy.MinConfidence != 0.8 {
			t.Fatalf("unexpected ordinary status parameters: confidence=%v policy=%#v", confidence, policy)
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
