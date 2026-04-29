package contextbuilder

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"drydock/internal/lspbridge"
	"drydock/internal/symbols"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

// DefaultProviders returns the standard provider set, optionally enhanced
// with Qdrant retrieval and LSP-based analysis when services are configured.
func DefaultProviders(opts ...BuilderOptions) []Provider {
	var opt BuilderOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	srch := newSearcher()

	providers := []Provider{
		patchDiffProvider{},
		fileContextProvider{},
		changeImpactProvider{search: srch},
		symbolsCallsitesProvider{lspClient: opt.lspClient, search: srch},
		testsProvider{search: srch},
		importsExportsProvider{},
		commitHistoryProvider{},
		projectDocsProvider{},
	}

	// Add Qdrant retrieval provider if configured.
	if opt.qdrantProvider != nil {
		providers = append(providers, opt.qdrantProvider)
	}

	// Add any extra providers (e.g. security scanner).
	providers = append(providers, opt.extraProviders...)

	return providers
}

func parsePatch(content string) ([]*gitdiff.File, error) {
	files, _, err := gitdiff.Parse(strings.NewReader(content))
	if err != nil {
		return nil, err
	}
	return files, nil
}

type patchDiffProvider struct{}

func (patchDiffProvider) LayerName() string { return LayerPatchDiff }
func (patchDiffProvider) Priority() int     { return 1 }
func (patchDiffProvider) Build(_ context.Context, in BuildInput) (string, error) {
	diff := strings.TrimSpace(in.PatchEventContent)
	if diff == "" {
		return "", nil
	}
	b := []byte(diff)
	if len(b) > 40*1024 {
		b = b[:40*1024]
	}
	return string(b), nil
}

type fileContextProvider struct{}

func (fileContextProvider) LayerName() string { return LayerFileContext }
func (fileContextProvider) Priority() int     { return 2 }
func (fileContextProvider) Build(_ context.Context, in BuildInput) (string, error) {
	if in.RepoPath == "" {
		return "", nil
	}
	files, err := parsePatch(in.PatchEventContent)
	if err != nil {
		return "", nil
	}

	var out strings.Builder
	for _, f := range files {
		path := pickPath(f)
		if path == "" || isExcludedPath(path) {
			continue
		}
		abs := filepath.Join(in.RepoPath, path)
		content, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		if !isProbablyText(content) {
			continue
		}
		if out.Len() > 20*1024 {
			break
		}
		if len(content) > 4*1024 {
			content = content[:4*1024]
		}
		out.WriteString("### ")
		out.WriteString(path)
		out.WriteString("\n")
		out.Write(content)
		out.WriteString("\n\n")
	}
	return strings.TrimSpace(out.String()), nil
}

type symbolsCallsitesProvider struct {
	lspClient *lspbridge.Client   // optional LSP bridge for type-aware analysis
	search    *searcher           // ripgrep with git grep fallback
}

func (p symbolsCallsitesProvider) LayerName() string { return LayerSymbolsCallsites }
func (p symbolsCallsitesProvider) Priority() int     { return 3 }
func (p symbolsCallsitesProvider) Build(ctx context.Context, in BuildInput) (string, error) {
	if in.RepoPath == "" {
		return "", nil
	}

	// Create per-call extractor (not shared — tree-sitter parser is mutable).
	extractor := symbols.New()
	defer extractor.Close()

	// Try tree-sitter extraction first for accurate, AST-based symbol detection.
	// Falls back to regex for unsupported languages or when tree-sitter is unavailable.
	syms := p.extractWithTreeSitter(in, extractor)
	if len(syms) == 0 {
		syms = extractChangedSymbols(in.PatchEventContent)
	}
	if len(syms) == 0 {
		return "", nil
	}

	var out strings.Builder
	out.WriteString("symbols: ")
	out.WriteString(strings.Join(syms, ", "))
	out.WriteString("\n")

	// Try LSP bridge for type-aware definitions and references when available.
	// Falls back to git grep on any error (bridge down, timeout, etc.).
	if p.lspClient != nil {
		if lspContent := p.queryLSP(ctx, in, syms); lspContent != "" {
			out.WriteString(lspContent)
			return strings.TrimSpace(out.String()), nil
		}
	}

	// Fallback: git grep / ripgrep search.
	srch := p.search
	if srch == nil {
		srch = newSearcher()
	}

	for _, sym := range syms {
		lines, err := srch.SearchSymbol(ctx, in.RepoPath, sym, in.WorkspaceRoots)
		if err != nil || lines == "" {
			continue
		}
		out.WriteString("\n#### ")
		out.WriteString(sym)
		out.WriteString("\n")
		out.WriteString(lines)
		out.WriteString("\n")
	}
	return strings.TrimSpace(out.String()), nil
}

// extractWithTreeSitter parses changed files from the diff using tree-sitter
// and returns symbol names that overlap with changed line ranges.
func (p symbolsCallsitesProvider) extractWithTreeSitter(in BuildInput, extractor *symbols.Extractor) []string {
	if extractor == nil {
		return nil
	}

	files, err := parsePatch(in.PatchEventContent)
	if err != nil {
		return nil
	}

	var names []string
	for _, f := range files {
		path := pickPath(f)
		if path == "" || isExcludedPath(path) {
			continue
		}

		ext := filepath.Ext(path)
		lang := symbols.LangFromExt(ext)
		if lang == "" {
			continue
		}

		abs := filepath.Join(in.RepoPath, path)
		source, err := os.ReadFile(abs)
		if err != nil {
			continue
		}

		changedLines := extractChangedLineNumbers(f)
		result, err := extractor.ExtractChanged(lang, source, changedLines)
		if err != nil {
			continue
		}

		for _, s := range result {
			names = append(names, s.Name)
		}
	}

	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) > 12 {
		names = names[:12]
	}
	return names
}

// queryLSP calls the LSP bridge for type-aware symbol analysis.
// Returns formatted context string, or empty string on any error (caller falls
// back to git grep). Changed files are extracted from the patch diff.
func (p symbolsCallsitesProvider) queryLSP(ctx context.Context, in BuildInput, syms []string) string {
	files, err := parsePatch(in.PatchEventContent)
	if err != nil {
		return ""
	}
	var changedFiles []string
	for _, f := range files {
		path := pickPath(f)
		if path != "" {
			changedFiles = append(changedFiles, path)
		}
	}

	resp, err := p.lspClient.Analyze(ctx, lspbridge.AnalyzeRequest{
		RepoPath:     in.RepoPath,
		ChangedFiles: changedFiles,
		Symbols:      syms,
	})
	if err != nil {
		return ""
	}
	if resp.Error != "" {
		return ""
	}

	// Only use LSP results if they returned something useful.
	if len(resp.Definitions) == 0 && len(resp.References) == 0 && len(resp.Diagnostics) == 0 {
		return ""
	}

	var out strings.Builder

	// Definitions: group by symbol for readability.
	if len(resp.Definitions) > 0 {
		out.WriteString("\n## Definitions (LSP)\n")
		for _, def := range resp.Definitions {
			out.WriteString(fmt.Sprintf("- **%s** (%s) — %s:%d", def.Name, def.Kind, def.File, def.Line))
			if def.Detail != "" {
				out.WriteString(fmt.Sprintf(" `%s`", def.Detail))
			}
			out.WriteString("\n")
		}
	}

	// References: show up to 20 to keep context within budget.
	if len(resp.References) > 0 {
		out.WriteString("\n## References (LSP)\n")
		refs := resp.References
		if len(refs) > 20 {
			refs = refs[:20]
		}
		for _, ref := range refs {
			out.WriteString(fmt.Sprintf("- %s → %s:%d:%d\n", ref.Symbol, ref.File, ref.Line, ref.Column))
		}
		if len(resp.References) > 20 {
			out.WriteString(fmt.Sprintf("- ... and %d more references\n", len(resp.References)-20))
		}
	}

	// Diagnostics from the language server (warnings/errors in changed files).
	if len(resp.Diagnostics) > 0 {
		out.WriteString("\n## Diagnostics (LSP)\n")
		diags := resp.Diagnostics
		if len(diags) > 10 {
			diags = diags[:10]
		}
		for _, d := range diags {
			out.WriteString(fmt.Sprintf("- [%s] %s:%d — %s (%s)\n", d.Severity, d.File, d.Line, d.Message, d.Source))
		}
		if len(resp.Diagnostics) > 10 {
			out.WriteString(fmt.Sprintf("- ... and %d more diagnostics\n", len(resp.Diagnostics)-10))
		}
	}

	return out.String()
}

// extractChangedLineNumbers returns 0-based line numbers from the new file
// that correspond to added lines in the diff.
func extractChangedLineNumbers(f *gitdiff.File) []uint32 {
	var lines []uint32
	for _, frag := range f.TextFragments {
		if frag == nil {
			continue
		}
		// NewPosition is 1-based; tree-sitter uses 0-based
		newLine := uint32(frag.NewPosition) - 1
		for _, line := range frag.Lines {
			switch line.Op {
			case gitdiff.OpAdd:
				lines = append(lines, newLine)
				newLine++
			case gitdiff.OpDelete:
				// Deleted lines don't exist in the new file
			default: // context
				newLine++
			}
		}
	}
	return lines
}

type testsProvider struct {
	search *searcher
}

func (p testsProvider) LayerName() string { return LayerTests }
func (p testsProvider) Priority() int     { return 4 }
func (p testsProvider) Build(ctx context.Context, in BuildInput) (string, error) {
	if in.RepoPath == "" {
		return "", nil
	}
	symbols := extractChangedSymbols(in.PatchEventContent)
	if len(symbols) == 0 {
		return "", nil
	}
	var out strings.Builder
	srch := p.search
	if srch == nil {
		srch = newSearcher()
	}

	var uncovered []string
	for _, sym := range symbols {
		lines, _ := srch.SearchSymbolTests(ctx, in.RepoPath, sym, in.WorkspaceRoots)
		if strings.TrimSpace(lines) == "" {
			uncovered = append(uncovered, sym)
			continue
		}
		out.WriteString("### ")
		out.WriteString(sym)
		out.WriteString("\n")
		out.WriteString(lines)
		out.WriteString("\n")
	}

	// Explicitly flag symbols without test coverage as finding candidates
	// so the reviewer considers missing tests rather than silently ignoring them.
	if len(uncovered) > 0 {
		out.WriteString("\n")
		for _, sym := range uncovered {
			out.WriteString(TestCoverageGapPrefix)
			out.WriteString(sym)
			out.WriteString("\n")
		}
	}

	return strings.TrimSpace(out.String()), nil
}

type importsExportsProvider struct{}

func (importsExportsProvider) LayerName() string { return LayerImportsExports }
func (importsExportsProvider) Priority() int     { return 5 }
func (importsExportsProvider) Build(_ context.Context, in BuildInput) (string, error) {
	lines := extractImportExportLines(in.PatchEventContent)
	if len(lines) == 0 {
		return "", nil
	}
	return strings.Join(lines, "\n"), nil
}

type commitHistoryProvider struct{}

func (commitHistoryProvider) LayerName() string { return LayerCommitHistory }
func (commitHistoryProvider) Priority() int     { return 6 }
func (commitHistoryProvider) Build(ctx context.Context, in BuildInput) (string, error) {
	if in.RepoPath == "" {
		return "", nil
	}
	files, err := parsePatch(in.PatchEventContent)
	if err != nil {
		return "", nil
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		path := pickPath(f)
		if path == "" || isExcludedPath(path) {
			continue
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return "", nil
	}

	args := []string{"-C", in.RepoPath, "log", "--oneline", "-n", "10", "--"}
	args = append(args, paths...)
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

type projectDocsProvider struct{}

func (projectDocsProvider) LayerName() string { return LayerProjectDocs }
func (projectDocsProvider) Priority() int     { return 7 }
func (projectDocsProvider) Build(_ context.Context, in BuildInput) (string, error) {
	if in.RepoPath == "" {
		return "", nil
	}

	candidates := []string{
		"CONTRIBUTING.md",
		"README.md",
		"docs/CONTRIBUTING.md",
		"docs/style-guide.md",
		"docs/STYLEGUIDE.md",
	}

	// Also check workspace-local docs when in a monorepo
	if len(in.WorkspaceRoots) > 0 {
		var wsCandidates []string
		for _, root := range in.WorkspaceRoots {
			wsCandidates = append(wsCandidates,
				filepath.Join(root, "README.md"),
				filepath.Join(root, "CONTRIBUTING.md"),
			)
		}
		// Workspace docs first, then repo-level docs
		candidates = append(wsCandidates, candidates...)
	}

	var out bytes.Buffer
	for _, rel := range candidates {
		path := filepath.Join(in.RepoPath, rel)
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 {
			continue
		}
		if !isProbablyText(data) {
			continue
		}
		if out.Len() > 15*1024 {
			break
		}
		if len(data) > 4*1024 {
			data = data[:4*1024]
		}
		out.WriteString("### ")
		out.WriteString(rel)
		out.WriteString("\n")
		out.Write(data)
		out.WriteString("\n\n")
	}
	return strings.TrimSpace(out.String()), nil
}

func pickPath(f *gitdiff.File) string {
	if f == nil {
		return ""
	}
	if f.NewName != "" && f.NewName != "/dev/null" {
		return f.NewName
	}
	if f.OldName != "" && f.OldName != "/dev/null" {
		return f.OldName
	}
	return ""
}

func extractChangedSymbols(diff string) []string {
	re := regexp.MustCompile(`(?m)^[+-]\s*(?:func|type|class|def)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	matches := re.FindAllStringSubmatch(diff, 32)
	syms := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		syms = append(syms, m[1])
	}
	slices.Sort(syms)
	syms = slices.Compact(syms)
	if len(syms) > 12 {
		syms = syms[:12]
	}
	return syms
}

func extractImportExportLines(diff string) []string {
	re := regexp.MustCompile(`(?m)^[+-]\s*(?:import|export|from|use|#include)\b.*$`)
	lines := re.FindAllString(diff, 100)
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	lines = slices.Compact(lines)
	return lines
}


func runGit(ctx context.Context, repoPath string, args ...string) (string, error) {
	full := append([]string{"-C", repoPath}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func isExcludedPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".proto") {
		return true
	}
	switch base {
	case "package-lock.json", "cargo.lock", "poetry.lock", "pnpm-lock.yaml", "bun.lock", "yarn.lock":
		return true
	}
	if strings.Contains(lower, "__generated__") || strings.Contains(lower, "generated/graphql") {
		return true
	}
	if strings.Contains(lower, "migration") && strings.Contains(lower, "snapshot") {
		return true
	}
	return false
}

func isProbablyText(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	if bytes.IndexByte(data, 0x00) >= 0 {
		return false
	}
	return true
}

