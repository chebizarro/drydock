package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"drydock/internal/contextbuilder"
	"drydock/internal/lspbridge"
	"drydock/internal/securityscan"
	"drydock/internal/workspacesnapshot"
)

func TestReviewSubmitSchemaRequiresEvidenceAndCoverage(t *testing.T) {
	var schema string
	for _, definition := range NewRegistry().List(RoleCodeReviewer) {
		if definition.Name == ToolReviewSubmit {
			schema = string(definition.InputSchema)
			break
		}
	}
	if schema == "" {
		t.Fatal("review.submit definition missing")
	}
	for _, required := range []string{
		`"summary"`, `"priority"`, `"evidence_tool_call_ids"`,
		`"examined_files"`, `"no_findings"`,
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("review.submit schema missing %s: %s", required, schema)
		}
	}
}

func TestRolePolicyFiltersListingAndDispatch(t *testing.T) {
	snapshot := mutableSnapshot(t, map[string]string{"main.go": "package p"})
	registry := NewRegistry()

	discovery := definitionNames(registry.List(RoleContextDiscovery))
	if !slices.Contains(discovery, ToolSelectionFinalize) || slices.Contains(discovery, ToolReviewSubmit) {
		t.Fatalf("discovery definitions = %v", discovery)
	}
	reviewer := definitionNames(registry.List(RoleCodeReviewer))
	if !slices.Contains(reviewer, ToolReviewSubmit) || slices.Contains(reviewer, ToolSelectionAdd) {
		t.Fatalf("reviewer definitions = %v", reviewer)
	}
	security := definitionNames(registry.List(RoleSecurityAuditor))
	if !slices.Contains(security, ToolSecurityTrace) || slices.Contains(reviewer, ToolSecurityTrace) {
		t.Fatalf("security definitions = %v reviewer = %v", security, reviewer)
	}
	securityDiscovery := definitionNames(registry.List(RoleSecurityAuditorDiscovery))
	if !slices.Contains(securityDiscovery, ToolSecurityTrace) || !slices.Contains(securityDiscovery, ToolSelectionFinalize) || slices.Contains(securityDiscovery, ToolReviewSubmit) {
		t.Fatalf("security discovery definitions = %v", securityDiscovery)
	}
	external := definitionNames(registry.List(RoleExternalReadonly))
	if slices.Contains(external, ToolReviewSubmit) || slices.Contains(external, ToolSelectionStatus) {
		t.Fatalf("external definitions = %v", external)
	}

	scope := NewScope("review-run", snapshot, RoleCodeReviewer)
	_, err := registry.Dispatch(context.Background(), Invocation{
		ToolCallID: "call-1", Name: ToolSelectionAdd,
		Arguments: json.RawMessage(`{"artifacts":[]}`), Scope: scope,
	})
	if !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("dispatch hidden selection tool error = %v", err)
	}
}

func TestCapabilityEscalationFailsAtListingAndDispatch(t *testing.T) {
	snapshot := mutableSnapshot(t, map[string]string{"main.go": "package p"})
	selection, err := NewSelection(SelectionConfig{
		Snapshot: snapshot, ChangedFiles: []string{"main.go"},
		Counter: byteCounter{}, TokenBudget: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	tests := []struct {
		name string
		role Role
		tool string
	}{
		{"external submit", RoleExternalReadonly, ToolReviewSubmit},
		{"external finalize", RoleExternalReadonly, ToolSelectionFinalize},
		{"reviewer selection mutation", RoleCodeReviewer, ToolSelectionAdd},
		{"auditor selection mutation", RoleSecurityAuditor, ToolSelectionAdd},
		{"unknown role read", Role("forged_admin"), ToolCodeRead},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if slices.Contains(definitionNames(registry.List(test.role)), test.tool) {
				t.Fatalf("%s advertised to role %s", test.tool, test.role)
			}
			scope := NewScope("escalation:"+test.name, snapshot, test.role)
			scope.Selection = selection
			_, err := registry.Dispatch(context.Background(), Invocation{
				ToolCallID: "attack-" + test.name, Name: test.tool,
				Arguments: json.RawMessage(`{}`), Scope: scope,
			})
			if !errors.Is(err, ErrCapabilityDenied) {
				t.Fatalf("dispatch %s as %s error = %v", test.tool, test.role, err)
			}
		})
	}
	if selection.Status().Finalized {
		t.Fatal("denied capability mutated selection")
	}
}

func TestHandlersReadOnlyFromSnapshotAndRejectTraversal(t *testing.T) {
	workspace := t.TempDir()
	writeAgentFile(t, workspace, "main.go", "package p\nfunc Before() {}\n")
	writeAgentFile(t, workspace, "main_test.go", "package p\nfunc TestBefore() {}\n")
	snapshot := createMutableSnapshot(t, workspace)
	writeAgentFile(t, workspace, "main.go", "package p\nfunc After() {}\n")

	registry := NewRegistry()
	scope := NewScope("discovery-run", snapshot, RoleContextDiscovery)
	read, err := registry.Dispatch(context.Background(), Invocation{
		ToolCallID: "read-1", Name: ToolCodeRead,
		Arguments: json.RawMessage(`{"path":"main.go"}`), Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read.Content, "Before") || strings.Contains(read.Content, "After") {
		t.Fatalf("read content = %q", read.Content)
	}

	search, err := registry.Dispatch(context.Background(), Invocation{
		ToolCallID: "search-1", Name: ToolCodeSearch,
		Arguments: json.RawMessage(`{"query":"Before"}`), Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(search.Content, "main.go") {
		t.Fatalf("search content = %q", search.Content)
	}
	tests, err := registry.Dispatch(context.Background(), Invocation{
		ToolCallID: "tests-1", Name: ToolTestsSearch,
		Arguments: json.RawMessage(`{"query":"Before"}`), Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tests.Content, `"path":"main.go"`) || !strings.Contains(tests.Content, "main_test.go") {
		t.Fatalf("tests.search content = %q", tests.Content)
	}

	for index, path := range []string{
		"../main.go", "/etc/passwd", `C:\Windows\system.ini`,
		`src\..\main.go`, "src/../../etc/passwd", "main.go\x00outside",
	} {
		arguments, marshalErr := json.Marshal(map[string]any{"path": path})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		result, dispatchErr := registry.Dispatch(context.Background(), Invocation{
			ToolCallID: fmt.Sprintf("escape-%d", index), Name: ToolCodeRead,
			Arguments: arguments, Scope: scope,
		})
		if !errors.Is(dispatchErr, workspacesnapshot.ErrInvalidPath) {
			t.Errorf("path %q error = %v, want ErrInvalidPath", path, dispatchErr)
		}
		if result.Content != "" {
			t.Errorf("path %q leaked content %q", path, result.Content)
		}
	}
}

func TestToolReadRejectsPostFreezeSymlinkSubstitution(t *testing.T) {
	snapshot := mutableSnapshot(t, map[string]string{"main.go": "safe snapshot content"})
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := snapshot.Resolve("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(resolved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, resolved); err != nil {
		t.Fatal(err)
	}
	result, err := NewRegistry().Dispatch(context.Background(), Invocation{
		ToolCallID: "symlink-attack", Name: ToolCodeRead,
		Arguments: json.RawMessage(`{"path":"main.go"}`),
		Scope:     NewScope("symlink-attack", snapshot, RoleExternalReadonly),
	})
	if err == nil {
		t.Fatal("post-freeze symlink read succeeded")
	}
	if strings.Contains(result.Content, "outside secret") {
		t.Fatalf("outside content leaked: %q", result.Content)
	}
}

func TestAuditToolsUseFrozenFacadesAndSecurityCapability(t *testing.T) {
	snapshot := mutableSnapshot(t, map[string]string{
		"main.go": "package p\nfunc run(req string) { exec.Command(req) }\n",
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/analyze" {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","lsp_available":true,"references":[{"symbol":"run","file":"main.go","line":2,"column":1}]}`))
	}))
	defer server.Close()

	registry := NewRegistry(
		WithReferencesFacade(contextbuilder.NewReferencesFacade(lspbridge.NewClient(server.URL))),
		WithLayerFacade(contextbuilder.NewLayerFacade(contextbuilder.NewBuilderOptions())),
		WithSecurityTraceFacade(contextbuilder.NewSecurityTraceFacade(nil, securityscan.New())),
	)
	securityScope := NewScope("security-run", snapshot, RoleSecurityAuditor)

	references, err := registry.Dispatch(context.Background(), Invocation{
		ToolCallID: "references-1", Name: ToolCodeReferences,
		Arguments: json.RawMessage(`{"files":["main.go"],"symbols":["run"]}`), Scope: securityScope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(references.Content, `"symbol":"run"`) {
		t.Fatalf("references = %s", references.Content)
	}

	layer, err := registry.Dispatch(context.Background(), Invocation{
		ToolCallID: "layer-1", Name: ToolContextLayer,
		Arguments: json.RawMessage(`{"name":"patch","paths":["main.go"]}`), Scope: securityScope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(layer.Content, "diff --git a/main.go b/main.go") {
		t.Fatalf("context layer = %s", layer.Content)
	}

	trace, err := registry.Dispatch(context.Background(), Invocation{
		ToolCallID: "trace-1", Name: ToolSecurityTrace,
		Arguments: json.RawMessage(`{"kind":"taint","paths":["main.go"]}`), Scope: securityScope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(trace.Content, "command-execution") {
		t.Fatalf("security trace = %s", trace.Content)
	}

	_, err = registry.Dispatch(context.Background(), Invocation{
		ToolCallID: "trace-denied", Name: ToolSecurityTrace,
		Arguments: json.RawMessage(`{"kind":"taint","paths":["main.go"]}`),
		Scope:     NewScope("review-run", snapshot, RoleCodeReviewer),
	})
	if !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("non-security trace error = %v", err)
	}
}

func TestStructureUsesTreeSitterFacade(t *testing.T) {
	snapshot := mutableSnapshot(t, map[string]string{
		"main.go": "package p\ntype Item struct{}\nfunc Build() Item { return Item{} }\n",
	})
	registry := NewRegistry()
	result, err := registry.Dispatch(context.Background(), Invocation{
		ToolCallID: "structure-1", Name: ToolCodeStructure,
		Arguments: json.RawMessage(`{"path":"main.go"}`),
		Scope:     NewScope("structure-run", snapshot, RoleContextDiscovery),
	})
	if err != nil {
		if strings.Contains(err.Error(), "tree-sitter is unavailable") {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "Build") || !strings.Contains(result.Content, "Item") {
		t.Fatalf("structure = %s", result.Content)
	}
}

func TestResultLimitAndReplayCache(t *testing.T) {
	snapshot := mutableSnapshot(t, map[string]string{"large.txt": strings.Repeat("x", 128)})
	registry := NewRegistry()
	scope := NewScope("limited-run", snapshot, RoleExternalReadonly)
	scope.MaxResultBytes = 16
	result, err := registry.Dispatch(context.Background(), Invocation{
		ToolCallID: "limited-1", Name: ToolCodeRead,
		Arguments: json.RawMessage(`{"path":"large.txt"}`), Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || len(result.Content) > 16 {
		t.Fatalf("limited result = %#v", result)
	}

	var calls atomic.Int32
	if err := registry.Register(Definition{
		Name: "test.replay", Capability: CapabilityRead,
		InputSchema: json.RawMessage(`{"type":"object"}`), MaxResultBytes: 100,
	}, func(context.Context, Invocation) (Result, error) {
		calls.Add(1)
		return Result{Content: "once"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	invocation := Invocation{
		ToolCallID: "same-id", Name: "test.replay",
		Arguments: json.RawMessage(`{"value":1}`), Scope: scope,
	}
	first, err := registry.Dispatch(context.Background(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Dispatch(context.Background(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replay || !second.Replay || calls.Load() != 1 {
		t.Fatalf("first=%#v second=%#v calls=%d", first, second, calls.Load())
	}
	invocation.Arguments = json.RawMessage(`{"value":2}`)
	if _, err := registry.Dispatch(context.Background(), invocation); !errors.Is(err, ErrReplayConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}
}

func TestConcurrentDuplicateToolCallIsSingleFlight(t *testing.T) {
	snapshot := mutableSnapshot(t, map[string]string{"file.txt": "content"})
	registry := NewRegistry()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	if err := registry.Register(Definition{
		Name: "test.concurrent", Capability: CapabilityRead,
		InputSchema: json.RawMessage(`{"type":"object"}`), MaxResultBytes: 100,
	}, func(context.Context, Invocation) (Result, error) {
		calls.Add(1)
		close(started)
		<-release
		return Result{Content: "once"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	invocation := Invocation{
		ToolCallID: "concurrent-id", Name: "test.concurrent",
		Arguments: json.RawMessage(`{}`),
		Scope:     NewScope("concurrent-run", snapshot, RoleExternalReadonly),
	}
	results := make(chan Result, 2)
	errs := make(chan error, 2)
	go func() {
		result, err := registry.Dispatch(context.Background(), invocation)
		results <- result
		errs <- err
	}()
	<-started
	go func() {
		result, err := registry.Dispatch(context.Background(), invocation)
		results <- result
		errs <- err
	}()
	close(release)
	first, second := <-results, <-results
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls = %d", calls.Load())
	}
	if first.Replay == second.Replay {
		t.Fatalf("expected exactly one replay: first=%#v second=%#v", first, second)
	}
}

func TestGitReadClosedActionSet(t *testing.T) {
	repo := t.TempDir()
	runAgent(t, repo, "git", "init", "-q")
	runAgent(t, repo, "git", "config", "user.email", "test@example.com")
	runAgent(t, repo, "git", "config", "user.name", "Test")
	writeAgentFile(t, repo, "main.go", "package p")
	runAgent(t, repo, "git", "add", ".")
	runAgent(t, repo, "git", "commit", "-m", "initial")
	manager, err := workspacesnapshot.NewManager(workspacesnapshot.Config{
		StorageRoot: t.TempDir(), LeaseTTL: time.Hour, SessionLifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.CreatePinned(context.Background(), workspacesnapshot.PinnedGitOptions{
		RepoPath: repo, Ref: "HEAD", Patch: []byte("authoritative diff"), Allowlist: []string{"."},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	scope := NewScope("git-run", snapshot, RoleExternalReadonly)
	result, err := registry.Dispatch(context.Background(), Invocation{
		ToolCallID: "git-1", Name: ToolGitRead,
		Arguments: json.RawMessage(`{"action":"diff"}`), Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "authoritative diff" {
		t.Fatalf("git diff = %q", result.Content)
	}
	_, err = registry.Dispatch(context.Background(), Invocation{
		ToolCallID: "git-2", Name: ToolGitRead,
		Arguments: json.RawMessage(`{"action":"status"}`), Scope: scope,
	})
	if err == nil {
		t.Fatal("unsupported git action succeeded")
	}
}

func definitionNames(definitions []Definition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}

func mutableSnapshot(t *testing.T, files map[string]string) *workspacesnapshot.Snapshot {
	t.Helper()
	workspace := t.TempDir()
	for path, content := range files {
		writeAgentFile(t, workspace, path, content)
	}
	return createMutableSnapshot(t, workspace)
}

func createMutableSnapshot(t *testing.T, workspace string) *workspacesnapshot.Snapshot {
	t.Helper()
	manager, err := workspacesnapshot.NewManager(workspacesnapshot.Config{
		StorageRoot: t.TempDir(), LeaseTTL: time.Hour, SessionLifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.CreateMutable(context.Background(), workspacesnapshot.MutableCopyOptions{
		WorkspacePath: workspace, Allowlist: []string{"."},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func writeAgentFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runAgent(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
}
