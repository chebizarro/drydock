// Package embedding provides a client for OpenAI-compatible /embeddings endpoints.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"drydock/internal/circuitbreaker"
)

// Client calls an OpenAI-compatible embedding endpoint.
type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
	breaker    *circuitbreaker.Breaker
}

// NewClient creates an embedding client.
// baseURL should include the path prefix (e.g. "http://localhost:11434/v1").
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
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

// Embed returns the embedding vector for the given text.
// Uses a circuit breaker to fail fast when the embedding service is unavailable.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	if text == "" {
		return nil, fmt.Errorf("embed: empty input text")
	}

	// Check circuit breaker
	if !c.breaker.Allow() {
		return nil, fmt.Errorf("embed: %w", circuitbreaker.ErrCircuitOpen)
	}

	vec, err := c.doEmbed(ctx, text)
	if err != nil {
		c.breaker.RecordFailure()
		return nil, err
	}

	c.breaker.RecordSuccess()
	return vec, nil
}

// doEmbed performs the actual embedding HTTP call.
func (c *Client) doEmbed(ctx context.Context, text string) ([]float32, error) {
	reqBody := embeddingRequest{
		Model: c.model,
		Input: text,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("embed: marshal request: %w", err)
	}

	url := c.baseURL + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: http call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("embed: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var result embeddingResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("embed: parse response: %w", err)
	}
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("embed: response contains no embedding data")
	}
	return result.Data[0].Embedding, nil
}

// CircuitState returns the current circuit breaker state.
func (c *Client) CircuitState() circuitbreaker.State {
	return c.breaker.State()
}

// Model returns the configured model name.
func (c *Client) Model() string { return c.model }

type embeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embeddingResponse struct {
	Data []embeddingData `json:"data"`
}

type embeddingData struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
