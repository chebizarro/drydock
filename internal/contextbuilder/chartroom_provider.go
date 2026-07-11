package contextbuilder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	LayerChartroomDocs    = "chartroom-docs"
	chartroomDefaultLimit = 6
)

// AuditEmitter is the small audit sink surface used by context providers.
// publisher.AuditPublisher satisfies this interface without making the context
// builder depend on the publisher package.
type AuditEmitter interface {
	EmitAudit(ctx context.Context, action, subject string, tags map[string]string) error
}

// ChartRoomConfig configures the Chartroom-backed context provider.
type ChartRoomConfig struct {
	BaseURL     string
	BearerToken string
	CorpusIDs   []string
	SourceIDs   []string
	Limit       int
	HTTPClient  *http.Client
	Audit       AuditEmitter
}

// ChartRoomProvider retrieves ranked documentation chunks from Chartroom's
// strict /search HTTP endpoint.
type ChartRoomProvider struct {
	baseURL     string
	bearerToken string
	corpusIDs   []string
	sourceIDs   []string
	limit       int
	client      *http.Client
	audit       AuditEmitter
}

// NewChartRoomProvider creates a Chartroom-backed provider. It returns nil when
// no base URL is configured so callers can gracefully fall back in development.
func NewChartRoomProvider(cfg ChartRoomConfig) *ChartRoomProvider {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil
	}
	limit := cfg.Limit
	if limit <= 0 {
		limit = chartroomDefaultLimit
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &ChartRoomProvider{
		baseURL:     strings.TrimRight(base, "/"),
		bearerToken: strings.TrimSpace(cfg.BearerToken),
		corpusIDs:   append([]string(nil), cfg.CorpusIDs...),
		sourceIDs:   append([]string(nil), cfg.SourceIDs...),
		limit:       limit,
		client:      client,
		audit:       cfg.Audit,
	}
}

func (p *ChartRoomProvider) LayerName() string { return LayerChartroomDocs }
func (p *ChartRoomProvider) Priority() int     { return 8 }

func (p *ChartRoomProvider) Build(ctx context.Context, in BuildInput) (string, error) {
	queryText := in.PatchEventContent
	if len(queryText) > maxQueryTextLen {
		queryText = queryText[:maxQueryTextLen]
	}
	queryText = strings.TrimSpace(queryText)
	if queryText == "" {
		return "", nil
	}

	resp, endpoint, err := p.search(ctx, queryText)
	if err != nil {
		return "", err
	}
	if len(resp.Results) == 0 {
		return "", nil
	}

	if p.audit != nil {
		tags := map[string]string{
			"endpoint": endpoint,
			"results":  fmt.Sprintf("%d", len(resp.Results)),
		}
		if in.RepoID != "" {
			tags["repo_id"] = in.RepoID
		}
		if err := p.audit.EmitAudit(ctx, "chartroom-context-retrieved", endpoint, tags); err != nil {
			return "", fmt.Errorf("emit chartroom retrieval audit: %w", err)
		}
	}

	return formatChartroomResults(resp.Results), nil
}

func (p *ChartRoomProvider) search(ctx context.Context, query string) (chartroomSearchResponse, string, error) {
	endpoint := p.baseURL + "/search"
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return chartroomSearchResponse{}, endpoint, fmt.Errorf("invalid chartroom search URL %q: %w", endpoint, err)
	}

	body := chartroomSearchRequest{
		Mode:      "hybrid",
		Query:     query,
		Limit:     p.limit,
		CorpusIDs: p.corpusIDs,
	}
	if len(p.sourceIDs) > 0 {
		body.Filters.SourceIDs = p.sourceIDs
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return chartroomSearchResponse{}, endpoint, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return chartroomSearchResponse{}, endpoint, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if p.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.bearerToken)
	}

	httpResp, err := p.client.Do(req)
	if err != nil {
		return chartroomSearchResponse{}, endpoint, fmt.Errorf("chartroom search request: %w", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return chartroomSearchResponse{}, endpoint, fmt.Errorf("chartroom search failed: status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out chartroomSearchResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
		return chartroomSearchResponse{}, endpoint, fmt.Errorf("decode chartroom search response: %w", err)
	}
	return out, endpoint, nil
}

func formatChartroomResults(results []chartroomResult) string {
	var out strings.Builder
	out.WriteString("### Chartroom Retrieved Context\n\n")
	for _, r := range results {
		snippet := strings.TrimSpace(r.Chunk.Snippet)
		if snippet == "" {
			continue
		}
		title := firstNonEmpty(r.Document.Title, r.Document.CanonicalURI, r.Chunk.CitationURI, r.Chunk.ID)
		if title != "" {
			out.WriteString(fmt.Sprintf("**%s**", title))
			if len(r.Chunk.HeadingPath) > 0 {
				out.WriteString(" — ")
				out.WriteString(strings.Join(r.Chunk.HeadingPath, " › "))
			}
			out.WriteString(fmt.Sprintf(" (relevance: %.2f)\n", r.Score))
		}
		if citation := firstNonEmpty(r.Chunk.CitationURI, r.Document.CanonicalURI); citation != "" {
			out.WriteString("Source: ")
			out.WriteString(citation)
			if r.Chunk.CitationStartLine > 0 {
				out.WriteString(fmt.Sprintf(":%d", r.Chunk.CitationStartLine))
				if r.Chunk.CitationEndLine > r.Chunk.CitationStartLine {
					out.WriteString(fmt.Sprintf("-%d", r.Chunk.CitationEndLine))
				}
			}
			out.WriteString("\n")
		}
		out.WriteString(snippet)
		out.WriteString("\n\n")
	}
	return strings.TrimSpace(out.String())
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

type chartroomSearchRequest struct {
	Mode      string                 `json:"mode"`
	Query     string                 `json:"query"`
	CorpusIDs []string               `json:"corpus_ids,omitempty"`
	Limit     int                    `json:"limit"`
	Filters   chartroomSearchFilters `json:"filters,omitempty"`
}

type chartroomSearchFilters struct {
	SourceIDs []string `json:"source_ids,omitempty"`
}

type chartroomSearchResponse struct {
	Mode      string            `json:"mode"`
	Query     string            `json:"query"`
	Results   []chartroomResult `json:"results"`
	Logged    bool              `json:"logged"`
	LatencyMS int               `json:"latency_ms"`
}

type chartroomResult struct {
	Document chartroomResultDocument `json:"document"`
	Chunk    chartroomResultChunk    `json:"chunk"`
	Source   chartroomResultSource   `json:"source"`
	Score    float64                 `json:"score"`
}

type chartroomResultDocument struct {
	ID           string `json:"id"`
	CanonicalURI string `json:"canonical_uri"`
	Title        string `json:"title"`
}

type chartroomResultChunk struct {
	ID                string   `json:"id"`
	HeadingPath       []string `json:"heading_path"`
	Snippet           string   `json:"snippet"`
	CitationURI       string   `json:"citation_uri"`
	CitationStartLine int      `json:"citation_start_line"`
	CitationEndLine   int      `json:"citation_end_line"`
}

type chartroomResultSource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URI  string `json:"uri"`
}
