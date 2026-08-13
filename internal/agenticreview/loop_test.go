package agenticreview

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"drydock/internal/agenttools"
	"drydock/internal/contextbuilder"
	"drydock/internal/reviewengine"
	"drydock/internal/workspacesnapshot"
)

type testCounter struct{}

func (testCounter) Count(text string) int { return len(text) }

type scriptedClient struct {
	mu       sync.Mutex
	results  []reviewengine.CompletionResult
	requests []reviewengine.CompletionRequest
}

func (c *scriptedClient) Complete(_ context.Context, request reviewengine.CompletionRequest) (reviewengine.CompletionResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, request)
	if len(c.results) == 0 {
		return reviewengine.CompletionResult{}, errors.New("unexpected completion")
	}
	result := c.results[0]
	c.results = c.results[1:]
	return result, nil
}

func TestLoopExecutesToolsSequentiallyAndStopsOnlyOnFinalize(t *testing.T) {
	patch := testPatch()
	snapshot := discoverySnapshot(t, patch, map[string]string{"changed.go": "package p\nconst Value = 2\n"})
	selection, err := agenttools.NewSelection(agenttools.SelectionConfig{
		Snapshot: snapshot, ChangedFiles: []string{"changed.go"},
		Counter: testCounter{}, TokenBudget: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := agenttools.NewRegistry()
	var order []string
	if err := registry.Register(agenttools.Definition{
		Name: "test.first", Capability: agenttools.CapabilityRead,
		InputSchema: json.RawMessage(`{"type":"object"}`), MaxResultBytes: 100,
	}, func(context.Context, agenttools.Invocation) (agenttools.Result, error) {
		order = append(order, "first")
		return agenttools.Result{Content: "ok"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(agenttools.Definition{
		Name: "test.second", Capability: agenttools.CapabilityRead,
		InputSchema: json.RawMessage(`{"type":"object"}`), MaxResultBytes: 100,
	}, func(context.Context, agenttools.Invocation) (agenttools.Result, error) {
		order = append(order, "second")
		return agenttools.Result{Content: "ok"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{results: []reviewengine.CompletionResult{
		{
			Message: reviewengine.CompletionMessage{Role: reviewengine.MessageRoleAssistant, Content: "I should explore first."},
			Usage:   reviewengine.CompletionUsage{TotalTokens: 1},
		},
		{
			Message: reviewengine.CompletionMessage{Role: reviewengine.MessageRoleAssistant, ToolCalls: []reviewengine.ToolCall{
				{ID: "one", Type: "function", Function: reviewengine.ToolCallFunction{Name: "test.first", Arguments: `{}`}},
				{ID: "two", Type: "function", Function: reviewengine.ToolCallFunction{Name: "test.second", Arguments: `{}`}},
				{ID: "final", Type: "function", Function: reviewengine.ToolCallFunction{Name: agenttools.ToolSelectionFinalize, Arguments: `{}`}},
			}},
			Usage: reviewengine.CompletionUsage{TotalTokens: 1},
		},
	}}
	scope := agenttools.NewScope("loop-test", snapshot, agenttools.RoleContextDiscovery)
	result, err := (&LoopRunner{Client: client}).Run(context.Background(), LoopRequest{
		Completion: reviewengine.CompletionRequest{
			Messages: []reviewengine.CompletionMessage{{Role: reviewengine.MessageRoleUser, Content: "discover"}},
		},
		Registry: registry, Scope: scope, Selection: selection, Counter: testCounter{},
		Limits: LoopLimits{MaxTurns: 3, MaxToolCalls: 3, MaxCumulativeTokens: 100_000, MaxModelContext: 100_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "first,second" {
		t.Fatalf("execution order = %v", order)
	}
	if result.Trace.StopReason != StopFinalized || result.Trace.Turns != 2 || result.Trace.ToolCalls != 3 {
		t.Fatalf("trace = %#v", result.Trace)
	}
	if result.Bundle.Content == "" {
		t.Fatal("finalize did not return bundle")
	}
}

func TestLoopPreflightsModelContextBeforeCallingClient(t *testing.T) {
	patch := testPatch()
	snapshot := discoverySnapshot(t, patch, map[string]string{"changed.go": "new"})
	selection, err := agenttools.NewSelection(agenttools.SelectionConfig{
		Snapshot: snapshot, ChangedFiles: []string{"changed.go"},
		Counter: testCounter{}, TokenBudget: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{}
	_, err = (&LoopRunner{Client: client}).Run(context.Background(), LoopRequest{
		Completion: reviewengine.CompletionRequest{
			Messages: []reviewengine.CompletionMessage{{Role: reviewengine.MessageRoleUser, Content: strings.Repeat("x", 100)}},
		},
		Registry:  agenttools.NewRegistry(),
		Scope:     agenttools.NewScope("preflight", snapshot, agenttools.RoleContextDiscovery),
		Selection: selection, Counter: testCounter{},
		Limits: LoopLimits{MaxTurns: 1, MaxModelContext: 1, MaxCumulativeTokens: 1000},
	})
	if !errors.Is(err, ErrModelContext) {
		t.Fatalf("preflight error = %v", err)
	}
	if len(client.requests) != 0 {
		t.Fatalf("client called %d times", len(client.requests))
	}
}

type recordingBuilder struct {
	bundle  contextbuilder.ContextBundle
	content string
	err     error
}

func (b *recordingBuilder) Build(_ context.Context, input contextbuilder.BuildInput) (contextbuilder.ContextBundle, error) {
	if b.err != nil {
		return contextbuilder.ContextBundle{}, b.err
	}
	data, err := os.ReadFile(filepath.Join(input.RepoPath, "changed.go"))
	if err != nil {
		return contextbuilder.ContextBundle{}, err
	}
	b.content = string(data)
	return b.bundle, nil
}

func TestDiscoveryFallsBackOnExhaustionThroughExactGate(t *testing.T) {
	patch := testPatch()
	workspace := t.TempDir()
	writeDiscoveryFile(t, workspace, "changed.go", "package p\nconst Value = 2\n")
	snapshot := createDiscoverySnapshot(t, workspace, patch)
	writeDiscoveryFile(t, workspace, "changed.go", "LIVE MUTATION")
	builder := &recordingBuilder{bundle: contextbuilder.ContextBundle{Content: "deterministic fallback"}}
	client := &scriptedClient{results: []reviewengine.CompletionResult{{
		Message: reviewengine.CompletionMessage{Role: reviewengine.MessageRoleAssistant, Content: "not finalized"},
		Usage:   reviewengine.CompletionUsage{TotalTokens: 1},
	}}}
	discovery, err := NewDiscovery(DiscoveryConfig{
		Client: client, Registry: agenttools.NewRegistry(), Counter: testCounter{},
		Builder: builder, Limits: LoopLimits{MaxTurns: 1, MaxToolCalls: 4, MaxCumulativeTokens: 100_000, MaxModelContext: 100_000},
		TokenBudget: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := discovery.Run(context.Background(), DiscoveryInput{
		Snapshot: snapshot, Patch: patch, ChangedFiles: []string{"changed.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Trace.FallbackReason != FallbackLoopExhaustion || result.Trace.Loop.StopReason != StopTurnsExhausted {
		t.Fatalf("trace = %#v", result.Trace)
	}
	if result.Bundle.TokenCount != len(result.Bundle.Content) {
		t.Fatalf("fallback token count = %d", result.Bundle.TokenCount)
	}
	if strings.Contains(builder.content, "LIVE") || !strings.Contains(builder.content, "Value = 2") {
		t.Fatalf("builder read non-snapshot content %q", builder.content)
	}
}

func TestDiscoveryFailsWhenFallbackAlsoExceedsGate(t *testing.T) {
	patch := testPatch()
	snapshot := discoverySnapshot(t, patch, map[string]string{"changed.go": "new"})
	client := &scriptedClient{results: []reviewengine.CompletionResult{{
		Message: reviewengine.CompletionMessage{Role: reviewengine.MessageRoleAssistant, Content: "not finalized"},
		Usage:   reviewengine.CompletionUsage{TotalTokens: 1},
	}}}
	discovery, err := NewDiscovery(DiscoveryConfig{
		Client: client, Registry: agenttools.NewRegistry(), Counter: testCounter{},
		Builder:     &recordingBuilder{bundle: contextbuilder.ContextBundle{Content: strings.Repeat("x", 101)}},
		Limits:      LoopLimits{MaxTurns: 1, MaxCumulativeTokens: 100_000, MaxModelContext: 100_000},
		TokenBudget: 100, Headroom: 0.10,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := discovery.Run(context.Background(), DiscoveryInput{
		Snapshot: snapshot, Patch: patch, ChangedFiles: []string{"changed.go"},
	})
	if !errors.Is(err, agenttools.ErrBudgetExceeded) {
		t.Fatalf("fallback gate error = %v", err)
	}
	if result.Trace.FallbackReason != FallbackLoopExhaustion {
		t.Fatalf("fallback trace = %#v", result.Trace)
	}
}

func TestDiscoveryRejectsPatchSnapshotMismatch(t *testing.T) {
	snapshot := discoverySnapshot(t, "different", map[string]string{"changed.go": "new"})
	discovery, err := NewDiscovery(DiscoveryConfig{
		Client: &scriptedClient{}, Registry: agenttools.NewRegistry(), Counter: testCounter{},
		TokenBudget: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := discovery.Run(context.Background(), DiscoveryInput{Snapshot: snapshot, Patch: testPatch()}); err == nil {
		t.Fatal("patch mismatch succeeded")
	}
}

func TestDefaultLoopLimits(t *testing.T) {
	limits := DefaultLoopLimits()
	if limits.MaxTurns != 24 || limits.MaxToolCalls != 96 ||
		limits.MaxCumulativeTokens != 256_000 || limits.MaxToolResultBytes != 16*1024 {
		t.Fatalf("defaults = %#v", limits)
	}
}

func testPatch() string {
	return "diff --git a/changed.go b/changed.go\n" +
		"--- a/changed.go\n+++ b/changed.go\n@@ -1 +1 @@\n-old\n+new\n"
}

func discoverySnapshot(t *testing.T, patch string, files map[string]string) *workspacesnapshot.Snapshot {
	t.Helper()
	workspace := t.TempDir()
	for path, content := range files {
		writeDiscoveryFile(t, workspace, path, content)
	}
	return createDiscoverySnapshot(t, workspace, patch)
}

func createDiscoverySnapshot(t *testing.T, workspace, patch string) *workspacesnapshot.Snapshot {
	t.Helper()
	manager, err := workspacesnapshot.NewManager(workspacesnapshot.Config{
		StorageRoot: t.TempDir(), LeaseTTL: time.Hour, SessionLifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.CreateMutable(context.Background(), workspacesnapshot.MutableCopyOptions{
		WorkspacePath: workspace, Patch: []byte(patch), Allowlist: []string{"."},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func writeDiscoveryFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
