package agenticreview

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"drydock/internal/agenttools"
	"drydock/internal/db"
	"drydock/internal/reviewengine"
	"drydock/internal/reviewsession"
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

func osWrite(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
