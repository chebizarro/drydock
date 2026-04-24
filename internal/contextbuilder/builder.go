package contextbuilder

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"drydock/internal/lspbridge"
)

const DefaultTokenBudget = 64_000

const (
	LayerPatchDiff        = "patch"
	LayerFileContext      = "modified-files"
	LayerSymbolsCallsites = "symbols"
	LayerTests            = "tests"
	LayerImportsExports   = "imports-exports"
	LayerCommitHistory    = "commit-history"
	LayerProjectDocs      = "project-docs"

	// TestCoverageGapPrefix is the line prefix used by the tests provider to
	// mark symbols that have no test references. The builder scans for these
	// to populate ContextBundle.TestCoverageGaps.
	TestCoverageGapPrefix = "FINDING-CANDIDATE: no test coverage for "
)

type BuildInput struct {
	PatchEventContent string
	RepoPath          string
	// RepoID is the unique repository identifier. Used by the Qdrant provider
	// to filter project_docs results to the current repository.
	RepoID string
	// WorkspaceRoots are relative paths of detected monorepo workspaces that
	// contain changed files. Empty means "whole repo" (no workspace isolation).
	// Set automatically by Builder.Build when a workspace config is detected.
	WorkspaceRoots []string

	// TokenBudgetOverride, if > 0, overrides the builder's default token budget
	// for this build only. The effective budget is min(override, builder default).
	TokenBudgetOverride int
	// ExcludePaths are repo-config-specified path patterns to exclude from review.
	// These are merged with the built-in exclusions (lock files, generated code).
	ExcludePaths []string
	// DisableDocs, if true, skips documentation providers and marks them as dropped.
	DisableDocs bool
}

type ContextBundle struct {
	Content       string
	TokenBudget   int
	TokenCount    int
	LayersUsed    []string
	LayersDropped []string
	// ExcludedFiles lists paths of changed files that were excluded from
	// review (e.g. lock files, generated code, .proto files). The reviewer
	// LLM sees a notification about these so it can flag dependency or
	// schema changes that deserve human attention.
	ExcludedFiles []string
	// TestCoverageGaps lists symbol names that were modified by the patch
	// but have no test references. Surfaced as finding candidates.
	TestCoverageGaps []string
	// ChangedFiles lists the reviewable changed file paths extracted from
	// the filtered patch diff, before token budgeting. This is authoritative
	// and should be used instead of scraping the rendered bundle content.
	ChangedFiles []string
}

type TokenCounter interface {
	Count(text string) int
}

type ApproxTokenCounter struct{}

func (ApproxTokenCounter) Count(text string) int {
	if text == "" {
		return 0
	}
	runeCount := utf8.RuneCountInString(text)
	if runeCount < 0 || runeCount > 1<<30 {
		return 1 << 30
	}
	return (runeCount + 3) / 4
}

type Provider interface {
	LayerName() string
	Priority() int
	Build(ctx context.Context, in BuildInput) (string, error)
}

// BuilderOptions configures optional service clients for enhanced analysis.
// All fields are optional — nil means the feature is disabled.
type BuilderOptions struct {
	QdrantClient  interface{ /* *vectorstore.Client */ } // nil = no Qdrant retrieval
	EmbedClient   interface{ /* *embedding.Client */ }   // nil = no embedding

	// Typed accessors set internally. Use the With* helpers.
	qdrantProvider Provider
	lspClient      *lspbridge.Client
}

type Builder struct {
	TokenBudget int
	Counter     TokenCounter
	Providers   []Provider
}

// NewDefault creates a builder with no optional services.
func NewDefault() *Builder {
	return NewWithOptions(BuilderOptions{})
}

// NewWithOptions creates a builder with optional service clients.
func NewWithOptions(opts BuilderOptions) *Builder {
	return &Builder{
		TokenBudget: DefaultTokenBudget,
		Counter:     DefaultTiktokenCounter(),
		Providers:   DefaultProviders(opts),
	}
}

// isDocLayer returns true if the layer name is a documentation provider.
func isDocLayer(name string) bool {
	return name == LayerProjectDocs || name == "qdrant-docs"
}

// matchesExcludePath checks if a file path matches any of the exclusion patterns.
func matchesExcludePath(filePath string, patterns []string) bool {
	for _, pattern := range patterns {
		// Recursive directory prefix: "vendor/**" matches "vendor/foo/bar.go"
		if strings.HasSuffix(pattern, "/**") {
			prefix := strings.TrimSuffix(pattern, "/**")
			if strings.HasPrefix(filePath, prefix+"/") || filePath == prefix {
				return true
			}
			continue
		}
		// path.Match-style glob
		if matched, _ := filepath.Match(pattern, filePath); matched {
			return true
		}
		// Also try matching just the filename for patterns like "*.lock"
		base := filepath.Base(filePath)
		if matched, _ := filepath.Match(pattern, base); matched {
			return true
		}
	}
	return false
}

func (b *Builder) Build(ctx context.Context, in BuildInput) (ContextBundle, error) {
	if b.Counter == nil {
		b.Counter = ApproxTokenCounter{}
	}

	// Compute per-build effective budget (never mutate b.TokenBudget).
	effectiveBudget := b.TokenBudget
	if effectiveBudget <= 0 {
		effectiveBudget = DefaultTokenBudget
	}
	if in.TokenBudgetOverride > 0 && in.TokenBudgetOverride < effectiveBudget {
		effectiveBudget = in.TokenBudgetOverride
	}

	// Auto-detect workspace boundaries for monorepo context isolation.
	if in.RepoPath != "" && len(in.WorkspaceRoots) == 0 {
		workspaces := DetectWorkspaces(in.RepoPath)
		if len(workspaces) > 0 {
			files, _ := parsePatch(in.PatchEventContent)
			var changedPaths []string
			for _, f := range files {
				if p := pickPath(f); p != "" {
					changedPaths = append(changedPaths, p)
				}
			}
			in.WorkspaceRoots = RelevantWorkspaces(workspaces, changedPaths)
		}
	}

	// Pre-scan: identify excluded files (built-in + repo-config) and filter
	// the patch to remove excluded file diffs from the context.
	var excludedFiles []string
	if in.PatchEventContent != "" {
		patchFiles, _ := parsePatch(in.PatchEventContent)
		for _, f := range patchFiles {
			path := pickPath(f)
			if path == "" {
				continue
			}
			if isExcludedPath(path) || matchesExcludePath(path, in.ExcludePaths) {
				excludedFiles = append(excludedFiles, path)
			}
		}
		// Rebuild a filtered patch so all providers see the same filtered diff.
		if len(excludedFiles) > 0 {
			in = filterPatchInput(in, excludedFiles)
		}
	}

	type layer struct {
		name     string
		priority int
		content  string
		tokens   int
	}

	// Extract changed files from the (possibly filtered) patch input.
	// This is done before provider execution and token budgeting, so it
	// captures the true set of reviewable files.
	var changedFiles []string
	if in.PatchEventContent != "" {
		postFilterFiles, _ := parsePatch(in.PatchEventContent)
		for _, f := range postFilterFiles {
			if p := pickPath(f); p != "" {
				changedFiles = append(changedFiles, p)
			}
		}
	}

	// Track doc layers that were disabled by repo config.
	var docDropped []string

	layers := make([]layer, 0, len(b.Providers))
	for _, p := range b.Providers {
		// Skip doc layers if DisableDocs is set.
		if in.DisableDocs && isDocLayer(p.LayerName()) {
			docDropped = append(docDropped, p.LayerName())
			continue
		}
		content, err := p.Build(ctx, in)
		if err != nil {
			return ContextBundle{}, fmt.Errorf("build %s layer: %w", p.LayerName(), err)
		}
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		layers = append(layers, layer{
			name:     p.LayerName(),
			priority: p.Priority(),
			content:  content,
			tokens:   b.Counter.Count(content),
		})
	}

	slices.SortStableFunc(layers, func(a, c layer) int {
		if a.priority != c.priority {
			if a.priority < c.priority {
				return -1
			}
			return 1
		}
		if a.name < c.name {
			return -1
		}
		if a.name > c.name {
			return 1
		}
		return 0
	})

	usedTokens := 0
	used := make([]layer, 0, len(layers))
	dropped := make([]string, 0, len(layers))

	for i, lr := range layers {
		if usedTokens+lr.tokens > effectiveBudget {
			// hard stop policy: once budget hit, this and all lower-priority layers are dropped
			for _, d := range layers[i:] {
				dropped = append(dropped, d.name)
			}
			break
		}
		usedTokens += lr.tokens
		used = append(used, lr)
	}

	parts := make([]string, 0, len(used))
	usedNames := make([]string, 0, len(used))
	for _, lr := range used {
		parts = append(parts, "## "+lr.name+"\n"+lr.content)
		usedNames = append(usedNames, lr.name)
	}

	// Scan the tests layer for coverage gap markers and extract symbol names.
	var testCoverageGaps []string
	for _, lr := range used {
		if lr.name != LayerTests {
			continue
		}
		for _, line := range strings.Split(lr.content, "\n") {
			if strings.HasPrefix(line, TestCoverageGapPrefix) {
				sym := strings.TrimPrefix(line, TestCoverageGapPrefix)
				sym = strings.TrimSpace(sym)
				if sym != "" {
					testCoverageGaps = append(testCoverageGaps, sym)
				}
			}
		}
	}

	// Append a notification about excluded non-source files so the reviewer
	// can flag dependency / schema changes that deserve human attention.
	if len(excludedFiles) > 0 {
		note := "## excluded-files\n" +
			"The following non-source files were modified but not reviewed: " +
			strings.Join(excludedFiles, ", ") + ".\n" +
			"If these are dependency lock files, generated code, or schema definitions, " +
			"note that changes may have security or compatibility implications."
		noteTokens := b.Counter.Count(note)
		if usedTokens+noteTokens <= effectiveBudget {
			parts = append(parts, note)
			usedTokens += noteTokens
		}
	}

	// Append doc layers that were disabled by repo config to the dropped list.
	dropped = append(dropped, docDropped...)

	return ContextBundle{
		Content:          strings.Join(parts, "\n\n"),
		TokenBudget:      effectiveBudget,
		TokenCount:       usedTokens,
		LayersUsed:       usedNames,
		LayersDropped:    dropped,
		ExcludedFiles:    excludedFiles,
		TestCoverageGaps: testCoverageGaps,
		ChangedFiles:     changedFiles,
	}, nil
}

// filterPatchInput returns a copy of in with PatchEventContent filtered to
// exclude diffs for the given excluded files. Works at the raw text level
// by splitting on "diff --git" boundaries.
func filterPatchInput(in BuildInput, excludedFiles []string) BuildInput {
	excluded := make(map[string]bool, len(excludedFiles))
	for _, f := range excludedFiles {
		excluded[f] = true
	}

	// Split the patch into per-file sections using "diff --git" as delimiter.
	lines := strings.Split(in.PatchEventContent, "\n")
	var sections [][]string
	var current []string
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			if len(current) > 0 {
				sections = append(sections, current)
			}
			current = []string{line}
		} else {
			current = append(current, line)
		}
	}
	if len(current) > 0 {
		sections = append(sections, current)
	}

	var kept []string
	for _, section := range sections {
		if len(section) == 0 {
			continue
		}
		// Extract file paths from "diff --git a/path b/path" or "+++ b/path".
		skip := false
		for _, line := range section {
			if strings.HasPrefix(line, "+++ b/") {
				path := strings.TrimPrefix(line, "+++ b/")
				if excluded[path] {
					skip = true
					break
				}
			}
			if strings.HasPrefix(line, "--- a/") {
				path := strings.TrimPrefix(line, "--- a/")
				if excluded[path] {
					skip = true
					break
				}
			}
		}
		if !skip {
			kept = append(kept, strings.Join(section, "\n"))
		}
	}

	result := in
	result.PatchEventContent = strings.Join(kept, "\n")
	return result
}

