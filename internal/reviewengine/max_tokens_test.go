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
)

// capturePayloadServer records the decoded JSON request body and replies with
// a single choice that stops with finishReason.
func capturePayloadServer(t *testing.T, finishReason string, payload *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		*payload = nil
		if err := json.Unmarshal(body, payload); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"served","choices":[{"finish_reason":"` + finishReason + `","message":{"role":"assistant","content":"{}"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestChatCompletionMaxTokensPayload(t *testing.T) {
	var payload map[string]any
	srv := capturePayloadServer(t, FinishReasonLength, &payload)
	client := NewOpenAICompatClient(time.Minute)

	res, err := client.ChatCompletion(context.Background(), ChatRequest{BaseURL: srv.URL, Model: "m", MaxTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := payload["max_tokens"].(float64); !ok || got != 4096 {
		t.Fatalf("max_tokens = %#v, want 4096", payload["max_tokens"])
	}
	if res.FinishReason != FinishReasonLength {
		t.Fatalf("FinishReason = %q, want %q", res.FinishReason, FinishReasonLength)
	}

	if _, err := client.ChatCompletion(context.Background(), ChatRequest{BaseURL: srv.URL, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, present := payload["max_tokens"]; present {
		t.Fatalf("max_tokens sent with no cap configured: %#v", payload["max_tokens"])
	}
}

func TestCompleteMaxTokensPayload(t *testing.T) {
	var payload map[string]any
	srv := capturePayloadServer(t, FinishReasonLength, &payload)
	client := NewOpenAICompatClient(time.Minute)
	messages := []CompletionMessage{{Role: MessageRoleUser, Content: "hi"}}

	res, err := client.Complete(context.Background(), CompletionRequest{BaseURL: srv.URL, Model: "m", Messages: messages, MaxTokens: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := payload["max_tokens"].(float64); !ok || got != 2048 {
		t.Fatalf("max_tokens = %#v, want 2048", payload["max_tokens"])
	}
	if res.FinishReason != FinishReasonLength {
		t.Fatalf("FinishReason = %q, want %q", res.FinishReason, FinishReasonLength)
	}

	if _, err := client.Complete(context.Background(), CompletionRequest{BaseURL: srv.URL, Model: "m", Messages: messages}); err != nil {
		t.Fatal(err)
	}
	if _, present := payload["max_tokens"]; present {
		t.Fatalf("max_tokens sent with no cap configured: %#v", payload["max_tokens"])
	}
}

type scriptedChatLLM struct {
	results  []ChatResult
	requests []ChatRequest
}

func (s *scriptedChatLLM) ChatCompletion(_ context.Context, req ChatRequest) (ChatResult, error) {
	s.requests = append(s.requests, req)
	if len(s.results) == 0 {
		return ChatResult{}, errors.New("scriptedChatLLM: no results left")
	}
	r := s.results[0]
	s.results = s.results[1:]
	return r, nil
}

func TestCompleteStructuredRepairsTruncatedOutput(t *testing.T) {
	// The truncated reply parses on its own; finish_reason=length must still
	// route it through repair rather than accept a partial review.
	llm := &scriptedChatLLM{results: []ChatResult{
		{Content: `{"summary":"partial","findings":[],"needs_more_context":[]}`, FinishReason: FinishReasonLength},
		{Content: `{"summary":"repaired","findings":[],"needs_more_context":[]}`, FinishReason: "stop", Model: "served"},
	}}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	out, model, raw, err := CompleteStructured(context.Background(), llm, logger,
		ChatRequest{Model: "m", MaxTokens: 1024}, "reviewer", ParseReviewerOutput)
	if err != nil {
		t.Fatal(err)
	}
	if out.Summary != "repaired" || model != "served" || raw == "" {
		t.Fatalf("got summary=%q model=%q raw=%q, want repaired output", out.Summary, model, raw)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("requests = %d, want original + 1 repair", len(llm.requests))
	}
	if llm.requests[1].MaxTokens != 1024 {
		t.Fatalf("repair MaxTokens = %d, want the original cap 1024", llm.requests[1].MaxTokens)
	}
}

func TestCompleteStructuredFailsWhenEveryAttemptTruncated(t *testing.T) {
	truncated := ChatResult{Content: `{"summary":"cut`, FinishReason: FinishReasonLength}
	llm := &scriptedChatLLM{results: []ChatResult{truncated, truncated, truncated}}

	_, _, _, err := CompleteStructured(context.Background(), llm, nil,
		ChatRequest{Model: "m", MaxTokens: 64}, "planner", ParsePlannerOutput)
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("err = %v, want ErrOutputTruncated", err)
	}
	if len(llm.requests) != 1+maxStructuredRepairAttempts {
		t.Fatalf("requests = %d, want %d", len(llm.requests), 1+maxStructuredRepairAttempts)
	}
}

func TestEngineSendsEndpointMaxTokens(t *testing.T) {
	fake := &fakeLLM{responses: []string{
		`{"change_type":"bugfix","risk_areas":[],"needed_context":[],"review_focus":"correctness","model_route":"coder32b"}`,
		`{"summary":"ok","findings":[],"needs_more_context":[]}`,
		`{"walkthrough":"w","file_summaries":[]}`,
	}}
	engine := New(Config{
		Planner:  ModelEndpoint{BaseURL: "http://planner", Model: "p", MaxTokens: 1000},
		Coder32B: ModelEndpoint{BaseURL: "http://32b", Model: "32b", MaxTokens: 3000},
		LLM70B:   ModelEndpoint{BaseURL: "http://70b", Model: "70b"},
		Coder14B: ModelEndpoint{BaseURL: "http://14b", Model: "14b"},
	}, fake, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	if _, err := engine.Run(context.Background(), RunInput{ContextBundle: "ctx", ChangedFiles: []string{"main.go"}}); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 3 {
		t.Fatalf("requests = %d, want planner + reviewer + walkthrough", len(fake.requests))
	}
	for i, want := range []int{1000, 3000, 1000} {
		if got := fake.requests[i].MaxTokens; got != want {
			t.Fatalf("request[%d] MaxTokens = %d, want %d", i, got, want)
		}
	}
}
