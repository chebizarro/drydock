package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drydock/internal/agenticreview"
	"drydock/internal/agenttools"
	"drydock/internal/betterleaks"
	"drydock/internal/contextbuilder"
	"drydock/internal/db"
	"drydock/internal/metareview"
	"drydock/internal/publisher"
	"drydock/internal/repo"
	"drydock/internal/reviewengine"
	"drydock/internal/reviewsession"
	"drydock/internal/securityscan"
	"drydock/internal/testutil"
	"drydock/internal/workspacesnapshot"

	"fiatjaf.com/nostr"
)

// --- Integration test helpers ---

// testSigner signs events with a deterministic key for testing.
type testSigner struct {
	sk nostr.SecretKey
}

func (s testSigner) GetPublicKey(_ context.Context) (nostr.PubKey, error) {
	return nostr.GetPublicKey(s.sk), nil
}
func (s testSigner) SignEvent(_ context.Context, evt *nostr.Event) error {
	return evt.Sign(s.sk)
}

// collectingRelayPublisher captures published events instead of sending them.
type collectingRelayPublisher struct {
	events []nostr.Event
	relays [][]string
}

func (p *collectingRelayPublisher) Publish(_ context.Context, relays []string, event nostr.Event) error {
	p.events = append(p.events, event)
	p.relays = append(p.relays, relays)
	return nil
}

// gitRun runs a git command in the given directory.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// initRepoInCanonicalCache creates a git repo with an initial commit containing main.go
// directly inside the canonical repo cache directory that Service.PreparePatchSeries uses.
// This avoids needing a network-reachable clone URL in tests.
func initRepoInCanonicalCache(t *testing.T, cacheDir, repoID string) string {
	t.Helper()
	// Replicate Manager.canonicalRepoPath() for tests without exporting production internals.
	sum := sha256.Sum256([]byte("canonical\x00" + repoID))
	repoPath := filepath.Join(cacheDir, hex.EncodeToString(sum[:]))

	os.MkdirAll(repoPath, 0o755)
	gitRun(t, repoPath, "init", "-b", "master")
	os.WriteFile(filepath.Join(repoPath, "main.go"), []byte("package main\n"), 0o644)
	gitRun(t, repoPath, "add", "main.go")
	gitRun(t, repoPath, "commit", "-m", "initial")
	// Production cache entries always have an origin; use the repository itself
	// so fetch --all remains local in integration tests.
	gitRun(t, repoPath, "remote", "add", "origin", ".")

	return repoPath
}

// makePatchDiff returns a valid unified diff that adds a comment to main.go.
func makePatchDiff() string {
	return "diff --git a/main.go b/main.go\n" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1 +1,2 @@\n" +
		" package main\n" +
		"+// reviewed\n"
}

// seedIntegrationDB creates DB entries for a repo announcement and patch event.
// Returns the patch event ID and repo ID.
func seedIntegrationDB(t *testing.T, ctx context.Context, store *db.Store) (patchEventID, repoID string) {
	t.Helper()
	return seedIntegrationDBWithDiff(t, ctx, store, makePatchDiff())
}

func seedIntegrationDBWithDiff(t *testing.T, ctx context.Context, store *db.Store, diff string) (patchEventID, repoID string) {
	t.Helper()

	repoSK := nostr.Generate()
	patchSK := nostr.Generate()

	// Use a dummy https URL — the repo is pre-cloned into the cache so
	// EnsureRepo will find the .git dir and just run fetch.
	repoEvt := nostr.Event{
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "integ-repo"},
			{"clone", "https://example.com/integ-repo.git"},
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
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":integ-repo"},
			{"e", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "", "root"},
		},
		Content: diff,
	}
	patchEvt.Sign(patchSK)
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

type betterleaksPipelineFixture struct {
	runner            *Runner
	fakeLLM           *testutil.FakeLLM
	relayPub          *collectingRelayPublisher
	task              db.ReviewTask
	baseCommit        string
	canonicalRepoPath string
}

func newBetterleaksPipelineFixture(
	t *testing.T,
	secretScan bool,
	scanner BetterleaksScanner,
) betterleaksPipelineFixture {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "betterleaks-pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	patchID, repoID := seedIntegrationDB(t, ctx, store)
	cacheDir := filepath.Join(t.TempDir(), "repos")
	canonicalRepoPath := initRepoInCanonicalCache(t, cacheDir, repoID)
	if secretScan {
		config := []byte(`security:
  secret_scan: true
`)
		if err := os.WriteFile(filepath.Join(canonicalRepoPath, ".drydock.yaml"), config, 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, canonicalRepoPath, "add", ".drydock.yaml")
		gitRun(t, canonicalRepoPath, "commit", "-m", "enable secret scan")
	}
	baseCmd := exec.Command("git", "rev-parse", "HEAD")
	baseCmd.Dir = canonicalRepoPath
	baseBytes, err := baseCmd.Output()
	if err != nil {
		t.Fatalf("resolve base commit: %v", err)
	}
	baseCommit := strings.TrimSpace(string(baseBytes))

	repoSvc := repo.NewService(store, repo.NewManager(cacheDir, logger), logger)
	ctxBuilder := contextbuilder.NewWithOptions(contextbuilder.NewBuilderOptions(
		contextbuilder.WithExtraProviders(betterleaks.NewProvider()),
	))
	fakeLLM := &testutil.FakeLLM{Responses: []string{
		`{"change_type":"bugfix","risk_areas":["security"],"needed_context":[],"review_focus":"secrets","model_route":"coder32b"}`,
		`{"summary":"Secret review completed.","findings":[{"severity":"high","category":"correctness","file":"main.go","line":2,"evidence":"RAW_LLM_SECRET","explanation":"RAW_LLM_DESCRIPTION","suggestion":"RAW_LLM_SUGGESTION","confidence":0.85}],"needs_more_context":[]}`,
		`{"walkthrough":"The patch adds a reviewed marker.","file_summaries":[{"file":"main.go","summary":"Adds a comment"}]}`,
	}}
	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "planner"},
		Coder32B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder32b"},
		LLM70B:   reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "llm70b"},
		Coder14B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder14b"},
	}, fakeLLM, logger)
	relayPub := &collectingRelayPublisher{}
	pubSvc := publisher.New(publisher.Config{
		DefaultRelays: []string{"wss://relay.test"}, DetailSeverityFloor: "high",
		DefaultTTL: 90 * 24 * time.Hour, SupersededTTL: 7 * 24 * time.Hour,
	}, store, testSigner{sk: nostr.Generate()}, relayPub, logger)

	opts := []func(*Runner){WithMonitoringRegistry(allowAllRegistry{})}
	if scanner != nil {
		opts = append(opts, WithBetterleaksScanner(scanner))
	}
	runner := New(
		Config{Workers: 1, AgenticReviewFallback: true},
		store, repoSvc, ctxBuilder, engine, pubSvc, nil,
		make(chan db.ReviewTask, 1), logger, opts...,
	)
	return betterleaksPipelineFixture{
		runner: runner, fakeLLM: fakeLLM, relayPub: relayPub,
		task:       db.ReviewTask{PatchEventID: patchID, RepoID: repoID},
		baseCommit: baseCommit, canonicalRepoPath: canonicalRepoPath,
	}
}

// --- Integration tests ---

func TestIntegrationBetterleaksScansOnceBuildsContextAndPublishesSanitizedFinding(t *testing.T) {
	scanner := &recordingBetterleaksScanner{
		result: betterleaks.ScanResult{Findings: []securityscan.SecurityFinding{sensitiveScannerFinding()}},
	}
	fixture := newBetterleaksPipelineFixture(t, true, scanner)
	if err := fixture.runner.process(context.Background(), fixture.task); err != nil {
		t.Fatalf("process failed: %v", err)
	}

	if scanner.calls != 1 || len(scanner.requests) != 1 {
		t.Fatalf("betterleaks calls = %d, requests = %d; want exactly one", scanner.calls, len(scanner.requests))
	}
	req := scanner.requests[0]
	if strings.TrimSpace(req.RepoPath) == "" {
		t.Fatal("scanner did not receive the prepared repository path")
	}
	if req.RepoPath == fixture.canonicalRepoPath {
		t.Fatal("scanner received the mutable canonical cache instead of the prepared review checkout")
	}
	if req.PolicyRef != fixture.baseCommit {
		t.Fatalf("PolicyRef = %q, want base commit %q", req.PolicyRef, fixture.baseCommit)
	}
	if len(req.AllowedFiles) != 1 || req.AllowedFiles[0] != "main.go" {
		t.Fatalf("AllowedFiles = %#v, want authoritative main.go allowlist", req.AllowedFiles)
	}
	if strings.TrimSpace(req.Diff) != strings.TrimSpace(makePatchDiff()) {
		t.Fatalf("Diff was not the authoritative filtered patch: %q", req.Diff)
	}
	if len(fixture.fakeLLM.Requests) != 3 {
		t.Fatalf("LLM calls = %d, want 3", len(fixture.fakeLLM.Requests))
	}
	for i, request := range fixture.fakeLLM.Requests[:2] {
		if !strings.Contains(request.User, "BETTERLEAKS SECRET SCAN: 1 potential secret(s) detected.") {
			t.Fatalf("LLM request %d missing redacted secret-scan context: %s", i, request.User)
		}
		if strings.Contains(request.User, "RAW_SCANNER_SECRET") ||
			strings.Contains(request.User, "RAW_SCANNER_DESCRIPTION") ||
			strings.Contains(request.User, "RAW_SCANNER_SUGGESTION") {
			t.Fatalf("LLM request %d leaked scanner text: %s", i, request.User)
		}
	}

	var published strings.Builder
	for _, event := range fixture.relayPub.events {
		published.WriteString(event.Content)
	}
	output := published.String()
	for _, secretText := range []string{
		"RAW_SCANNER_SECRET", "RAW_SCANNER_DESCRIPTION", "RAW_SCANNER_SUGGESTION",
		"RAW_LLM_SECRET", "RAW_LLM_DESCRIPTION", "RAW_LLM_SUGGESTION",
	} {
		if strings.Contains(output, secretText) {
			t.Fatalf("published output leaked %q: %s", secretText, output)
		}
	}
	if !strings.Contains(output, "[REDACTED: sensitive finding]") {
		t.Fatalf("published output did not sanitize merged sensitive finding: %s", output)
	}
}

func TestIntegrationBetterleaksDisabledNeverInvokesScanner(t *testing.T) {
	scanner := &recordingBetterleaksScanner{err: errors.New("must not run")}
	fixture := newBetterleaksPipelineFixture(t, false, scanner)
	if err := fixture.runner.process(context.Background(), fixture.task); err != nil {
		t.Fatalf("process failed with secret scanning disabled: %v", err)
	}
	if scanner.calls != 0 {
		t.Fatalf("betterleaks calls = %d, want 0 when disabled", scanner.calls)
	}
}

func TestIntegrationBetterleaksFailureStopsBeforeLLM(t *testing.T) {
	scanner := &recordingBetterleaksScanner{err: errors.New("scanner exploded")}
	fixture := newBetterleaksPipelineFixture(t, true, scanner)
	err := fixture.runner.process(context.Background(), fixture.task)
	if err == nil || !strings.Contains(err.Error(), "betterleaks secret scan") ||
		!strings.Contains(err.Error(), "scanner exploded") {
		t.Fatalf("process error = %v, want fail-closed scanner error", err)
	}
	if scanner.calls != 1 {
		t.Fatalf("betterleaks calls = %d, want 1", scanner.calls)
	}
	if len(fixture.fakeLLM.Requests) != 0 {
		t.Fatalf("LLM calls = %d after scanner failure, want 0", len(fixture.fakeLLM.Requests))
	}
	if len(fixture.relayPub.events) != 0 {
		t.Fatalf("published %d events after scanner failure, want 0", len(fixture.relayPub.events))
	}
}

func TestIntegrationBetterleaksEnabledWithoutScannerIsConfigurationError(t *testing.T) {
	fixture := newBetterleaksPipelineFixture(t, true, nil)
	err := fixture.runner.process(context.Background(), fixture.task)
	if err == nil || !strings.Contains(err.Error(), "configuration error") ||
		!strings.Contains(err.Error(), "no betterleaks scanner") {
		t.Fatalf("process error = %v, want missing-scanner configuration error", err)
	}
	if len(fixture.fakeLLM.Requests) != 0 {
		t.Fatalf("LLM calls = %d with missing scanner, want 0", len(fixture.fakeLLM.Requests))
	}
}

type pipelineTokenCounter struct{}

func (pipelineTokenCounter) Count(text string) int { return len(text) }

func pipelineToolCompletion(t *testing.T, id, name string, arguments any) reviewengine.CompletionResult {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	return reviewengine.CompletionResult{
		Message: reviewengine.CompletionMessage{
			Role: reviewengine.MessageRoleAssistant,
			ToolCalls: []reviewengine.ToolCall{{
				ID: id, Type: "function",
				Function: reviewengine.ToolCallFunction{Name: name, Arguments: string(encoded)},
			}},
		},
		Usage: reviewengine.CompletionUsage{TotalTokens: 1},
	}
}

func TestIntegrationAgenticPipelineUsesCanonicalFrozenSnapshot(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "agentic-pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	patchID, repoID := seedIntegrationDBWithDiff(t, ctx, store, testutil.AgenticFixturePatch())
	cacheDir := filepath.Join(t.TempDir(), "repos")
	initRepoInCanonicalCache(t, cacheDir, repoID)
	repoSvc := repo.NewService(store, repo.NewManager(cacheDir, logger), logger)

	submission := map[string]any{
		"summary": "agentic pipeline finding",
		"findings": []any{map[string]any{
			"priority": "P1", "category": "correctness", "file": "main.go", "line": 2,
			"explanation": "verified through frozen code.read", "suggestion": "fix it", "confidence": 0.9,
			"evidence_tool_call_ids": []string{"pipeline-read"},
		}},
		"coverage": map[string]any{
			"examined_files": []string{"main.go"}, "outcome": "findings", "summary": "read canonical fixture",
		},
	}
	client := &testutil.ScriptedAgenticClient{
		ChatResults: []reviewengine.ChatResult{
			{Content: `{"change_type":"feature","risk_areas":[],"needed_context":[],"review_focus":"correctness","model_route":"coder32b"}`},
			{Content: `{"walkthrough":"agentic walkthrough","file_summaries":[{"file":"main.go","summary":"changed"}]}`},
		},
		Steps: []testutil.CompletionStep{
			{Model: "discovery", Result: pipelineToolCompletion(t, "pipeline-finalize", agenttools.ToolSelectionFinalize, map[string]any{})},
			{Model: "coder32b", Result: pipelineToolCompletion(t, "pipeline-read", agenttools.ToolCodeRead, map[string]any{"path": "main.go"})},
			{Model: "coder32b", Result: pipelineToolCompletion(t, "pipeline-submit", agenttools.ToolReviewSubmit, submission)},
		},
	}
	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "planner", Model: "planner"},
		Coder32B: reviewengine.ModelEndpoint{BaseURL: "coder", Model: "coder32b"},
		LLM70B:   reviewengine.ModelEndpoint{BaseURL: "large", Model: "llm70b"},
	}, client, logger)
	sessionStore, err := reviewsession.NewSQLiteStore(store.DB(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := workspacesnapshot.NewManager(workspacesnapshot.Config{
		StorageRoot: t.TempDir(), LeaseTTL: time.Hour, SessionLifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := agenttools.NewRegistry()
	discovery, err := agenticreview.NewDiscovery(agenticreview.DiscoveryConfig{
		Client: client, Registry: registry, Counter: pipelineTokenCounter{},
		Model: "discovery", TokenBudget: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := agenticreview.NewService(agenticreview.ServiceConfig{
		Snapshots: manager, Sessions: sessionStore, Discovery: discovery, Engine: engine,
		Client: client, Registry: registry, Counter: pipelineTokenCounter{},
	})
	if err != nil {
		t.Fatal(err)
	}
	relayPub := &collectingRelayPublisher{}
	pubSvc := publisher.New(publisher.Config{
		DefaultRelays: []string{"wss://relay.test"}, DetailSeverityFloor: "high",
		DefaultTTL: 90 * 24 * time.Hour, SupersededTTL: 7 * 24 * time.Hour,
	}, store, testSigner{sk: nostr.Generate()}, relayPub, logger)
	runner := New(Config{Workers: 1}, store, repoSvc, contextbuilder.NewDefault(), engine,
		pubSvc, nil, make(chan db.ReviewTask, 1), logger,
		WithAgenticReviewService(service), WithMonitoringRegistry(allowAllRegistry{}))
	if err := runner.process(ctx, db.ReviewTask{PatchEventID: patchID, RepoID: repoID}); err != nil {
		t.Fatal(err)
	}
	if len(relayPub.events) == 0 || !strings.Contains(relayPub.events[0].Content, "agentic pipeline finding") {
		t.Fatalf("agentic review was not published: %#v", relayPub.events)
	}
	requests := client.RequestsForModel("coder32b")
	if len(requests) != 2 {
		t.Fatalf("reviewer Complete requests = %d", len(requests))
	}
	var readResult agenttools.Result
	for _, message := range requests[1].Messages {
		if message.Role == reviewengine.MessageRoleTool && message.ToolCallID == "pipeline-read" {
			if err := json.Unmarshal([]byte(message.Content), &readResult); err != nil {
				t.Fatal(err)
			}
		}
	}
	if readResult.Content != testutil.AgenticFixtureBaseFiles()["main.go"] {
		t.Fatalf("pipeline code.read frozen bytes = %q", readResult.Content)
	}
	if client.RemainingSteps() != 0 {
		t.Fatalf("unconsumed completion steps = %d", client.RemainingSteps())
	}
}

func TestIntegrationFullPipelineProcess(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// 1. Real DB
	dbPath := filepath.Join(t.TempDir(), "integ.db")
	store, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	patchID, repoID := seedIntegrationDB(t, ctx, store)

	// 2. Pre-clone repo into cache so EnsureRepo finds it (avoids clone URL issues)
	cacheDir := filepath.Join(t.TempDir(), "repos")
	initRepoInCanonicalCache(t, cacheDir, repoID)

	// 3. Real repo service
	repoMgr := repo.NewManager(cacheDir, logger)
	repoSvc := repo.NewService(store, repoMgr, logger)

	// 4. Real context builder (no optional services)
	ctxBuilder := contextbuilder.NewDefault()

	// 5. Fake LLM that returns planner + reviewer + walkthrough responses.
	// The assertions below verify the real pipeline assembled context and invoked each step.
	fakeLLM := &testutil.FakeLLM{
		Responses: []string{
			`{"change_type":"feature","risk_areas":["correctness"],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`,
			`{"summary":"Code looks clean.","findings":[{"severity":"info","category":"style","file":"main.go","line":2,"evidence":"comment","explanation":"trivial comment","suggestion":"remove","confidence":0.95}],"needs_more_context":[]}`,
			`{"walkthrough":"This patch adds a hello world program.","file_summaries":[{"file":"main.go","summary":"New main function printing hello"}]}`,
		},
	}
	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "planner"},
		Coder32B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder32b"},
		LLM70B:   reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "llm70b"},
		Coder14B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder14b"},
	}, fakeLLM, logger)

	// 6. Real publisher with fake signer and collecting relay publisher
	signerKey := nostr.Generate()
	relayPub := &collectingRelayPublisher{}
	pubSvc := publisher.New(publisher.Config{
		DefaultRelays:       []string{"wss://relay.test"},
		DetailSeverityFloor: "high",
		DefaultTTL:          90 * 24 * time.Hour,
		SupersededTTL:       7 * 24 * time.Hour,
	}, store, testSigner{sk: signerKey}, relayPub, logger)

	// 7. Real meta-review service (with fake LLM — won't trigger for this test)
	metaLLM := &reviewengine.FakeLLMForTest{
		Responses: []string{
			`{"missed_findings":[],"false_positives":[],"reasoning_quality":0.9,"context_utilization":0.8,"prompt_gaps":[],"suggested_few_shot":false}`,
		},
	}
	metaSvc := metareview.New(metareview.Config{
		Endpoint:         reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "meta"},
		RandomSampleRate: 0, // disable random triggering
		MaxConcurrent:    1,
	}, store, metaLLM, logger)

	// 8. Build and run pipeline
	queue := make(chan db.ReviewTask, 1)
	runner := New(
		Config{Workers: 1, AgenticReviewFallback: true},
		store, repoSvc, ctxBuilder, engine, pubSvc, metaSvc,
		queue, logger,
		WithMonitoringRegistry(allowAllRegistry{}),
	)

	// Process the review task directly
	err = runner.process(ctx, db.ReviewTask{
		PatchEventID: patchID,
		RepoID:       repoID,
	})
	if err != nil {
		t.Fatalf("process failed: %v", err)
	}

	// --- Assertions ---

	// Should have published at least the summary event
	if len(relayPub.events) == 0 {
		t.Fatal("expected at least one published event")
	}

	summaryEvt := relayPub.events[0]

	// Kind should be 1111 (NIP-22 comment)
	if summaryEvt.Kind != nostr.KindComment {
		t.Fatalf("expected kind %d, got %d", nostr.KindComment, summaryEvt.Kind)
	}

	// Content should contain the summary
	if !strings.Contains(summaryEvt.Content, "Code looks clean") {
		t.Fatalf("expected summary in content, got: %s", summaryEvt.Content)
	}

	// Footer should contain metadata fields
	if !strings.Contains(summaryEvt.Content, "model:") {
		t.Fatal("missing model footer field")
	}
	if !strings.Contains(summaryEvt.Content, "context-hash:") {
		t.Fatal("missing context-hash footer field")
	}
	if !strings.Contains(summaryEvt.Content, "excluded-files:") {
		t.Fatal("missing excluded-files footer field")
	}

	// Should have correct tags (root scope)
	hasTag := func(name string) bool {
		for _, tag := range summaryEvt.Tags {
			if len(tag) > 0 && tag[0] == name {
				return true
			}
		}
		return false
	}
	if !hasTag("E") {
		t.Fatal("missing E root tag")
	}
	if !hasTag("K") {
		t.Fatal("missing K root kind tag")
	}

	// Relays should include the relay from the repo/patch
	if len(relayPub.relays) == 0 {
		t.Fatal("no relay lists recorded")
	}
	relayFound := false
	for _, r := range relayPub.relays[0] {
		if r == "wss://relay.test" {
			relayFound = true
		}
	}
	if !relayFound {
		t.Fatalf("expected wss://relay.test in relay list, got: %v", relayPub.relays[0])
	}

	// LLM should have received exactly 3 calls (planner + reviewer + walkthrough), and the
	// assembled context should flow into the prompts rather than bypassing the engine.
	if len(fakeLLM.Requests) != 3 {
		t.Fatalf("expected 3 LLM calls (planner + reviewer + walkthrough), got %d", len(fakeLLM.Requests))
	}
	if fakeLLM.Requests[0].Model != "planner" || fakeLLM.Requests[1].Model != "coder32b" || fakeLLM.Requests[2].Model != "planner" {
		t.Fatalf("unexpected LLM call sequence/models: %#v", []string{fakeLLM.Requests[0].Model, fakeLLM.Requests[1].Model, fakeLLM.Requests[2].Model})
	}
	for i, req := range fakeLLM.Requests {
		if !req.JSONMode {
			t.Fatalf("request %d did not enable JSON mode", i)
		}
	}
	if !strings.Contains(fakeLLM.Requests[0].User, "main.go") || !strings.Contains(fakeLLM.Requests[0].User, "+// reviewed") {
		t.Fatalf("planner prompt did not include assembled patch context: %s", fakeLLM.Requests[0].User)
	}
	if !strings.Contains(fakeLLM.Requests[1].User, "Review focus: logic") || !strings.Contains(fakeLLM.Requests[1].User, "+// reviewed") {
		t.Fatalf("reviewer prompt did not include planner output and context: %s", fakeLLM.Requests[1].User)
	}

	// Review should be marked as published in the DB
	status, err := store.GetReviewStatus(ctx, patchID, repoID)
	if err != nil {
		t.Fatalf("get review status: %v", err)
	}
	if status != "published" {
		t.Fatalf("expected review status 'published', got %q", status)
	}
}

func TestIntegrationMalformedReviewerJSONIsRepairedAndPublished(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	dbPath := filepath.Join(t.TempDir(), "repair.db")
	store, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	patchID, repoID := seedIntegrationDB(t, ctx, store)
	cacheDir := filepath.Join(t.TempDir(), "repos")
	initRepoInCanonicalCache(t, cacheDir, repoID)

	repoMgr := repo.NewManager(cacheDir, logger)
	repoSvc := repo.NewService(store, repoMgr, logger)
	ctxBuilder := contextbuilder.NewDefault()

	fakeLLM := &testutil.FakeLLM{
		Responses: []string{
			`{"change_type":"bugfix","risk_areas":["correctness"],"needed_context":[],"review_focus":"repair path","model_route":"coder32b"}`,
			`{"summary":"broken reviewer payload","findings":[{"severity":"severe","category":"correctness","file":"main.go","line":2,"evidence":"comment","explanation":"this should be repaired","suggestion":"fix it","confidence":1.7}],"needs_more_context":[]}`,
			`{"summary":"Repaired review summary","findings":[{"severity":"high","category":"correctness","file":"main.go","line":2,"evidence":"comment added by patch","explanation":"Repaired explanation that should be published.","suggestion":"Keep behavior intentional.","confidence":0.88}],"needs_more_context":[]}`,
			`{"walkthrough":"The patch adds a review marker comment.","file_summaries":[{"file":"main.go","summary":"Adds a comment below the package declaration"}]}`,
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

	runner := New(Config{Workers: 1, AgenticReviewFallback: true}, store, repoSvc, ctxBuilder, engine, pubSvc, nil, make(chan db.ReviewTask), logger, WithMonitoringRegistry(allowAllRegistry{}))
	if err := runner.process(ctx, db.ReviewTask{PatchEventID: patchID, RepoID: repoID}); err != nil {
		t.Fatalf("process failed: %v", err)
	}

	if len(fakeLLM.Requests) != 4 {
		t.Fatalf("expected planner, reviewer, reviewer repair, walkthrough calls; got %d", len(fakeLLM.Requests))
	}
	if !strings.Contains(fakeLLM.Requests[2].System, "repair malformed") || !strings.Contains(fakeLLM.Requests[2].User, "severe") {
		t.Fatalf("third LLM call was not a reviewer repair request: system=%q user=%q", fakeLLM.Requests[2].System, fakeLLM.Requests[2].User)
	}
	if len(relayPub.events) < 2 {
		t.Fatalf("expected summary and detail events from repaired high finding, got %d", len(relayPub.events))
	}
	if !strings.Contains(relayPub.events[0].Content, "Repaired review summary") {
		t.Fatalf("summary event did not contain repaired review summary: %s", relayPub.events[0].Content)
	}
	if strings.Contains(relayPub.events[0].Content, "broken reviewer payload") || strings.Contains(relayPub.events[0].Content, "severe") {
		t.Fatalf("summary event leaked unrepaired reviewer payload: %s", relayPub.events[0].Content)
	}
	if !strings.Contains(relayPub.events[1].Content, "Repaired explanation") {
		t.Fatalf("detail event did not contain repaired finding: %s", relayPub.events[1].Content)
	}

	status, err := store.GetReviewStatus(ctx, patchID, repoID)
	if err != nil {
		t.Fatalf("get review status: %v", err)
	}
	if status != "published" {
		t.Fatalf("expected review status 'published', got %q", status)
	}
}

func TestIntegrationApplyFailurePublishesOperationalNotice(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// 1. Real DB
	dbPath := filepath.Join(t.TempDir(), "integ-fail.db")
	store, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Seed with a patch that won't apply (references non-existent context lines)
	repoSK := nostr.Generate()
	patchSK := nostr.Generate()

	repoEvt := nostr.Event{
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "fail-repo"},
			{"clone", "https://example.com/fail-repo.git"},
			{"relays", "wss://relay.test"},
		},
	}
	repoEvt.Sign(repoSK)
	store.UpsertRepositoryAnnouncement(ctx, repoEvt)

	badDiff := "diff --git a/nonexistent.go b/nonexistent.go\n" +
		"--- a/nonexistent.go\n" +
		"+++ b/nonexistent.go\n" +
		"@@ -1,3 +1,4 @@\n" +
		" package old\n" +
		" func Existing() {}\n" +
		" func Other() {}\n" +
		"+func New() {}\n"

	patchEvt := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":fail-repo"},
			{"e", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "", "root"},
		},
		Content: badDiff,
	}
	patchEvt.Sign(patchSK)
	store.InsertPatchEvent(ctx, patchEvt)
	store.RecordPatchEventRelay(ctx, patchEvt.ID.Hex(), "wss://relay.test")

	rID := db.RepoIDFromPatch(patchEvt)
	store.BeginReview(ctx, patchEvt.ID.Hex(), rID)

	// 2. Pre-clone repo into cache with only main.go — the bad diff won't apply
	cacheDir := filepath.Join(t.TempDir(), "repos")
	initRepoInCanonicalCache(t, cacheDir, rID)

	// 3. Build pipeline
	repoMgr := repo.NewManager(cacheDir, logger)
	repoSvc := repo.NewService(store, repoMgr, logger)
	ctxBuilder := contextbuilder.NewDefault()

	fakeLLM := &testutil.FakeLLM{}
	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "p"},
		Coder32B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "c"},
		LLM70B:   reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "l"},
		Coder14B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "s"},
	}, fakeLLM, logger)

	signerKey := nostr.Generate()
	relayPub := &collectingRelayPublisher{}
	pubSvc := publisher.New(publisher.Config{
		DefaultRelays:       []string{"wss://relay.test"},
		DetailSeverityFloor: "high",
		DefaultTTL:          90 * 24 * time.Hour,
		SupersededTTL:       7 * 24 * time.Hour,
	}, store, testSigner{sk: signerKey}, relayPub, logger)

	queue := make(chan db.ReviewTask, 1)
	runner := New(
		Config{Workers: 1, AgenticReviewFallback: true},
		store, repoSvc, ctxBuilder, engine, pubSvc, nil,
		queue, logger,
		WithMonitoringRegistry(allowAllRegistry{}),
	)

	// process should return an error (patch doesn't apply)
	err = runner.process(ctx, db.ReviewTask{
		PatchEventID: patchEvt.ID.Hex(),
		RepoID:       rID,
	})
	if err == nil {
		t.Fatal("expected process to fail for unapplyable patch")
	}

	// It publishes a distinct operational notice, never an ordinary review.
	if len(relayPub.events) == 0 {
		t.Fatal("expected apply-failure operational notice to be published")
	}
	if !strings.Contains(relayPub.events[0].Content, "review not performed") ||
		!strings.Contains(relayPub.events[0].Content, "does not apply cleanly") {
		t.Fatalf("expected apply-failure hint in content, got: %s", relayPub.events[0].Content)
	}
	if strings.Contains(relayPub.events[0].Content, "Automated review summary") ||
		strings.Contains(relayPub.events[0].Content, "model: none") {
		t.Fatalf("apply failure was formatted as a review: %s", relayPub.events[0].Content)
	}
	noticeType := relayPub.events[0].Tags.Find("drydock-type")
	if noticeType == nil || len(noticeType) < 2 || noticeType[1] != publisher.FailureNoticeType {
		t.Fatalf("missing operational notice tag: %v", noticeType)
	}
	if reviewID, err := store.GetReviewEventID(ctx, patchEvt.ID.Hex(), rID); err != nil || reviewID != "" {
		t.Fatalf("failure notice reserved an ordinary review id: id=%q err=%v", reviewID, err)
	}

	// LLM should NOT have been called (patch didn't apply)
	if len(fakeLLM.Requests) != 0 {
		t.Fatalf("expected 0 LLM calls for apply failure, got %d", len(fakeLLM.Requests))
	}
}
