package codeindex

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/contextbuilder"
	"git.sharegap.net/cascadia/drydock/internal/embedding"
	"git.sharegap.net/cascadia/drydock/internal/vectorstore"
)

const (
	layerName       = "symbols-related-code"
	layerPriority   = 3
	searchLimit     = 30
	resultLimit     = 10
	maxPerFile      = 2
	scoreThreshold  = 0.6
	maxQueryBytes   = 8 * 1024
	maxSnippetBytes = 1200
	maxLayerBytes   = 12 * 1024
)

// Provider is a context builder provider that retrieves semantically related
// code from the code_chunks Qdrant collection. It surfaces functions, methods,
// and types that may be affected by the changes in a patch, helping the
// reviewer catch ripple effects and missing updates.
type Provider struct {
	qdrant   *vectorstore.Client
	embedder *embedding.Client
	logger   *slog.Logger
}

// NewProvider creates a code index context builder provider.
// Returns nil if either client is nil (graceful degradation).
func NewProvider(qdrant *vectorstore.Client, embedder *embedding.Client, logger *slog.Logger) *Provider {
	if qdrant == nil || embedder == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{
		qdrant:   qdrant,
		embedder: embedder,
		logger:   logger,
	}
}

func (p *Provider) LayerName() string { return layerName }
func (p *Provider) Priority() int     { return layerPriority }

// Build searches the code_chunks collection for code related to the current
// patch but NOT in the changed files. This surfaces "you changed X but Y
// depends on X" insights.
func (p *Provider) Build(ctx context.Context, in contextbuilder.BuildInput) (string, error) {
	if in.RepoID == "" || strings.TrimSpace(in.PatchEventContent) == "" {
		return "", nil
	}

	// Truncate query text to avoid oversized embedding requests.
	query := in.PatchEventContent
	if len(query) > maxQueryBytes {
		query = query[:maxQueryBytes]
	}

	// Embed the patch diff.
	vec, err := p.embedder.Embed(ctx, query)
	if err != nil {
		p.logger.Warn("code index embed failed, skipping related-code layer",
			"repo_id", in.RepoID, "error", err)
		return "", nil // graceful degradation
	}

	// Search for semantically similar code in the same repo.
	filter := vectorstore.Filter(vectorstore.Match("repo_id", in.RepoID))

	results, err := p.qdrant.Search(ctx, p.qdrant.CollectionNames().CodeChunks, vec, searchLimit, filter)
	if err != nil {
		p.logger.Warn("code index search failed, skipping related-code layer",
			"repo_id", in.RepoID, "error", err)
		return "", nil // graceful degradation
	}

	if len(results) == 0 {
		return "", nil
	}

	// Build changed-files set for exclusion.
	// Collect both old (--- a/) and new (+++ b/) paths to handle renames.
	changedFiles := make(map[string]bool)
	for _, line := range strings.Split(in.PatchEventContent, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "+++ b/") {
			changedFiles[strings.TrimPrefix(line, "+++ b/")] = true
		} else if strings.HasPrefix(line, "--- a/") {
			changedFiles[strings.TrimPrefix(line, "--- a/")] = true
		}
	}

	// Post-filter results.
	type hit struct {
		filePath   string
		symbolName string
		symbolKind string
		startLine  int
		endLine    int
		content    string
		score      float32
	}

	fileHitCount := make(map[string]int)
	var hits []hit

	for _, r := range results {
		// Score threshold.
		if r.Score < scoreThreshold {
			continue
		}

		var chunk ChunkPayload
		if err := vectorstore.DecodePayload(r.Payload, &chunk); err != nil {
			continue
		}
		filePath := chunk.FilePath
		if filePath == "" {
			continue
		}

		// Exclude changed files.
		if changedFiles[filePath] {
			continue
		}

		// Workspace root filtering.
		if len(in.WorkspaceRoots) > 0 {
			inWorkspace := false
			for _, root := range in.WorkspaceRoots {
				if strings.HasPrefix(filePath, root+"/") || filePath == root {
					inWorkspace = true
					break
				}
			}
			if !inWorkspace {
				continue
			}
		}

		// Cap per-file hits.
		if fileHitCount[filePath] >= maxPerFile {
			continue
		}
		fileHitCount[filePath]++

		content := chunk.Content
		if len(content) > maxSnippetBytes {
			content = content[:maxSnippetBytes] + "\n// ... truncated"
		}

		hits = append(hits, hit{
			filePath:   filePath,
			symbolName: chunk.SymbolName,
			symbolKind: chunk.SymbolKind,
			startLine:  chunk.StartLine,
			endLine:    chunk.EndLine,
			content:    content,
			score:      r.Score,
		})

		if len(hits) >= resultLimit {
			break
		}
	}

	if len(hits) == 0 {
		return "", nil
	}

	// Render output.
	var sb strings.Builder
	sb.WriteString("Related code that may be affected by these changes:\n\n")

	for _, h := range hits {
		header := fmt.Sprintf("### %s (%s) — %s:%d-%d\n",
			h.symbolName, h.symbolKind, h.filePath, h.startLine, h.endLine)
		entry := header + h.content + "\n\n"

		if sb.Len()+len(entry) > maxLayerBytes {
			break
		}
		sb.WriteString(entry)
	}

	return sb.String(), nil
}
