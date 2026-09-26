package deprunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/hashutil"
)

// Client is the drydock-side HTTP client for the dep-runner sidecar.
type Client struct {
	baseURL    string
	apiToken   string
	httpClient *http.Client
}

// NewClient creates a dep-runner client with no authentication token. Prefer
// NewClientWithToken: the sidecar rejects unauthenticated /update calls, so a
// token-less client succeeds against /healthz and then fails every real call.
func NewClient(baseURL string) *Client {
	return NewClientWithToken(baseURL, "")
}

// NewClientWithToken creates a dep-runner client that authenticates requests
// with a bearer token. baseURL is the sidecar root (e.g. "http://dep-runner:8083").
func NewClientWithToken(baseURL, apiToken string) *Client {
	return &Client{
		baseURL:  baseURL,
		apiToken: apiToken,
		// Toolchain runs can be slow (index fetches, lockfile solves); give the
		// transport more headroom than the sidecar's own per-request timeout so a
		// server-side timeout surfaces as a structured error, not a client hangup.
		httpClient: &http.Client{Timeout: 5 * time.Minute},
	}
}

// Update requests a dependency edit. The returned response carries the changed
// manifest/lockfile contents on success.
func (c *Client) Update(ctx context.Context, req UpdateRequest) (*UpdateResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("deprunner: invalid update request: %w", err)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("deprunner: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/update", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("deprunner: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.authorize(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("deprunner: http call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("deprunner: read response: %w", err)
	}

	// The handler returns a structured UpdateResponse for both application-level
	// failures (422) and successes (200). Reserve the generic HTTP error for
	// transport/auth failures that carry no JSON body we control.
	switch resp.StatusCode {
	case http.StatusOK, http.StatusUnprocessableEntity:
		var result UpdateResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("deprunner: parse response: %w", err)
		}
		return &result, nil
	default:
		return nil, fmt.Errorf("deprunner: HTTP %d: %s", resp.StatusCode, hashutil.TruncateForLog(string(respBody), 200))
	}
}

// Ping checks that the sidecar is reachable and healthy.
func (c *Client) Ping(ctx context.Context) (*HealthResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return nil, fmt.Errorf("deprunner: create ping: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("deprunner: ping: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("deprunner: read ping response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deprunner: ping returned HTTP %d", resp.StatusCode)
	}
	var health HealthResponse
	if err := json.Unmarshal(respBody, &health); err != nil {
		return nil, fmt.Errorf("deprunner: parse ping response: %w", err)
	}
	return &health, nil
}

func (c *Client) authorize(req *http.Request) {
	if c.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
	}
}
