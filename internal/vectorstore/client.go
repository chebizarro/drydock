// Package vectorstore provides a client for the Qdrant vector database REST API.
//
// Drydock uses three Qdrant collections for retrieval-augmented review:
//   - nip_specs: NIP protocol specifications chunked by section
//   - project_docs: per-repo CONTRIBUTING guides, style guides, schemas
//   - few_shot_reviews: patch-type-keyed positive/negative review examples
package vectorstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"drydock/internal/circuitbreaker"
	"drydock/internal/metrics"
)

// Client interacts with a Qdrant instance via its REST API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	breaker    *circuitbreaker.Breaker
}

// NewClient creates a Qdrant REST client.
// baseURL should be the Qdrant server root (e.g. "http://localhost:6333").
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		breaker: circuitbreaker.New(circuitbreaker.Config{
			FailureThreshold: 5,
			SuccessThreshold: 2,
			Timeout:          30 * time.Second,
		}),
	}
}

// Drydock collection names.
const (
	CollectionNIPSpecs    = "nip_specs"
	CollectionProjectDocs = "project_docs"
	CollectionFewShot     = "few_shot_reviews"
	CollectionCodeChunks  = "code_chunks"

	collectionDistanceCosine = "Cosine"
)

// Point represents a vector point in Qdrant.
type Point struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload,omitempty"`
}

// SearchResult represents a single search hit.
type SearchResult struct {
	ID      string         `json:"id"`
	Score   float32        `json:"score"`
	Payload map[string]any `json:"payload,omitempty"`
}

// CollectionInfo contains basic metadata about a collection.
type CollectionInfo struct {
	Status      string `json:"status"`
	PointsCount int64  `json:"points_count"`
	VectorSize  int    `json:"vector_size"`
	Distance    string `json:"distance"`
}

// EnsureCollection creates a collection if it does not already exist.
// vectorSize is the dimensionality of the embedding vectors (e.g. 768 for nomic-embed).
func (c *Client) EnsureCollection(ctx context.Context, name string, vectorSize int) error {
	// Check existence first.
	info, err := c.GetCollection(ctx, name)
	if err == nil {
		if err := validateCollectionConfig(name, info, vectorSize, collectionDistanceCosine); err != nil {
			return err
		}
		return nil // already exists and matches configured embedding dimensions
	}

	body := map[string]any{
		"vectors": map[string]any{
			"size":     vectorSize,
			"distance": collectionDistanceCosine,
		},
	}
	_, err = c.do(ctx, http.MethodPut, "/collections/"+name, body)
	if err != nil {
		return fmt.Errorf("create collection %s: %w", name, err)
	}
	return nil
}

// GetCollection returns metadata for a named collection.
func (c *Client) GetCollection(ctx context.Context, name string) (*CollectionInfo, error) {
	respBody, err := c.do(ctx, http.MethodGet, "/collections/"+name, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Result struct {
			Status      string `json:"status"`
			PointsCount int64  `json:"points_count"`
			Config      struct {
				Params struct {
					Vectors struct {
						Size     int    `json:"size"`
						Distance string `json:"distance"`
					} `json:"vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("parse collection info: %w", err)
	}

	return &CollectionInfo{
		Status:      resp.Result.Status,
		PointsCount: resp.Result.PointsCount,
		VectorSize:  resp.Result.Config.Params.Vectors.Size,
		Distance:    resp.Result.Config.Params.Vectors.Distance,
	}, nil
}

// DeleteCollection removes a collection and all its points.
func (c *Client) DeleteCollection(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodDelete, "/collections/"+name, nil)
	return err
}

// Upsert inserts or updates points in a collection.
// Write is synchronous (wait=true).
func (c *Client) Upsert(ctx context.Context, collection string, points []Point) error {
	if len(points) == 0 {
		return nil
	}

	body := map[string]any{
		"points": points,
	}
	_, err := c.do(ctx, http.MethodPut, "/collections/"+collection+"/points?wait=true", body)
	if err != nil {
		return fmt.Errorf("upsert points in %s: %w", collection, err)
	}
	return nil
}

// Search performs a nearest-neighbor vector search and returns up to limit results.
// An optional filter map is passed directly to Qdrant's filter field.
func (c *Client) Search(ctx context.Context, collection string, vector []float32, limit int, filter map[string]any) ([]SearchResult, error) {
	body := map[string]any{
		"vector":       vector,
		"limit":        limit,
		"with_payload": true,
	}
	if filter != nil {
		body["filter"] = filter
	}

	respBody, err := c.do(ctx, http.MethodPost, "/collections/"+collection+"/points/search", body)
	if err != nil {
		return nil, fmt.Errorf("search %s: %w", collection, err)
	}

	var resp struct {
		Result []SearchResult `json:"result"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("parse search results: %w", err)
	}
	return resp.Result, nil
}

// Delete removes points by ID from a collection.
func (c *Client) Delete(ctx context.Context, collection string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	body := map[string]any{
		"points": ids,
	}
	_, err := c.do(ctx, http.MethodPost, "/collections/"+collection+"/points/delete?wait=true", body)
	if err != nil {
		return fmt.Errorf("delete points from %s: %w", collection, err)
	}
	return nil
}

// Count returns the number of points in a collection, optionally filtered.
func (c *Client) Count(ctx context.Context, collection string, filter map[string]any) (int64, error) {
	body := map[string]any{
		"exact": true,
	}
	if filter != nil {
		body["filter"] = filter
	}

	respBody, err := c.do(ctx, http.MethodPost, "/collections/"+collection+"/points/count", body)
	if err != nil {
		return 0, fmt.Errorf("count points in %s: %w", collection, err)
	}

	var resp struct {
		Result struct {
			Count int64 `json:"count"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return 0, fmt.Errorf("parse count response: %w", err)
	}
	return resp.Result.Count, nil
}

// Scroll retrieves points with optional filtering and cursor-based pagination.
// Returns points (without vectors), and a nextOffset cursor (nil when exhausted).
func (c *Client) Scroll(ctx context.Context, collection string, limit int, offset *string, filter map[string]any) ([]Point, *string, error) {
	body := map[string]any{
		"limit":        limit,
		"with_payload": true,
		"with_vector":  false,
	}
	if offset != nil {
		body["offset"] = *offset
	}
	if filter != nil {
		body["filter"] = filter
	}

	respBody, err := c.do(ctx, http.MethodPost, "/collections/"+collection+"/points/scroll", body)
	if err != nil {
		return nil, nil, fmt.Errorf("scroll %s: %w", collection, err)
	}

	var resp struct {
		Result struct {
			Points []struct {
				ID      string         `json:"id"`
				Payload map[string]any `json:"payload"`
			} `json:"points"`
			NextPageOffset *string `json:"next_page_offset"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, nil, fmt.Errorf("parse scroll results: %w", err)
	}

	points := make([]Point, len(resp.Result.Points))
	for i, p := range resp.Result.Points {
		points[i] = Point{
			ID:      p.ID,
			Payload: p.Payload,
		}
	}
	return points, resp.Result.NextPageOffset, nil
}

// do executes an HTTP request against the Qdrant REST API.
// Uses a circuit breaker to fail fast when the service is unavailable.
func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	// Check circuit breaker
	if !c.breaker.Allow() {
		metrics.CircuitBreakerRejected.With("vectorstore").Inc()
		return nil, fmt.Errorf("vectorstore: %w", circuitbreaker.ErrCircuitOpen)
	}

	respBody, err := c.doHTTP(ctx, method, path, body)
	if err != nil {
		c.recordFailure()
		return nil, err
	}

	c.recordSuccess()
	return respBody, nil
}

// doHTTP performs the actual HTTP request.
func (c *Client) doHTTP(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("api-key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	return respBody, nil
}

// Ping verifies the Qdrant service can answer a lightweight collections request.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/collections", nil)
	return err
}

// CircuitState returns the current circuit breaker state.
func (c *Client) CircuitState() circuitbreaker.State {
	return c.breaker.State()
}

func (c *Client) recordFailure() {
	before := c.breaker.State()
	c.breaker.RecordFailure()
	if before != circuitbreaker.StateOpen && c.breaker.State() == circuitbreaker.StateOpen {
		metrics.CircuitBreakerOpened.With("vectorstore").Inc()
	}
}

func (c *Client) recordSuccess() {
	before := c.breaker.State()
	c.breaker.RecordSuccess()
	if before != circuitbreaker.StateClosed && c.breaker.State() == circuitbreaker.StateClosed {
		metrics.CircuitBreakerClosed.With("vectorstore").Inc()
	}
}

func validateCollectionConfig(name string, info *CollectionInfo, wantSize int, wantDistance string) error {
	if info == nil {
		return fmt.Errorf("collection %s exists but returned no configuration", name)
	}
	if info.VectorSize != wantSize {
		return fmt.Errorf("collection %s vector size mismatch: existing size %d, configured size %d; migrate or recreate the collection before indexing", name, info.VectorSize, wantSize)
	}
	if info.Distance != "" && !strings.EqualFold(info.Distance, wantDistance) {
		return fmt.Errorf("collection %s distance mismatch: existing distance %q, configured distance %q; migrate or recreate the collection before indexing", name, info.Distance, wantDistance)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
