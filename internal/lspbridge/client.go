package lspbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client communicates with the LSP bridge service over HTTP.
type Client struct {
	baseURL    string
	apiToken   string
	httpClient *http.Client
}

// NewClient creates an LSP bridge client.
// baseURL should be the bridge root (e.g. "http://localhost:8082").
func NewClient(baseURL string) *Client {
	return NewClientWithToken(baseURL, "")
}

// NewClientWithToken creates an LSP bridge client that authenticates requests.
func NewClientWithToken(baseURL, apiToken string) *Client {
	return &Client{
		baseURL:  baseURL,
		apiToken: apiToken,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// Analyze sends changed files and symbols to the LSP bridge for analysis.
func (c *Client) Analyze(ctx context.Context, req AnalyzeRequest) (*AnalyzeResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("lspbridge: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/analyze", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("lspbridge: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.authorize(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("lspbridge: http call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("lspbridge: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lspbridge: HTTP %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))
	}

	var result AnalyzeResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("lspbridge: parse response: %w", err)
	}
	return &result, nil
}

// Ping checks if the LSP bridge is healthy.
func (c *Client) Ping(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("lspbridge: create ping: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("lspbridge: ping: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("lspbridge: ping returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) authorize(req *http.Request) {
	if c.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
