package reviewengine

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeLLM struct {
	responses []string
	requests  []ChatRequest
}

func (f *fakeLLM) ChatCompletion(_ context.Context, req ChatRequest) (string, error) {
	f.requests = append(f.requests, req)
	if len(f.responses) == 0 {
		return "{}", nil
	}
	r := f.responses[0]
	f.responses = f.responses[1:]
	return r, nil
}

func TestEngineRoutesPlannerToLLM70B(t *testing.T) {
	fake := &fakeLLM{
		responses: []string{
			`{"change_type":"feature","risk_areas":["architecture"],"needed_context":[],"review_focus":"design","model_route":"llm70b"}`,
			`{"summary":"ok","findings":[{"severity":"high","category":"architecture","file":"a.go","line":10,"evidence":"x","explanation":"y","suggestion":"z","confidence":0.9}],"needs_more_context":[]}`,
		},
	}
	engine := New(Config{
		Planner:  ModelEndpoint{BaseURL: "http://planner", Model: "planner-model"},
		Coder32B: ModelEndpoint{BaseURL: "http://32b", Model: "32b-model"},
		LLM70B:   ModelEndpoint{BaseURL: "http://70b", Model: "70b-model"},
		Coder14B: ModelEndpoint{BaseURL: "http://14b", Model: "14b-model"},
	}, fake, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	out, err := engine.Run(context.Background(), RunInput{
		ContextBundle: "ctx",
		ChangedFiles:  []string{"internal/auth/service.go"},
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if out.Route != RouteLLM70B {
		t.Fatalf("expected route llm70b, got %s", out.Route)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(fake.requests))
	}
	if fake.requests[1].BaseURL != "http://70b" {
		t.Fatalf("expected reviewer call to llm70b endpoint, got %s", fake.requests[1].BaseURL)
	}
	if len(out.Checklist) == 0 {
		t.Fatalf("expected non-empty checklist for auth file")
	}
}

func TestReviewerSchemaRejectsLowConfidenceWithoutNeedsMoreContext(t *testing.T) {
	_, err := ParseReviewerOutput(`{
		"summary":"s",
		"findings":[{"severity":"medium","category":"correctness","file":"main.go","line":1,"evidence":"e","explanation":"x","suggestion":"y","confidence":0.4}],
		"needs_more_context":[]
	}`)
	if err == nil {
		t.Fatalf("expected validation error for low-confidence finding without needs_more_context")
	}
}

func TestIsTransient(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		expect bool
	}{
		{"nil", nil, false},
		{"http 429", &LLMHTTPError{StatusCode: 429}, true},
		{"http 500", &LLMHTTPError{StatusCode: 500}, true},
		{"http 503", &LLMHTTPError{StatusCode: 503}, true},
		{"http 400", &LLMHTTPError{StatusCode: 400}, false},
		{"http 401", &LLMHTTPError{StatusCode: 401}, false},
		{"http 404", &LLMHTTPError{StatusCode: 404}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransient(tc.err); got != tc.expect {
				t.Fatalf("IsTransient(%v) = %v, want %v", tc.err, got, tc.expect)
			}
		})
	}
}

type failingLLM struct {
	errors []error
	calls  int
}

func (f *failingLLM) ChatCompletion(_ context.Context, _ ChatRequest) (string, error) {
	f.calls++
	if len(f.errors) > 0 {
		err := f.errors[0]
		f.errors = f.errors[1:]
		return "", err
	}
	return `{"ok":true}`, nil
}

func TestRetryingClientRetriesTransientErrors(t *testing.T) {
	inner := &failingLLM{
		errors: []error{
			&LLMHTTPError{StatusCode: 503, Status: "503 Service Unavailable", Body: "overloaded"},
			&LLMHTTPError{StatusCode: 429, Status: "429 Too Many Requests", Body: "rate limited"},
		},
	}
	rc := NewRetryingClient(inner, RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Millisecond, // fast for tests
		MaxDelay:    10 * time.Millisecond,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	result, err := rc.ChatCompletion(context.Background(), ChatRequest{Model: "test"})
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("unexpected result: %s", result)
	}
	if inner.calls != 3 {
		t.Fatalf("expected 3 attempts (2 transient + 1 success), got %d", inner.calls)
	}
}

func TestRetryingClientFailsImmediatelyOnNonTransient(t *testing.T) {
	inner := &failingLLM{
		errors: []error{
			&LLMHTTPError{StatusCode: 400, Status: "400 Bad Request", Body: "invalid"},
		},
	}
	rc := NewRetryingClient(inner, RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Millisecond,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	_, err := rc.ChatCompletion(context.Background(), ChatRequest{Model: "test"})
	if err == nil {
		t.Fatal("expected error for non-transient failure")
	}
	if inner.calls != 1 {
		t.Fatalf("expected 1 attempt (immediate fail), got %d", inner.calls)
	}
}

func TestRetryingClientExhaustsAttempts(t *testing.T) {
	inner := &failingLLM{
		errors: []error{
			&LLMHTTPError{StatusCode: 500, Status: "500", Body: "err1"},
			&LLMHTTPError{StatusCode: 500, Status: "500", Body: "err2"},
			&LLMHTTPError{StatusCode: 500, Status: "500", Body: "err3"},
		},
	}
	rc := NewRetryingClient(inner, RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Millisecond,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	_, err := rc.ChatCompletion(context.Background(), ChatRequest{Model: "test"})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if inner.calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", inner.calls)
	}
}

