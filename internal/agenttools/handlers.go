package agenttools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
		{definition(ToolTestsSearch, "Search test files in the frozen snapshot.", CapabilityRead, `{"type":"object","properties":{"query":{"type":"string"},"path":{"type":"string"},"regex":{"type":"boolean"},"max_results":{"type":"integer","minimum":1,"maximum":1000}},"required":["query"],"additionalProperties":false}`), handleTestsSearch},
		{definition(ToolGitRead, "Run one closed read-only git action (diff, show, log, blame) at the pinned commit.", CapabilityRead, `{"type":"object","properties":{"action":{"type":"string","enum":["diff","show","log","blame"]},"path":{"type":"string"},"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1,"maximum":100}},"required":["action"],"additionalProperties":false}`), handleGitRead},
		{definition(ToolSelectionAdd, "Add file, line-range, or codemap artifacts to the run-local selection.", CapabilitySelectionMutate, `{"type":"object","properties":{"artifacts":{"type":"array","items":{"type":"object"}}},"required":["artifacts"],"additionalProperties":false}`), handleSelection},
		{definition(ToolSelectionRemove, "Remove non-mandatory artifacts from the run-local selection.", CapabilitySelectionMutate, `{"type":"object","properties":{"artifacts":{"type":"array","items":{"type":"object"}}},"required":["artifacts"],"additionalProperties":false}`), handleSelection},
		{definition(ToolSelectionStatus, "Report the run-local selection and budget status.", CapabilitySelectionRead, `{"type":"object","additionalProperties":false}`), handleSelection},
		{definition(ToolSelectionFinalize, "Render, hash-verify, and exact-token-check the immutable context package.", CapabilitySelectionFinalize, `{"type":"object","additionalProperties":false}`), handleSelection},
		{definition(ToolReviewSubmit, "Submit the final structured review.", CapabilityReviewSubmit, `{"type":"object","properties":{"findings":{"type":"array"},"coverage":{"type":"string"}},"required":["findings","coverage"],"additionalProperties":true}`), handleReviewSubmit},
	}
	for _, item := range definitions {
		if err := registry.Register(item.def, item.handler); err != nil {
			panic(err)
		}
	}
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
