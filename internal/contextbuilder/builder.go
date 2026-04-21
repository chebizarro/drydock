package contextbuilder

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
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
)

type BuildInput struct {
	PatchEventContent string
	RepoPath          string
}

type ContextBundle struct {
	Content       string
	TokenBudget   int
	TokenCount    int
	LayersUsed    []string
	LayersDropped []string
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

type Builder struct {
	TokenBudget int
	Counter     TokenCounter
	Providers   []Provider
}

func NewDefault() *Builder {
	return &Builder{
		TokenBudget: DefaultTokenBudget,
		Counter:     DefaultTiktokenCounter(),
		Providers:   DefaultProviders(),
	}
}

func (b *Builder) Build(ctx context.Context, in BuildInput) (ContextBundle, error) {
	if b.Counter == nil {
		b.Counter = ApproxTokenCounter{}
	}
	if b.TokenBudget <= 0 {
		b.TokenBudget = DefaultTokenBudget
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

	return ContextBundle{
		Content:       strings.Join(parts, "\n\n"),
		TokenBudget:   b.TokenBudget,
		TokenCount:    usedTokens,
		LayersUsed:    usedNames,
		LayersDropped: dropped,
	}, nil
}

