package reviewengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"drydock/internal/circuitbreaker"
	"drydock/internal/metrics"
)

type LLMClient interface {
	ChatCompletion(ctx context.Context, req ChatRequest) (ChatResult, error)
}

// CompletionClient is the additive conversational transport capability.
// It is separate from LLMClient so existing single-shot implementations remain
// source-compatible while agentic callers can require both interfaces.
type CompletionClient interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResult, error)
}

// ConversationalLLMClient supports both legacy single-shot and conversational
// completions.
type ConversationalLLMClient interface {
	LLMClient
	CompletionClient
}

type MessageRole string

const (
	MessageRoleSystem    MessageRole = "system"
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleTool      MessageRole = "tool"
)

// CompletionMessage is one ordered conversation message.
type CompletionMessage struct {
	Role       MessageRole `json:"role"`
	Content    string      `json:"content,omitempty"`
	Name       string      `json:"name,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
}

// ToolCallFunction contains OpenAI-compatible function-call arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall is a model-requested tool invocation. ID is provider-assigned and
// must be preserved unchanged in subsequent tool messages.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolSchema describes one callable function.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// CompletionRequest is an ordered conversational completion request.
type CompletionRequest struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	Messages    []CompletionMessage
	Tools       []ToolSchema
}

// CompletionUsage reports provider token accounting when available.
type CompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// CompletionResult is the first OpenAI-compatible choice.
type CompletionResult struct {
	Message      CompletionMessage
	FinishReason string
	Usage        CompletionUsage
	Model        string
}

var ErrCompletionUnsupported = errors.New("llm client does not support conversational completions")

// ChatResult is the outcome of a chat completion.
type ChatResult struct {
	// Content is the assistant message content.
	Content string
	// Model is the model identifier the endpoint reported serving for this
	// request. Empty when the provider omits it.
	Model string
}

type ChatRequest struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	System      string
	User        string
	// JSONMode asks OpenAI-compatible providers to constrain output to a JSON
	// object when they support response_format/json_object.
	JSONMode bool
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
	// Identity, when set, records the model identifier each endpoint reports
	// serving so published reviews can name the model that actually handled
	// the request. Safe to leave nil.
	Identity *ModelIdentity
}

func NewOpenAICompatClient() *OpenAICompatClient {
	return &OpenAICompatClient{
		HTTP: &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *OpenAICompatClient) ChatCompletion(ctx context.Context, req ChatRequest) (ChatResult, error) {
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
	if req.JSONMode {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}

	res, err := c.HTTP.Do(httpReq)
	if err != nil {
		return ChatResult{}, err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		metrics.LLMErrors.With(req.Model).Inc()
		respBody, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return ChatResult{}, &LLMHTTPError{
			StatusCode: res.StatusCode,
			Status:     res.Status,
			Body:       string(respBody),
		}
	}

	var decoded struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return ChatResult{}, fmt.Errorf("decode llm response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return ChatResult{}, fmt.Errorf("llm response has no choices (model=%s)", req.Model)
	}
	// Ground truth for the served model: the response reports what actually
	// handled the request, regardless of configured deployment names.
	if c.Identity != nil {
		c.Identity.Observe(req.BaseURL, req.APIKey, req.Model, decoded.Model)
	}
	return ChatResult{Content: decoded.Choices[0].Message.Content, Model: strings.TrimSpace(decoded.Model)}, nil
}

func (c *OpenAICompatClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
	metrics.LLMRequests.With(req.Model).Inc()
	done := metrics.TimerVec(metrics.LLMDuration, req.Model)
	defer done()

	for i, message := range req.Messages {
		switch message.Role {
		case MessageRoleSystem, MessageRoleUser, MessageRoleAssistant, MessageRoleTool:
		default:
			return CompletionResult{}, fmt.Errorf("message[%d] has invalid role %q", i, message.Role)
		}
	}
	tools := make([]map[string]any, 0, len(req.Tools))
	for i, tool := range req.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return CompletionResult{}, fmt.Errorf("tool[%d] name is required", i)
		}
		parameters := tool.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{}`)
		}
		if !json.Valid(parameters) {
			return CompletionResult{}, fmt.Errorf("tool[%d] parameters are not valid JSON", i)
		}
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": tool.Name, "description": tool.Description, "parameters": parameters,
			},
		})
	}

	payload := map[string]any{
		"model": req.Model, "messages": req.Messages, "temperature": req.Temperature,
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return CompletionResult{}, fmt.Errorf("encode completion request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return CompletionResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}

	res, err := c.HTTP.Do(httpReq)
	if err != nil {
		return CompletionResult{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		metrics.LLMErrors.With(req.Model).Inc()
		respBody, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return CompletionResult{}, &LLMHTTPError{StatusCode: res.StatusCode, Status: res.Status, Body: string(respBody)}
	}

	var decoded struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      CompletionMessage `json:"message"`
			FinishReason string            `json:"finish_reason"`
		} `json:"choices"`
		Usage CompletionUsage `json:"usage"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return CompletionResult{}, fmt.Errorf("decode llm response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return CompletionResult{}, fmt.Errorf("llm response has no choices (model=%s)", req.Model)
	}
	if c.Identity != nil {
		c.Identity.Observe(req.BaseURL, req.APIKey, req.Model, decoded.Model)
	}
	choice := decoded.Choices[0]
	return CompletionResult{
		Message: choice.Message, FinishReason: choice.FinishReason, Usage: decoded.Usage,
		Model: strings.TrimSpace(decoded.Model),
	}, nil
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

// CircuitBreakingClient wraps an LLMClient with per-endpoint/model circuit breakers.
type CircuitBreakingClient struct {
	Inner         LLMClient
	Logger        *slog.Logger
	breakerConfig circuitbreaker.Config

	mu       sync.Mutex
	breakers map[string]*circuitbreaker.Breaker
}

func NewCircuitBreakingClient(inner LLMClient, cfg circuitbreaker.Config, logger *slog.Logger) *CircuitBreakingClient {
	return &CircuitBreakingClient{
		Inner:         inner,
		Logger:        logger,
		breakerConfig: cfg,
		breakers:      make(map[string]*circuitbreaker.Breaker),
	}
}

func (c *CircuitBreakingClient) ChatCompletion(ctx context.Context, req ChatRequest) (ChatResult, error) {
	breaker := c.getBreaker(req.BaseURL, req.Model)
	var result ChatResult
	err := breaker.Execute(ctx, func(ctx context.Context) error {
		var callErr error
		result, callErr = c.Inner.ChatCompletion(ctx, req)
		return callErr
	})
	if err != nil {
		if errors.Is(err, circuitbreaker.ErrCircuitOpen) && c.Logger != nil {
			c.Logger.Warn("llm circuit breaker open",
				"base_url", req.BaseURL,
				"model", req.Model,
			)
		}
		return ChatResult{}, err
	}
	return result, nil
}

func (c *CircuitBreakingClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
	inner, ok := c.Inner.(CompletionClient)
	if !ok {
		return CompletionResult{}, ErrCompletionUnsupported
	}
	breaker := c.getBreaker(req.BaseURL, req.Model)
	var result CompletionResult
	err := breaker.Execute(ctx, func(ctx context.Context) error {
		var callErr error
		result, callErr = inner.Complete(ctx, req)
		return callErr
	})
	if err != nil {
		if errors.Is(err, circuitbreaker.ErrCircuitOpen) && c.Logger != nil {
			c.Logger.Warn("llm circuit breaker open", "base_url", req.BaseURL, "model", req.Model)
		}
		return CompletionResult{}, err
	}
	return result, nil
}

func (c *CircuitBreakingClient) getBreaker(baseURL, model string) *circuitbreaker.Breaker {
	key := baseURL + "|" + model
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.breakers[key]; ok {
		return b
	}
	b := circuitbreaker.New(c.breakerConfig)
	c.breakers[key] = b
	return b
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

func (c *RetryingClient) ChatCompletion(ctx context.Context, req ChatRequest) (ChatResult, error) {
	var lastErr error
	for attempt := 0; attempt < c.Config.MaxAttempts; attempt++ {
		result, err := c.Inner.ChatCompletion(ctx, req)
		if err == nil {
			return result, nil
		}
		lastErr = err

		if !IsTransient(err) {
			return ChatResult{}, err // non-transient: fail immediately
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
			return ChatResult{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	return ChatResult{}, fmt.Errorf("llm request failed after %d attempts: %w", c.Config.MaxAttempts, lastErr)
}

func (c *RetryingClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
	inner, ok := c.Inner.(CompletionClient)
	if !ok {
		return CompletionResult{}, ErrCompletionUnsupported
	}
	var lastErr error
	for attempt := 0; attempt < c.Config.MaxAttempts; attempt++ {
		result, err := inner.Complete(ctx, req)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !IsTransient(err) {
			return CompletionResult{}, err
		}
		if attempt+1 >= c.Config.MaxAttempts {
			break
		}

		delay := time.Duration(float64(c.Config.BaseDelay) * math.Pow(2, float64(attempt)))
		if delay > c.Config.MaxDelay {
			delay = c.Config.MaxDelay
		}
		if c.Logger != nil {
			c.Logger.Warn("llm completion failed (transient), retrying",
				"attempt", attempt+1,
				"max_attempts", c.Config.MaxAttempts,
				"delay", delay.String(),
				"model", req.Model,
				"error", err,
			)
		}
		select {
		case <-ctx.Done():
			return CompletionResult{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	return CompletionResult{}, fmt.Errorf("llm completion failed after %d attempts: %w", c.Config.MaxAttempts, lastErr)
}
