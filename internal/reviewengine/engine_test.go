package reviewengine

import (
	"context"
	"io"
	"log/slog"
	"testing"
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

