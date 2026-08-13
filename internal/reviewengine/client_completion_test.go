package reviewengine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"drydock/internal/circuitbreaker"
)

var (
	_ ConversationalLLMClient = (*OpenAICompatClient)(nil)
	_ ConversationalLLMClient = (*RetryingClient)(nil)
	_ ConversationalLLMClient = (*CircuitBreakingClient)(nil)
)

func TestOpenAICompatCompleteEncodesConversationAndDecodesToolCall(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"served-model",
			"choices":[{
				"finish_reason":"tool_calls",
				"message":{"role":"assistant","tool_calls":[{
					"id":"call_stable_123","type":"function",
					"function":{"name":"code.search","arguments":"{\"query\":\"Widget\"}"}
				}]}
			}],
			"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}
		}`))
	}))
	defer srv.Close()

	client := NewOpenAICompatClient()
	result, err := client.Complete(context.Background(), CompletionRequest{
		BaseURL: srv.URL, APIKey: "secret", Model: "configured-model", Temperature: 0.2,
		Messages: []CompletionMessage{
			{Role: MessageRoleSystem, Content: "review carefully"},
			{Role: MessageRoleUser, Content: "inspect Widget"},
			{Role: MessageRoleAssistant, ToolCalls: []ToolCall{{ID: "prior_call", Type: "function", Function: ToolCallFunction{Name: "code.read", Arguments: `{"path":"a.go"}`}}}},
			{Role: MessageRoleTool, ToolCallID: "prior_call", Content: "package a"},
		},
		Tools: []ToolSchema{{Name: "code.search", Description: "search code", Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) != 4 {
		t.Fatalf("messages = %#v", payload["messages"])
	}
	wantRoles := []string{"system", "user", "assistant", "tool"}
	for i, want := range wantRoles {
		message := messages[i].(map[string]any)
		if message["role"] != want {
			t.Fatalf("message[%d] role = %v, want %s", i, message["role"], want)
		}
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" {
		t.Fatalf("tools = %#v", payload["tools"])
	}
	if result.FinishReason != "tool_calls" || result.Model != "served-model" {
		t.Fatalf("result metadata = %+v", result)
	}
	if len(result.Message.ToolCalls) != 1 || result.Message.ToolCalls[0].ID != "call_stable_123" {
		t.Fatalf("tool calls = %+v", result.Message.ToolCalls)
	}
	if result.Usage.PromptTokens != 11 || result.Usage.CompletionTokens != 7 || result.Usage.TotalTokens != 18 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

type completionSequenceClient struct {
	errs  []error
	calls int
}

func (f *completionSequenceClient) ChatCompletion(context.Context, ChatRequest) (ChatResult, error) {
	return ChatResult{Content: "legacy"}, nil
}

func (f *completionSequenceClient) Complete(context.Context, CompletionRequest) (CompletionResult, error) {
	f.calls++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return CompletionResult{}, err
	}
	return CompletionResult{Message: CompletionMessage{Role: MessageRoleAssistant, Content: "ok"}}, nil
}

func TestRetryingClientCompleteRetriesTransientErrors(t *testing.T) {
	inner := &completionSequenceClient{errs: []error{&LLMHTTPError{StatusCode: 503}}}
	client := NewRetryingClient(inner, RetryConfig{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}, slog.Default())
	result, err := client.Complete(context.Background(), CompletionRequest{Model: "m"})
	if err != nil || result.Message.Content != "ok" || inner.calls != 2 {
		t.Fatalf("result=%+v calls=%d err=%v", result, inner.calls, err)
	}
}

func TestCircuitBreakingClientCompleteUsesEndpointBreaker(t *testing.T) {
	inner := &completionSequenceClient{errs: []error{errors.New("offline")}}
	client := NewCircuitBreakingClient(inner, circuitbreaker.Config{
		FailureThreshold: 1, SuccessThreshold: 1, Timeout: time.Hour, MaxHalfOpenRequests: 1,
	}, slog.Default())
	req := CompletionRequest{BaseURL: "http://llm", Model: "m"}
	_, _ = client.Complete(context.Background(), req)
	_, err := client.Complete(context.Background(), req)
	if !errors.Is(err, circuitbreaker.ErrCircuitOpen) || inner.calls != 1 {
		t.Fatalf("calls=%d err=%v", inner.calls, err)
	}
}

func TestCompletionWrappersReportUnsupportedLegacyInner(t *testing.T) {
	inner := &failingLLM{}
	retrying := NewRetryingClient(inner, RetryConfig{}, slog.Default())
	if _, err := retrying.Complete(context.Background(), CompletionRequest{}); !errors.Is(err, ErrCompletionUnsupported) {
		t.Fatalf("retrying error = %v", err)
	}
	breaker := NewCircuitBreakingClient(inner, circuitbreaker.Config{}, slog.Default())
	if _, err := breaker.Complete(context.Background(), CompletionRequest{}); !errors.Is(err, ErrCompletionUnsupported) {
		t.Fatalf("breaker error = %v", err)
	}
}
