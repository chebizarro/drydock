package codeindex

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"drydock/internal/contextbuilder"
	"drydock/internal/embedding"
	"drydock/internal/vectorstore"
)

const (
	layerName          = "symbols-related-code"
	layerPriority      = 3
	searchLimit        = 30
	resultLimit        = 10
	maxPerFile         = 2
	scoreThreshold     = 0.6
	maxQueryBytes      = 8 * 1024
	maxSnippetBytes    = 1200
	maxLayerBytes      = 12 * 1024
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
	filter := map[string]any{
		"must": []map[string]any{
			{"key": "repo_id", "match": map[string]any{"value": in.RepoID}},
		},
	}

	results, err := p.qdrant.Search(ctx, vectorstore.CollectionCodeChunks, vec, searchLimit, filter)
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

		filePath, _ := r.Payload["file_path"].(string)
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

		symbolName, _ := r.Payload["symbol_name"].(string)
		symbolKind, _ := r.Payload["symbol_kind"].(string)
		content, _ := r.Payload["content"].(string)
		startLine := payloadInt(r.Payload, "start_line")
		endLine := payloadInt(r.Payload, "end_line")

		if len(content) > maxSnippetBytes {
			content = content[:maxSnippetBytes] + "\n// ... truncated"
		}

		hits = append(hits, hit{
			filePath:   filePath,
			symbolName: symbolName,
			symbolKind: symbolKind,
			startLine:  startLine,
			endLine:    endLine,
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

// payloadInt extracts an integer from a Qdrant payload field.
// JSON numbers decode as float64 in map[string]any.
func payloadInt(payload map[string]any, key string) int {
	v, ok := payload[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}
