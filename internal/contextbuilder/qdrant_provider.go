package contextbuilder

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"drydock/internal/embedding"
	"drydock/internal/vectorstore"
)

const (
	LayerQdrantDocs = "qdrant-docs"

	// maxQueryTextLen limits the text sent for embedding to avoid excessive tokens.
	maxQueryTextLen = 4096

	// resultsPerCollection is the number of top results to fetch per Qdrant collection.
	resultsPerCollection = 3
)

// QdrantProvider retrieves relevant documents from Qdrant collections
// at context build time. It embeds the patch diff and queries:
//   - nip_specs: if the patch touches Nostr-related code (detected by imports/keywords)
//   - project_docs: always
//
// Results are injected into the context bundle with NIP specs first.
type QdrantProvider struct {
	qdrant   *vectorstore.Client
	embedder *embedding.Client
}

// NewQdrantProvider creates a provider that retrieves context from Qdrant.
// Returns nil if either client is nil (graceful degradation).
func NewQdrantProvider(qdrant *vectorstore.Client, embedder *embedding.Client) *QdrantProvider {
	if qdrant == nil || embedder == nil {
		return nil
	}
	return &QdrantProvider{
		qdrant:   qdrant,
		embedder: embedder,
	}
}

func (p *QdrantProvider) LayerName() string { return LayerQdrantDocs }
func (p *QdrantProvider) Priority() int     { return 8 }

func (p *QdrantProvider) Build(ctx context.Context, in BuildInput) (string, error) {
	// Embed the patch diff as the query vector.
	queryText := in.PatchEventContent
	if len(queryText) > maxQueryTextLen {
		queryText = queryText[:maxQueryTextLen]
	}
	if strings.TrimSpace(queryText) == "" {
		return "", nil
	}

	vec, err := p.embedder.Embed(ctx, queryText)
	if err != nil {
		return "", fmt.Errorf("embed query: %w", err)
	}

	var out strings.Builder
	var searchErrors []error

	// Query nip_specs if the patch looks Nostr-related.
	if looksNostrRelated(in.PatchEventContent) {
		results, err := p.qdrant.Search(ctx, vectorstore.CollectionNIPSpecs, vec, resultsPerCollection, nil)
		if err != nil {
			searchErrors = append(searchErrors, fmt.Errorf("search %s: %w", vectorstore.CollectionNIPSpecs, err))
		} else if len(results) > 0 {
			out.WriteString("### NIP Specifications\n\n")
			for _, r := range results {
				nipID, _ := r.Payload["nip_id"].(string)
				section, _ := r.Payload["section_title"].(string)
				content, _ := r.Payload["content"].(string)
				if content == "" {
					continue
				}
				out.WriteString(fmt.Sprintf("**NIP-%s: %s** (relevance: %.2f)\n", nipID, section, r.Score))
				out.WriteString(content)
				out.WriteString("\n\n")
			}
		}
	}

	// Query project_docs always, filtered by repo_id when available.
	var docsFilter map[string]any
	if in.RepoID != "" {
		docsFilter = map[string]any{
			"must": []map[string]any{
				{"key": "repo_id", "match": map[string]any{"value": in.RepoID}},
			},
		}
	}
	results, err := p.qdrant.Search(ctx, vectorstore.CollectionProjectDocs, vec, resultsPerCollection, docsFilter)
	if err != nil {
		searchErrors = append(searchErrors, fmt.Errorf("search %s: %w", vectorstore.CollectionProjectDocs, err))
	} else if len(results) > 0 {
		out.WriteString("### Retrieved Project Documentation\n\n")
		for _, r := range results {
			title, _ := r.Payload["section_title"].(string)
			content, _ := r.Payload["content"].(string)
			if content == "" {
				continue
			}
			if title != "" {
				out.WriteString(fmt.Sprintf("**%s** (relevance: %.2f)\n", title, r.Score))
			}
			out.WriteString(content)
			out.WriteString("\n\n")
		}
	}

	content := strings.TrimSpace(out.String())
	if len(searchErrors) > 0 {
		return content, fmt.Errorf("qdrant retrieval degraded: %w", errors.Join(searchErrors...))
	}
	return content, nil
}

// nostrKeywords are terms that suggest the patch touches Nostr-related code.
var nostrKeywords = []string{
	"nostr", "nip-", "nip46", "nip44", "nip04",
	"kind 1", "kind 3", "kind 5", "kind 6", "kind 7",
	"fiatjaf.com/nostr", "nostr-tools", "nostr-sdk",
	"npub", "nsec", "nprofile", "nevent", "naddr",
	"relay", "wss://",
	"30617", "30618", "1617", "1618", "1619",
	"1621", "1622", "1111",
	"bunker://",
}

// looksNostrRelated checks if the patch content contains Nostr-related keywords.
func looksNostrRelated(content string) bool {
	lower := strings.ToLower(content)
	for _, kw := range nostrKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
