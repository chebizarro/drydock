package agenttools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"drydock/internal/contextbuilder"
	"drydock/internal/workspacesnapshot"
)

const DefaultMaxResultBytes = 16 * 1024

const (
	ToolRepoFileTree      = "repo.file_tree"
	ToolCodeStructure     = "code.structure"
	ToolCodeSearch        = "code.search"
	ToolCodeRead          = "code.read"
	ToolCodeReferences    = "code.references"
	ToolContextLayer      = "context.layer"
	ToolSecurityTrace     = "security.trace"
	ToolTestsSearch       = "tests.search"
	ToolGitRead           = "git.read"
	ToolSelectionAdd      = "selection.add"
	ToolSelectionRemove   = "selection.remove"
	ToolSelectionStatus   = "selection.status"
	ToolSelectionFinalize = "selection.finalize"
	ToolReviewSubmit      = "review.submit"
)

func registerCoreTools(registry *Registry) {
	definitions := []struct {
		def     Definition
		handler Handler
	}{
		{definition(ToolRepoFileTree, "List files in the frozen repository snapshot.", CapabilityRead, `{"type":"object","properties":{"path":{"type":"string"},"max_depth":{"type":"integer","minimum":0,"maximum":32}},"additionalProperties":false}`), handleFileTree},
		{definition(ToolCodeStructure, "Extract tree-sitter declarations from a frozen source file.", CapabilityRead, `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`), handleStructure},
		{definition(ToolCodeSearch, "Search frozen source content.", CapabilityRead, `{"type":"object","properties":{"query":{"type":"string"},"path":{"type":"string"},"regex":{"type":"boolean"},"max_results":{"type":"integer","minimum":1,"maximum":1000}},"required":["query"],"additionalProperties":false}`), handleCodeSearch},
		{definition(ToolCodeRead, "Read a line range from a frozen file.", CapabilityRead, `{"type":"object","properties":{"path":{"type":"string"},"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`), handleCodeRead},
		{definition(ToolCodeReferences, "Resolve symbol definitions and references through the configured LSP facade against the frozen snapshot.", CapabilityRead, `{"type":"object","properties":{"files":{"type":"array","items":{"type":"string"},"minItems":1,"uniqueItems":true},"symbols":{"type":"array","items":{"type":"string"},"minItems":1,"uniqueItems":true}},"required":["files","symbols"],"additionalProperties":false}`), registry.handleCodeReferences},
		{definition(ToolContextLayer, "Run one existing context provider selected from its closed layer name.", CapabilityRead, `{"type":"object","properties":{"name":{"type":"string"},"paths":{"type":"array","items":{"type":"string"},"uniqueItems":true}},"required":["name"],"additionalProperties":false}`), registry.handleContextLayer},
		{definition(ToolSecurityTrace, "Trace taint paths or security surfaces across frozen audit roots.", CapabilitySnapshotWide, `{"type":"object","properties":{"kind":{"type":"string","enum":["taint","security-surface"]},"paths":{"type":"array","items":{"type":"string"},"uniqueItems":true}},"additionalProperties":false}`), registry.handleSecurityTrace},
		{definition(ToolTestsSearch, "Search test files in the frozen snapshot.", CapabilityRead, `{"type":"object","properties":{"query":{"type":"string"},"path":{"type":"string"},"regex":{"type":"boolean"},"max_results":{"type":"integer","minimum":1,"maximum":1000}},"required":["query"],"additionalProperties":false}`), handleTestsSearch},
		{definition(ToolGitRead, "Run one closed read-only git action (diff, show, log, blame) at the pinned commit.", CapabilityRead, `{"type":"object","properties":{"action":{"type":"string","enum":["diff","show","log","blame"]},"path":{"type":"string"},"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1,"maximum":100}},"required":["action"],"additionalProperties":false}`), handleGitRead},
		{definition(ToolSelectionAdd, "Add file, line-range, or codemap artifacts to the run-local selection.", CapabilitySelectionMutate, `{"type":"object","properties":{"artifacts":{"type":"array","items":{"type":"object"}}},"required":["artifacts"],"additionalProperties":false}`), handleSelection},
		{definition(ToolSelectionRemove, "Remove non-mandatory artifacts from the run-local selection.", CapabilitySelectionMutate, `{"type":"object","properties":{"artifacts":{"type":"array","items":{"type":"object"}}},"required":["artifacts"],"additionalProperties":false}`), handleSelection},
		{definition(ToolSelectionStatus, "Report the run-local selection and budget status.", CapabilitySelectionRead, `{"type":"object","additionalProperties":false}`), handleSelection},
		{definition(ToolSelectionFinalize, "Render, hash-verify, and exact-token-check the immutable context package.", CapabilitySelectionFinalize, `{"type":"object","additionalProperties":false}`), handleSelection},
		{reviewSubmitDefinition(), handleReviewSubmit},
	}
	for _, item := range definitions {
		if err := registry.Register(item.def, item.handler); err != nil {
			panic(err)
		}
	}
}

func reviewSubmitDefinition() Definition {
	return definition(ToolReviewSubmit, "Submit the final evidence-backed structured review.", CapabilityReviewSubmit, `{
		"type":"object",
		"properties":{
			"summary":{"type":"string","minLength":1},
			"findings":{"type":"array","items":{
				"type":"object",
				"properties":{
					"priority":{"type":"string","enum":["P0","P1","P2"]},
					"category":{"type":"string","enum":["security","correctness","architecture","style","test-coverage"]},
					"file":{"type":"string","minLength":1},
					"line":{"type":"integer","minimum":1},
					"explanation":{"type":"string","minLength":1},
					"suggestion":{"type":"string"},
					"suggested_diff":{"type":"string"},
					"suggested_code":{"type":"string"},
					"confidence":{"type":"number","minimum":0,"maximum":1},
					"evidence_tool_call_ids":{"type":"array","items":{"type":"string","minLength":1},"minItems":1,"uniqueItems":true}
				},
				"required":["priority","category","file","line","explanation","confidence","evidence_tool_call_ids"],
				"additionalProperties":false
			}},
			"needs_more_context":{"type":"array","items":{"type":"string"}},
			"coverage":{
				"type":"object",
				"properties":{
					"examined_files":{"type":"array","items":{"type":"string","minLength":1},"minItems":1,"uniqueItems":true},
					"outcome":{"type":"string","enum":["findings","no_findings"]},
					"summary":{"type":"string","minLength":1}
				},
				"required":["examined_files","outcome","summary"],
				"additionalProperties":false
			}
		},
		"required":["summary","findings","coverage"],
		"additionalProperties":false
	}`)
}

func definition(name, description string, capability Capability, schema string) Definition {
	return Definition{
		Name: name, Description: description, Capability: capability,
		InputSchema: json.RawMessage(schema), MaxResultBytes: DefaultMaxResultBytes,
	}
}

type snapshotContentSource struct{ snapshot *workspacesnapshot.Snapshot }

func (s snapshotContentSource) ListPaths(_ context.Context, prefix string) ([]string, error) {
	entries, err := s.snapshot.List(prefix)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	return paths, nil
}

func (s snapshotContentSource) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return s.snapshot.ReadFile(ctx, path)
}

func handleFileTree(_ context.Context, invocation Invocation) (Result, error) {
	var args struct {
		Path     string `json:"path"`
		MaxDepth int    `json:"max_depth"`
	}
	if err := decodeArguments(invocation.Arguments, &args); err != nil {
		return Result{}, err
	}
	if args.Path == "" {
		args.Path = "."
	}
	entries, err := invocation.Scope.Snapshot.List(args.Path)
	if err != nil {
		return Result{}, err
	}
	var paths []string
	for _, entry := range entries {
		path := entry.Path
		if args.MaxDepth > 0 {
			rel := path
			if args.Path != "." {
				rel = strings.TrimPrefix(strings.TrimPrefix(path, args.Path), "/")
			}
			if strings.Count(rel, "/") > args.MaxDepth {
				continue
			}
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return Result{Content: strings.Join(paths, "\n")}, nil
}

func handleCodeRead(ctx context.Context, invocation Invocation) (Result, error) {
	var args struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := decodeArguments(invocation.Arguments, &args); err != nil {
		return Result{}, err
	}
	content, err := invocation.Scope.Snapshot.ReadFile(ctx, args.Path)
	if err != nil {
		return Result{}, err
	}
	lines := strings.Split(string(content), "\n")
	start := args.StartLine
	if start <= 0 {
		start = 1
	}
	end := args.EndLine
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start > end || start > len(lines) {
		return Result{Content: ""}, nil
	}
	return Result{Content: strings.Join(lines[start-1:end], "\n")}, nil
}

func handleStructure(ctx context.Context, invocation Invocation) (Result, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := decodeArguments(invocation.Arguments, &args); err != nil {
		return Result{}, err
	}
	content, err := invocation.Scope.Snapshot.ReadFile(ctx, args.Path)
	if err != nil {
		return Result{}, err
	}
	structure, err := contextbuilder.NewStructureFacade().Analyze(contextbuilder.StructureRequest{Path: args.Path, Content: content})
	if err != nil {
		return Result{}, err
	}
	return jsonResult(structure)
}

func handleCodeSearch(ctx context.Context, invocation Invocation) (Result, error) {
	return handleSearch(ctx, invocation, false)
}

func handleTestsSearch(ctx context.Context, invocation Invocation) (Result, error) {
	return handleSearch(ctx, invocation, true)
}

func handleSearch(ctx context.Context, invocation Invocation, testsOnly bool) (Result, error) {
	var args struct {
		Query      string `json:"query"`
		Path       string `json:"path"`
		Regex      bool   `json:"regex"`
		MaxResults int    `json:"max_results"`
	}
	if err := decodeArguments(invocation.Arguments, &args); err != nil {
		return Result{}, err
	}
	hits, err := contextbuilder.NewContentSearchFacade().Search(ctx, snapshotContentSource{invocation.Scope.Snapshot}, contextbuilder.ContentSearchRequest{
		Query: args.Query, Path: args.Path, Regex: args.Regex, TestsOnly: testsOnly, MaxResults: args.MaxResults,
	})
	if err != nil {
		return Result{}, err
	}
	return jsonResult(hits)
}

func handleGitRead(ctx context.Context, invocation Invocation) (Result, error) {
	var args struct {
		Action    string `json:"action"`
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		Limit     int    `json:"limit"`
	}
	if err := decodeArguments(invocation.Arguments, &args); err != nil {
		return Result{}, err
	}
	content, err := invocation.Scope.Snapshot.GitRead(ctx, workspacesnapshot.GitReadRequest{
		Action: args.Action, Path: args.Path, StartLine: args.StartLine, EndLine: args.EndLine, Limit: args.Limit,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Content: string(content)}, nil
}

func (r *Registry) handleCodeReferences(ctx context.Context, invocation Invocation) (Result, error) {
	if r.references == nil {
		return Result{}, ErrHandlerUnavailable
	}
	var args struct {
		Files   []string `json:"files"`
		Symbols []string `json:"symbols"`
	}
	if err := decodeArguments(invocation.Arguments, &args); err != nil {
		return Result{}, err
	}
	files, err := validatedSnapshotPaths(ctx, invocation.Scope.Snapshot, args.Files)
	if err != nil {
		return Result{}, err
	}
	var response any
	err = withMaterializedSnapshot(ctx, invocation.Scope.Snapshot, func(root string) error {
		var analyzeErr error
		response, analyzeErr = r.references.Analyze(ctx, contextbuilder.ReferencesRequest{
			RepoPath: root, Files: files, Symbols: args.Symbols,
		})
		return analyzeErr
	})
	if err != nil {
		return Result{}, err
	}
	return jsonResult(response)
}

func (r *Registry) handleContextLayer(ctx context.Context, invocation Invocation) (Result, error) {
	if r.layers == nil {
		return Result{}, ErrHandlerUnavailable
	}
	var args struct {
		Name  string   `json:"name"`
		Paths []string `json:"paths"`
	}
	if err := decodeArguments(invocation.Arguments, &args); err != nil {
		return Result{}, err
	}
	if (args.Name == contextbuilder.LayerTaint || args.Name == contextbuilder.LayerSecuritySurface) &&
		invocation.Scope.Role != RoleSecurityAuditor && invocation.Scope.Role != RoleSecurityAuditorDiscovery {
		return Result{}, ErrCapabilityDenied
	}
	files, err := validatedSnapshotPaths(ctx, invocation.Scope.Snapshot, args.Paths)
	if err != nil {
		return Result{}, err
	}
	patch := string(invocation.Scope.Snapshot.PatchContent())
	if len(files) > 0 {
		patch = syntheticSnapshotPatch(files)
	}
	var result contextbuilder.LayerResult
	err = withMaterializedSnapshot(ctx, invocation.Scope.Snapshot, func(root string) error {
		var analyzeErr error
		result, analyzeErr = r.layers.Analyze(ctx, args.Name, contextbuilder.BuildInput{
			RepoPath: root, PatchEventContent: patch,
		})
		return analyzeErr
	})
	if err != nil {
		return Result{}, err
	}
	return jsonResult(result)
}

func (r *Registry) handleSecurityTrace(ctx context.Context, invocation Invocation) (Result, error) {
	if r.securityTrace == nil {
		return Result{}, ErrHandlerUnavailable
	}
	var args struct {
		Kind  string   `json:"kind"`
		Paths []string `json:"paths"`
	}
	if err := decodeArguments(invocation.Arguments, &args); err != nil {
		return Result{}, err
	}
	files, err := validatedSnapshotPaths(ctx, invocation.Scope.Snapshot, args.Paths)
	if err != nil {
		return Result{}, err
	}
	var result contextbuilder.SecurityTraceResult
	err = withMaterializedSnapshot(ctx, invocation.Scope.Snapshot, func(root string) error {
		var analyzeErr error
		result, analyzeErr = r.securityTrace.Analyze(ctx, contextbuilder.SecurityTraceRequest{
			Kind: args.Kind, RepoPath: root, Patch: syntheticSnapshotPatch(files),
		})
		return analyzeErr
	})
	if err != nil {
		return Result{}, err
	}
	return jsonResult(result)
}

func validatedSnapshotPaths(ctx context.Context, snapshot *workspacesnapshot.Snapshot, requested []string) ([]string, error) {
	if len(requested) == 0 {
		entries, err := snapshot.List(".")
		if err != nil {
			return nil, err
		}
		requested = make([]string, 0, len(entries))
		for _, entry := range entries {
			requested = append(requested, entry.Path)
		}
	}
	seen := make(map[string]struct{}, len(requested))
	paths := make([]string, 0, len(requested))
	for _, path := range requested {
		if _, err := snapshot.ReadFile(ctx, path); err != nil {
			return nil, err
		}
		path = cleanDisplayPath(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func withMaterializedSnapshot(ctx context.Context, snapshot *workspacesnapshot.Snapshot, run func(string) error) error {
	root, err := os.MkdirTemp("", "drydock-agent-tool-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	if err := snapshot.Materialize(ctx, root); err != nil {
		return err
	}
	return run(root)
}

func syntheticSnapshotPatch(paths []string) string {
	var patch strings.Builder
	for _, path := range paths {
		fmt.Fprintf(&patch, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1,0 +1,1 @@\n+audit target\n", path, path, path, path)
	}
	return patch.String()
}

func handleSelection(ctx context.Context, invocation Invocation) (Result, error) {
	if invocation.Scope.Selection == nil {
		return Result{}, ErrHandlerUnavailable
	}
	return invocation.Scope.Selection.HandleSelectionTool(ctx, invocation.Name, invocation.Arguments, invocation.ToolCallID)
}

func handleReviewSubmit(ctx context.Context, invocation Invocation) (Result, error) {
	if invocation.Scope.Review == nil {
		return Result{}, ErrHandlerUnavailable
	}
	return invocation.Scope.Review.HandleReviewSubmit(ctx, invocation.Arguments, invocation.ToolCallID)
}

func decodeArguments(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInvocation, err)
	}
	if decoder.More() {
		return fmt.Errorf("%w: multiple JSON values", ErrInvalidInvocation)
	}
	return nil
}

func jsonResult(value any) (Result, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return Result{}, err
	}
	return Result{Content: string(data), Structured: data}, nil
}

func invocationDigest(name string, arguments json.RawMessage) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, arguments); err != nil {
		compact.Write(arguments)
	}
	sum := sha256.Sum256(append(append([]byte(name), 0), compact.Bytes()...))
	return hex.EncodeToString(sum[:])
}

func cloneResult(result Result) Result {
	result.Structured = append(json.RawMessage(nil), result.Structured...)
	return result
}

func limitResult(result Result, limit int) Result {
	if limit <= 0 {
		limit = DefaultMaxResultBytes
	}
	if len(result.Content)+len(result.Structured) <= limit {
		return result
	}
	result.Structured = nil
	result.Truncated = true
	if len(result.Content) <= limit {
		return result
	}
	content := result.Content[:limit]
	for len(content) > 0 && !utf8.ValidString(content) {
		content = content[:len(content)-1]
	}
	result.Content = content
	return result
}

func sortDefinitions(definitions []Definition) {
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
}

func cleanDisplayPath(path string) string {
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
}
