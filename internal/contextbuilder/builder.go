package contextbuilder

import (
	"context"
	"fmt"
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

func (b *Builder) Build(ctx context.Context, in BuildInput) (ContextBundle, error) {
	if b.Counter == nil {
		b.Counter = ApproxTokenCounter{}
	}
	if b.TokenBudget <= 0 {
		b.TokenBudget = DefaultTokenBudget
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

	// Pre-scan: identify excluded files so we can notify the reviewer.
	var excludedFiles []string
	if in.PatchEventContent != "" {
		patchFiles, _ := parsePatch(in.PatchEventContent)
		for _, f := range patchFiles {
			path := pickPath(f)
			if path != "" && isExcludedPath(path) {
				excludedFiles = append(excludedFiles, path)
			}
		}
	}

	type layer struct {
		name     string
		priority int
		content  string
		tokens   int
	}

	layers := make([]layer, 0, len(b.Providers))
	for _, p := range b.Providers {
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
		if usedTokens+lr.tokens > b.TokenBudget {
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
		if usedTokens+noteTokens <= b.TokenBudget {
			parts = append(parts, note)
			usedTokens += noteTokens
		}
	}

	return ContextBundle{
		Content:          strings.Join(parts, "\n\n"),
		TokenBudget:      b.TokenBudget,
		TokenCount:       usedTokens,
		LayersUsed:       usedNames,
		LayersDropped:    dropped,
		ExcludedFiles:    excludedFiles,
		TestCoverageGaps: testCoverageGaps,
	}, nil
}

