package reviewengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"time"

	"drydock/internal/metrics"
)

type LLMClient interface {
	ChatCompletion(ctx context.Context, req ChatRequest) (string, error)
}

type ChatRequest struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	System      string
	User        string
}

// LLMHTTPError represents an HTTP-level error from the LLM endpoint.
// Callers can inspect StatusCode to decide if the error is transient.
type LLMHTTPError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *LLMHTTPError) Error() string {
	return fmt.Sprintf("llm request failed: HTTP %d %s: %s", e.StatusCode, e.Status, e.Body)
}

// IsTransient returns true for errors that may succeed on retry (429, 5xx, network).
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if httpErr, ok := err.(*LLMHTTPError); ok {
		return httpErr.StatusCode == 429 || httpErr.StatusCode >= 500
	}
	// Context cancellation is not transient
	if err == context.Canceled || err == context.DeadlineExceeded {
		return false
	}
	// For non-HTTP errors (network timeouts, connection refused, DNS failures),
	// assume transient since these typically resolve on retry.
	return true
}

// OpenAICompatClient is a simple, non-retrying LLM client.
type OpenAICompatClient struct {
	HTTP *http.Client
}

func NewOpenAICompatClient() *OpenAICompatClient {
	return &OpenAICompatClient{
		HTTP: &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *OpenAICompatClient) ChatCompletion(ctx context.Context, req ChatRequest) (string, error) {
	metrics.LLMRequests.With(req.Model).Inc()
	done := metrics.TimerVec(metrics.LLMDuration, req.Model)
	defer done()

	payload := map[string]any{
		"model": req.Model,
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
		"temperature": req.Temperature,
	}
	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}

	res, err := c.HTTP.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		metrics.LLMErrors.With(req.Model).Inc()
		respBody, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return "", &LLMHTTPError{
			StatusCode: res.StatusCode,
			Status:     res.Status,
			Body:       string(respBody),
		}
	}

	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("decode llm response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return "", fmt.Errorf("llm response has no choices (model=%s)", req.Model)
	}
	return decoded.Choices[0].Message.Content, nil
}

// RetryConfig controls retry behavior for the RetryingClient.
type RetryConfig struct {
	MaxAttempts int           // Maximum number of attempts (including the first). Default: 3.
	BaseDelay   time.Duration // Initial backoff delay. Default: 2s.
	MaxDelay    time.Duration // Maximum backoff delay. Default: 30s.
}

// RetryingClient wraps an LLMClient and retries transient failures with exponential backoff.
type RetryingClient struct {
	Inner  LLMClient
	Config RetryConfig
	Logger *slog.Logger
}

func NewRetryingClient(inner LLMClient, cfg RetryConfig, logger *slog.Logger) *RetryingClient {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 2 * time.Second
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 30 * time.Second
	}
	return &RetryingClient{Inner: inner, Config: cfg, Logger: logger}
}

func (c *RetryingClient) ChatCompletion(ctx context.Context, req ChatRequest) (string, error) {
	var lastErr error
	for attempt := 0; attempt < c.Config.MaxAttempts; attempt++ {
		result, err := c.Inner.ChatCompletion(ctx, req)
		if err == nil {
			return result, nil
		}
		lastErr = err

		if !IsTransient(err) {
			return "", err // non-transient: fail immediately
		}

		if attempt+1 >= c.Config.MaxAttempts {
			break // no more attempts
		}

		// Exponential backoff: baseDelay * 2^attempt, capped at maxDelay
		delay := time.Duration(float64(c.Config.BaseDelay) * math.Pow(2, float64(attempt)))
		if delay > c.Config.MaxDelay {
			delay = c.Config.MaxDelay
		}

		c.Logger.Warn("llm request failed (transient), retrying",
			"attempt", attempt+1,
			"max_attempts", c.Config.MaxAttempts,
			"delay", delay.String(),
			"model", req.Model,
			"error", err,
		)

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delay):
		}
	}
	return "", fmt.Errorf("llm request failed after %d attempts: %w", c.Config.MaxAttempts, lastErr)
}
