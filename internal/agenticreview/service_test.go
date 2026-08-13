package agenticreview

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"drydock/internal/agenttools"
	"drydock/internal/db"
	"drydock/internal/reviewengine"
	"drydock/internal/reviewsession"
	"drydock/internal/testutil"
	"drydock/internal/workspacesnapshot"
)

type serviceClient struct {
	mu       sync.Mutex
	results  []reviewengine.CompletionResult
	complete int
}

func (c *serviceClient) ChatCompletion(context.Context, reviewengine.ChatRequest) (reviewengine.ChatResult, error) {
	return reviewengine.ChatResult{Content: `{"change_type":"feature","risk_areas":[],"needed_context":[],"review_focus":"correctness","model_route":"coder32b"}`}, nil
}

func (c *serviceClient) Complete(_ context.Context, _ reviewengine.CompletionRequest) (reviewengine.CompletionResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := c.results[c.complete]
	c.complete++
	return result, nil
}

func serviceToolCall(t *testing.T, id, name string, arguments any) reviewengine.ToolCall {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	return reviewengine.ToolCall{ID: id, Type: "function", Function: reviewengine.ToolCallFunction{
		Name: name, Arguments: string(encoded),
	}}
}

func serviceCompletion(calls ...reviewengine.ToolCall) reviewengine.CompletionResult {
	return reviewengine.CompletionResult{
		Message: reviewengine.CompletionMessage{Role: reviewengine.MessageRoleAssistant, ToolCalls: calls},
		Usage:   reviewengine.CompletionUsage{TotalTokens: 1},
	}
}

func TestServicePrepareStartContinueAndReplay(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	if err := osWrite(filepath.Join(workspace, "changed.go"), "new\nkeep\n"); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sessionStore, err := reviewsession.NewSQLiteStore(database.DB(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	snapshotStorage := t.TempDir()
	managerConfig := workspacesnapshot.Config{
		StorageRoot: snapshotStorage, LeaseTTL: time.Hour, SessionLifetime: time.Hour,
	}
	manager, err := workspacesnapshot.NewManager(managerConfig)
	if err != nil {
		t.Fatal(err)
	}
	client := &serviceClient{results: []reviewengine.CompletionResult{
		serviceCompletion(serviceToolCall(t, "finalize", agenttools.ToolSelectionFinalize, map[string]any{})),
		serviceCompletion(serviceToolCall(t, "read-1", agenttools.ToolCodeRead, map[string]any{
			"path": "changed.go", "start_line": 1, "end_line": 2,
		})),
		serviceCompletion(serviceToolCall(t, "submit-1", agenttools.ToolReviewSubmit, noFindingSubmission("no_findings"))),
		serviceCompletion(serviceToolCall(t, "read-2", agenttools.ToolCodeRead, map[string]any{
			"path": "changed.go", "start_line": 1, "end_line": 2,
		})),
		serviceCompletion(serviceToolCall(t, "submit-2", agenttools.ToolReviewSubmit, noFindingSubmission("no_findings"))),
	}}
	registry := agenttools.NewRegistry()
	discovery, err := NewDiscovery(DiscoveryConfig{
		Client: client, Registry: registry, Counter: testCounter{}, TokenBudget: 100_000,
		Limits: LoopLimits{MaxTurns: 4, MaxToolCalls: 8, MaxCumulativeTokens: 1_000_000, MaxModelContext: 1_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "planner", Model: "planner"},
		Coder32B: reviewengine.ModelEndpoint{BaseURL: "reviewer", Model: "reviewer"},
	}, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	service, err := NewService(ServiceConfig{
		Snapshots: manager, Sessions: sessionStore, Discovery: discovery,
		Engine: engine, Client: client, Registry: registry, Counter: testCounter{},
		ReviewerLimits: LoopLimits{MaxTurns: 4, MaxToolCalls: 8, MaxCumulativeTokens: 1_000_000, MaxModelContext: 1_000_000},
		HistoryTokens:  100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := service.Prepare(ctx, PrepareInput{
		Mode: reviewsession.ModeInlinePatch,
		Snapshot: SnapshotSpec{
			Kind: workspacesnapshot.KindMutableCopy, WorkspacePath: workspace,
			Allowlist: []string{"."}, TTL: time.Hour,
		},
		Patch: reviewerPatch(),
		Target: TargetInput{
			RepoID: "repo", RootID: "root", PatchEventID: "patch",
			CanonicalRemoteIdentity: "remote",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.SnapshotHandle().ID == "" || prepared.Bundle().Content == "" {
		t.Fatal("prepare did not return finalized bundle and snapshot handle")
	}
	owner := reviewsession.Owner{Kind: "nostr", ID: "owner"}
	start, err := service.StartSession(ctx, StartSessionInput{
		Prepared: prepared, Owner: owner, RequestID: "start",
		Options: ReviewOptions{SkipWalkthrough: true}, Lifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if start.ChatID == "" || start.Version != 0 {
		t.Fatalf("start = %+v", start)
	}
	restartedManager, err := workspacesnapshot.NewManager(managerConfig)
	if err != nil {
		t.Fatal(err)
	}
	service, err = NewService(ServiceConfig{
		Snapshots: restartedManager, Sessions: sessionStore, Discovery: discovery,
		Engine: engine, Client: client, Registry: registry, Counter: testCounter{},
		ReviewerLimits: LoopLimits{MaxTurns: 4, MaxToolCalls: 8, MaxCumulativeTokens: 1_000_000, MaxModelContext: 1_000_000},
		HistoryTokens:  100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeRejected := client.complete
	_, err = service.Continue(ctx, ContinueInput{
		ChatID: start.ChatID, Owner: owner, RequestID: "future-before",
		ExpectedVersion: 4, Message: "Skip ahead.",
	})
	if !errors.Is(err, reviewsession.ErrVersionConflict) {
		t.Fatalf("future continuation error = %v", err)
	}
	if client.complete != beforeRejected {
		t.Fatal("future continuation invoked the model")
	}

	continued, err := service.Continue(ctx, ContinueInput{
		ChatID: start.ChatID, Owner: owner, RequestID: "continue-1",
		ExpectedVersion: 0, Message: "Check the frozen code again.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if continued.Version != 1 || continued.Replay {
		t.Fatalf("continue = %+v", continued)
	}
	replay, err := service.Continue(ctx, ContinueInput{
		ChatID: start.ChatID, Owner: owner, RequestID: "continue-1",
		ExpectedVersion: 0, Message: "Check the frozen code again.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replay || replay.Version != 1 {
		t.Fatalf("replay = %+v", replay)
	}
	if client.complete != 5 {
		t.Fatalf("model completion calls = %d, duplicate request reran model", client.complete)
	}
	rejectedAt := client.complete
	_, err = service.Continue(ctx, ContinueInput{
		ChatID: start.ChatID, Owner: owner, RequestID: "continue-1",
		ExpectedVersion: 0, Message: "Different payload.",
	})
	if !errors.Is(err, reviewsession.ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict error = %v", err)
	}
	for _, attempt := range []ContinueInput{
		{ChatID: start.ChatID, Owner: owner, RequestID: "stale", ExpectedVersion: 0, Message: "stale"},
		{ChatID: start.ChatID, Owner: owner, RequestID: "future", ExpectedVersion: 9, Message: "future"},
	} {
		if _, err := service.Continue(ctx, attempt); !errors.Is(err, reviewsession.ErrVersionConflict) {
			t.Errorf("version %d error = %v", attempt.ExpectedVersion, err)
		}
	}
	if client.complete != rejectedAt {
		t.Fatalf("rejected continuations invoked model: before=%d after=%d", rejectedAt, client.complete)
	}

	sessionSnapshot, err := restartedManager.Get(prepared.SnapshotHandle().ID)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := sessionSnapshot.Resolve("changed.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolved, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = service.Continue(ctx, ContinueInput{
		ChatID: start.ChatID, Owner: owner, RequestID: "after-corruption",
		ExpectedVersion: 1, Message: "continue",
	})
	if !errors.Is(err, workspacesnapshot.ErrHashMismatch) {
		t.Fatalf("corrupt snapshot continuation error = %v", err)
	}
	loadedBroken, err := sessionStore.LoadForContinuation(ctx, start.ChatID)
	if err != nil || loadedBroken.Session.State != reviewsession.StateBroken {
		t.Fatalf("session after corruption = %#v, error = %v", loadedBroken.Session, err)
	}
	// Continue verifies the corrupt snapshot before consulting persisted state,
	// so a retry currently returns the integrity error again. The session must
	// nevertheless remain broken and the model must not run.
	if _, err := service.Continue(ctx, ContinueInput{
		ChatID: start.ChatID, Owner: owner, RequestID: "broken-retry",
		ExpectedVersion: 1, Message: "retry",
	}); !errors.Is(err, workspacesnapshot.ErrHashMismatch) {
		t.Fatalf("broken session retry error = %v", err)
	}
	if client.complete != rejectedAt {
		t.Fatal("corrupt or broken continuation invoked the model")
	}
}

func TestSecurityAuditServiceValidatesFindingsAgainstSnapshotRoot(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	if err := osWrite(filepath.Join(workspace, "changed.go"), "package p\nfunc changed() {}\n"); err != nil {
		t.Fatal(err)
	}
	if err := osWrite(filepath.Join(workspace, "context.go"), "package p\nfunc vulnerable(input string) { execute(input) }\n"); err != nil {
		t.Fatal(err)
	}

	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "security-service.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sessionStore, err := reviewsession.NewSQLiteStore(database.DB(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := workspacesnapshot.NewManager(workspacesnapshot.Config{
		StorageRoot: t.TempDir(), LeaseTTL: time.Hour, SessionLifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &serviceClient{results: []reviewengine.CompletionResult{
		serviceCompletion(serviceToolCall(t, "finalize", agenttools.ToolSelectionFinalize, map[string]any{})),
		serviceCompletion(serviceToolCall(t, "context-evidence", agenttools.ToolCodeRead, map[string]any{
			"path": "context.go", "start_line": 1, "end_line": 2,
		})),
		serviceCompletion(serviceToolCall(t, "audit-submit", agenttools.ToolReviewSubmit,
			findingSubmission("context.go", 2, "context-evidence"))),
	}}
	registry := agenttools.NewRegistry()
	discovery, err := NewDiscovery(DiscoveryConfig{
		Client: client, Registry: registry, Counter: testCounter{}, TokenBudget: 100_000,
		Limits: LoopLimits{MaxTurns: 4, MaxToolCalls: 8, MaxCumulativeTokens: 1_000_000, MaxModelContext: 1_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := reviewengine.New(reviewengine.Config{
		Planner: reviewengine.ModelEndpoint{BaseURL: "planner", Model: "planner"},
		Sec70B:  reviewengine.ModelEndpoint{BaseURL: "reviewer", Model: "reviewer"},
	}, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	service, err := NewService(ServiceConfig{
		Snapshots: manager, Sessions: sessionStore, Discovery: discovery,
		Engine: engine, Client: client, Registry: registry, Counter: testCounter{},
		ReviewerLimits: LoopLimits{MaxTurns: 4, MaxToolCalls: 8, MaxCumulativeTokens: 1_000_000, MaxModelContext: 1_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := service.Prepare(ctx, PrepareInput{
		Mode: reviewsession.ModeSecurityAudit,
		Snapshot: SnapshotSpec{
			Kind: workspacesnapshot.KindMutableCopy, WorkspacePath: workspace,
			Allowlist: []string{"."}, TTL: time.Hour,
		},
		Patch: reviewerPatch(),
		Target: TargetInput{
			RepoID: "repo", RootID: "root", PatchEventID: "audit",
			CanonicalRemoteIdentity: "remote",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.ReleasePrepared(prepared)

	output, err := service.ReviewPrepared(ctx, prepared, ReviewOptions{
		ReviewerRoute: reviewengine.RouteSec70B, SkipWalkthrough: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Review.Findings) != 1 || output.Review.Findings[0].File != "context.go" {
		t.Fatalf("snapshot-root findings = %#v", output.Review.Findings)
	}
}

func TestServiceEnsembleIsolatesTranscriptsAndEvidenceLedgers(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	if err := osWrite(filepath.Join(workspace, "changed.go"), "new\nkeep\n"); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "ensemble.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sessions, err := reviewsession.NewSQLiteStore(database.DB(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := workspacesnapshot.NewManager(workspacesnapshot.Config{
		StorageRoot: t.TempDir(), LeaseTTL: time.Hour, SessionLifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &testutil.ScriptedAgenticClient{
		ChatResults: []reviewengine.ChatResult{{Content: `{"change_type":"feature","risk_areas":[],"needed_context":[],"review_focus":"correctness","model_route":"coder32b"}`}},
		Steps: []testutil.CompletionStep{
			{Model: "discovery", Result: serviceCompletion(serviceToolCall(t, "finalize", agenttools.ToolSelectionFinalize, map[string]any{}))},
			{Model: "coder32b", Result: serviceCompletion(serviceToolCall(t, "a-read", agenttools.ToolCodeRead, map[string]any{"path": "changed.go"}))},
			{Model: "coder32b", Result: serviceCompletion(serviceToolCall(t, "a-submit", agenttools.ToolReviewSubmit, findingSubmission("changed.go", 1, "a-read")))},
			{Model: "llm70b", Result: serviceCompletion(serviceToolCall(t, "b-cross-submit", agenttools.ToolReviewSubmit, findingSubmission("changed.go", 1, "a-read")))},
			{Model: "llm70b", Result: serviceCompletion(serviceToolCall(t, "b-read", agenttools.ToolCodeRead, map[string]any{"path": "changed.go"}))},
			{Model: "llm70b", Result: serviceCompletion(serviceToolCall(t, "b-submit", agenttools.ToolReviewSubmit, findingSubmission("changed.go", 1, "b-read")))},
		},
	}
	registry := agenttools.NewRegistry()
	discovery, err := NewDiscovery(DiscoveryConfig{
		Client: client, Registry: registry, Counter: testCounter{}, Model: "discovery",
		TokenBudget: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "planner", Model: "planner"},
		Coder32B: reviewengine.ModelEndpoint{BaseURL: "coder", Model: "coder32b"},
		LLM70B:   reviewengine.ModelEndpoint{BaseURL: "large", Model: "llm70b"},
	}, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	service, err := NewService(ServiceConfig{
		Snapshots: manager, Sessions: sessions, Discovery: discovery, Engine: engine,
		Client: client, Registry: registry, Counter: testCounter{},
		ReviewerLimits: LoopLimits{MaxTurns: 6, MaxToolCalls: 12, MaxCumulativeTokens: 1_000_000, MaxModelContext: 1_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := service.Prepare(ctx, PrepareInput{
		Mode:     reviewsession.ModePatch,
		Snapshot: SnapshotSpec{Kind: workspacesnapshot.KindMutableCopy, WorkspacePath: workspace, Allowlist: []string{"."}},
		Patch:    reviewerPatch(),
		Target: TargetInput{
			RepoID: "repo", RootID: "root", PatchEventID: "patch",
			CanonicalRemoteIdentity: "remote",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.ReleasePrepared(prepared)
	output, err := service.ReviewPrepared(ctx, prepared, ReviewOptions{
		SkipWalkthrough: true,
		Ensemble: &reviewengine.EnsembleConfig{
			Enabled: true, Models: []reviewengine.ModelRoute{reviewengine.RouteCoder32B, reviewengine.RouteLLM70B},
			RequireConsensus: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Review.Findings) != 1 || output.EnsembleStatus.Degraded ||
		len(output.EnsembleStatus.ReviewerTraces) != 2 {
		t.Fatalf("ensemble output = %#v", output)
	}
	for _, member := range output.EnsembleStatus.ReviewerTraces {
		want := "a-read"
		if member.Route == reviewengine.RouteLLM70B {
			want = "b-read"
		}
		if !slices.Equal(member.Trace.EvidenceToolCallIDs, []string{want}) {
			t.Fatalf("%s evidence ledger = %v", member.Route, member.Trace.EvidenceToolCallIDs)
		}
	}
	for _, request := range client.RequestsForModel("coder32b") {
		encoded, _ := json.Marshal(request.Messages)
		if strings.Contains(string(encoded), "b-read") {
			t.Fatal("coder32b transcript contains llm70b evidence")
		}
	}
	llmRequests := client.RequestsForModel("llm70b")
	var sawCrossEvidenceRejection bool
	for _, message := range llmRequests[len(llmRequests)-1].Messages {
		if message.Role == reviewengine.MessageRoleTool && message.ToolCallID == "b-cross-submit" &&
			strings.Contains(message.Content, "unknown current-run evidence") {
			sawCrossEvidenceRejection = true
		}
	}
	if !sawCrossEvidenceRejection {
		t.Fatal("cross-member evidence was not rejected")
	}
}

func osWrite(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
