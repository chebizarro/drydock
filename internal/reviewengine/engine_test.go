package reviewengine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"drydock/internal/circuitbreaker"
)

type fakeLLM struct {
	responses   []string
	requests    []ChatRequest
	servedModel string
}

func (f *fakeLLM) ChatCompletion(_ context.Context, req ChatRequest) (ChatResult, error) {
	f.requests = append(f.requests, req)
	if len(f.responses) == 0 {
		return ChatResult{Content: "{}"}, nil
	}
	r := f.responses[0]
	f.responses = f.responses[1:]
	return ChatResult{Content: r, Model: f.servedModel}, nil
}

func TestEngineRoutesPlannerToLLM70B(t *testing.T) {
	fake := &fakeLLM{
		responses: []string{
			`{"change_type":"feature","risk_areas":["architecture"],"needed_context":[],"review_focus":"design","model_route":"llm70b"}`,
			`{"summary":"ok","findings":[{"severity":"high","category":"architecture","file":"a.go","line":10,"evidence":"x","explanation":"y","suggestion":"z","confidence":0.9}],"needs_more_context":[]}`,
			`{"walkthrough":"This adds a feature.","file_summaries":[{"file":"a.go","summary":"Added feature"}]}`,
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
	if len(fake.requests) != 3 {
		t.Fatalf("expected 3 requests (planner + reviewer + walkthrough), got %d", len(fake.requests))
	}
	if fake.requests[1].BaseURL != "http://70b" {
		t.Fatalf("expected reviewer call to llm70b endpoint, got %s", fake.requests[1].BaseURL)
	}
	if len(out.Checklist) == 0 {
		t.Fatalf("expected non-empty checklist for auth file")
	}
}

func TestEngineRunPropagatesServedModel(t *testing.T) {
	fake := &fakeLLM{
		servedModel: "gemma-4-26b",
		responses: []string{
			`{"change_type":"feature","risk_areas":[],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`,
			`{"summary":"ok","findings":[],"needs_more_context":[]}`,
		},
	}
	engine := New(Config{
		Planner:  ModelEndpoint{BaseURL: "http://planner", Model: "planner-model"},
		Coder32B: ModelEndpoint{BaseURL: "http://32b", Model: "32b-model"},
		LLM70B:   ModelEndpoint{BaseURL: "http://70b", Model: "70b-model"},
		Coder14B: ModelEndpoint{BaseURL: "http://14b", Model: "14b-model"},
	}, fake, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	out, err := engine.Run(context.Background(), RunInput{
		ContextBundle:   "ctx",
		ChangedFiles:    []string{"a.go"},
		SkipWalkthrough: true,
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if out.ServedModel != "gemma-4-26b" {
		t.Fatalf("expected served model from reviewer response, got %q", out.ServedModel)
	}
}

func TestEngineTestCoverageGapsAddedToChecklist(t *testing.T) {
	fake := &fakeLLM{
		responses: []string{
			`{"change_type":"feature","risk_areas":[],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`,
			`{"summary":"missing tests","findings":[],"needs_more_context":[]}`,
			`{"walkthrough":"Test changes.","file_summaries":[]}`,
		},
	}
	engine := New(Config{
		Planner:  ModelEndpoint{BaseURL: "http://planner", Model: "planner-model"},
		Coder32B: ModelEndpoint{BaseURL: "http://32b", Model: "32b-model"},
		LLM70B:   ModelEndpoint{BaseURL: "http://70b", Model: "70b-model"},
		Coder14B: ModelEndpoint{BaseURL: "http://14b", Model: "14b-model"},
	}, fake, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	out, err := engine.Run(context.Background(), RunInput{
		ContextBundle:    "ctx",
		ChangedFiles:     []string{"main.go"},
		TestCoverageGaps: []string{"Foo", "Bar"},
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// Should have at least one checklist item about missing test coverage
	found := false
	for _, item := range out.Checklist {
		if strings.Contains(item, "Foo") && strings.Contains(item, "Bar") && strings.Contains(item, "test coverage") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected checklist item about test coverage gaps, got: %v", out.Checklist)
	}

	// The system prompt should contain the coverage gap checklist
	reviewerReq := fake.requests[1]
	if !strings.Contains(reviewerReq.System, "Foo") || !strings.Contains(reviewerReq.System, "Bar") {
		t.Fatalf("expected coverage gap symbols in system prompt, got: %s", reviewerReq.System)
	}
}

func TestEngineRepairsMalformedFencedReviewerJSON(t *testing.T) {
	fake := &FakeLLMForTest{
		Responses: []string{
			`{"change_type":"bugfix","risk_areas":[],"needed_context":[],"review_focus":"correctness","model_route":"coder32b"}`,
			"```json\n{\"summary\":\"ok\" \"findings\":[],\"needs_more_context\":[]}\n```",
			`{"summary":"ok after repair","findings":[],"needs_more_context":[]}`,
			`{"walkthrough":"Repair path works.","file_summaries":[]}`,
		},
	}
	engine := New(Config{
		Planner:  ModelEndpoint{BaseURL: "http://planner", Model: "p"},
		Coder32B: ModelEndpoint{BaseURL: "http://32b", Model: "32b"},
		LLM70B:   ModelEndpoint{BaseURL: "http://70b", Model: "70b"},
		Coder14B: ModelEndpoint{BaseURL: "http://14b", Model: "14b"},
	}, fake, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	out, err := engine.Run(context.Background(), RunInput{
		ContextBundle: "ctx",
		ChangedFiles:  []string{"main.go"},
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if out.Review.Summary != "ok after repair" {
		t.Fatalf("expected repaired review summary, got %q", out.Review.Summary)
	}
	if len(fake.Requests) != 4 {
		t.Fatalf("expected planner + reviewer + repair + walkthrough requests, got %d", len(fake.Requests))
	}
	if !fake.Requests[1].JSONMode || !fake.Requests[2].JSONMode {
		t.Fatal("expected reviewer and repair requests to use JSON mode")
	}
	if !strings.Contains(fake.Requests[2].System, "repair") {
		t.Fatalf("expected repair prompt, got system prompt %q", fake.Requests[2].System)
	}
}

func TestEngineWalkthroughFailureReflectedInStatus(t *testing.T) {
	fake := &FakeLLMForTest{
		Responses: []string{
			`{"change_type":"feature","risk_areas":[],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`,
			`{"summary":"ok","findings":[],"needs_more_context":[]}`,
			`{"walkthrough":`,
			`still not json`,
			`{"file_summaries": "wrong type"}`,
		},
	}
	engine := New(Config{
		Planner:  ModelEndpoint{BaseURL: "http://planner", Model: "p"},
		Coder32B: ModelEndpoint{BaseURL: "http://32b", Model: "32b"},
		LLM70B:   ModelEndpoint{BaseURL: "http://70b", Model: "70b"},
		Coder14B: ModelEndpoint{BaseURL: "http://14b", Model: "14b"},
	}, fake, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	out, err := engine.Run(context.Background(), RunInput{
		ContextBundle: "ctx",
		ChangedFiles:  []string{"main.go"},
	})
	if err != nil {
		t.Fatalf("run should keep walkthrough non-fatal: %v", err)
	}
	if out.WalkthroughStatus.State != StepStateFailed {
		t.Fatalf("expected failed walkthrough status, got %#v", out.WalkthroughStatus)
	}
	if out.WalkthroughStatus.Error == "" {
		t.Fatal("expected walkthrough failure error to be recorded")
	}
	if out.Walkthrough.Walkthrough != "" {
		t.Fatalf("expected empty walkthrough body on failure, got %q", out.Walkthrough.Walkthrough)
	}
}

func TestEngineStructuredSuggestionsPreserved(t *testing.T) {
	fake := &fakeLLM{
		responses: []string{
			`{"change_type":"bugfix","risk_areas":[],"needed_context":[],"review_focus":"correctness","model_route":"coder32b"}`,
			`{"summary":"found a bug","findings":[{
				"severity":"high","category":"correctness","file":"main.go","line":42,
				"evidence":"err ignored","explanation":"error not checked",
				"suggestion":"check error","confidence":0.95,
				"suggested_diff":"@@ -42,1 +42,3 @@\n-\tValidate(token)\n+\tif err := Validate(token); err != nil {\n+\t\treturn err\n+\t}",
				"suggested_code":"if err := Validate(token); err != nil {\n\treturn err\n}"
			}],"needs_more_context":[]}`,
			`{"walkthrough":"Fixes a bug.","file_summaries":[{"file":"main.go","summary":"Fixed error handling"}]}`,
		},
	}
	engine := New(Config{
		Planner:  ModelEndpoint{BaseURL: "http://planner", Model: "p"},
		Coder32B: ModelEndpoint{BaseURL: "http://32b", Model: "32b"},
		LLM70B:   ModelEndpoint{BaseURL: "http://70b", Model: "70b"},
		Coder14B: ModelEndpoint{BaseURL: "http://14b", Model: "14b"},
	}, fake, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	out, err := engine.Run(context.Background(), RunInput{
		ContextBundle: "ctx",
		ChangedFiles:  []string{"main.go"},
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(out.Review.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(out.Review.Findings))
	}
	f := out.Review.Findings[0]
	if f.SuggestedDiff == "" {
		t.Fatal("expected suggested_diff to be preserved")
	}
	if !strings.Contains(f.SuggestedDiff, "@@ -42,1 +42,3 @@") {
		t.Fatalf("unexpected suggested_diff: %q", f.SuggestedDiff)
	}
	if f.SuggestedCode == "" {
		t.Fatal("expected suggested_code to be preserved")
	}
}

func TestMalformedSuggestedDiffIsSanitized(t *testing.T) {
	// A suggested_diff that doesn't look like a diff should be cleared.
	raw := `{"summary":"ok","findings":[{
		"severity":"low","category":"style","file":"x.go","line":1,
		"evidence":"e","explanation":"e","suggestion":"s","confidence":0.9,
		"suggested_diff":"this is not a diff at all"
	}],"needs_more_context":[]}`
	out, err := ParseReviewerOutput(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if out.Findings[0].SuggestedDiff != "" {
		t.Fatalf("expected malformed suggested_diff to be cleared, got %q", out.Findings[0].SuggestedDiff)
	}
}

func TestAdditionalInstructionsAppendedWithoutReplacingBase(t *testing.T) {
	fake := &fakeLLM{
		responses: []string{
			`{"change_type":"feature","risk_areas":[],"needed_context":[],"review_focus":"design","model_route":"coder32b"}`,
			`{"summary":"ok","findings":[],"needs_more_context":[]}`,
			`{"walkthrough":"Feature work.","file_summaries":[]}`,
		},
	}
	engine := New(Config{
		Planner:  ModelEndpoint{BaseURL: "http://planner", Model: "p"},
		Coder32B: ModelEndpoint{BaseURL: "http://32b", Model: "32b"},
		LLM70B:   ModelEndpoint{BaseURL: "http://70b", Model: "70b"},
		Coder14B: ModelEndpoint{BaseURL: "http://14b", Model: "14b"},
	}, fake, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	_, err := engine.Run(context.Background(), RunInput{
		ContextBundle:          "ctx",
		ChangedFiles:           []string{"main.go"},
		AdditionalInstructions: "Focus on API compatibility.",
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(fake.requests) != 3 {
		t.Fatalf("expected 3 requests (planner + reviewer + walkthrough), got %d", len(fake.requests))
	}
	system := fake.requests[1].System
	// Must contain the default prompt AND the additional instructions.
	if !strings.Contains(system, "code review agent") {
		t.Fatal("expected default prompt base in system prompt")
	}
	if !strings.Contains(system, "API compatibility") {
		t.Fatal("expected additional instructions in system prompt")
	}
	if !strings.Contains(system, "Repository-specific instructions:") {
		t.Fatal("expected instructions section header in system prompt")
	}
}

func TestEngineWalkthroughGenerated(t *testing.T) {
	fake := &fakeLLM{
		responses: []string{
			`{"change_type":"feature","risk_areas":[],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`,
			`{"summary":"ok","findings":[],"needs_more_context":[]}`,
			`{"walkthrough":"This PR adds a caching layer to the HTTP client.","file_summaries":[{"file":"cache.go","summary":"New LRU cache implementation"},{"file":"client.go","summary":"Integrated cache into request pipeline"}]}`,
		},
	}
	engine := New(Config{
		Planner:  ModelEndpoint{BaseURL: "http://planner", Model: "p"},
		Coder32B: ModelEndpoint{BaseURL: "http://32b", Model: "32b"},
		LLM70B:   ModelEndpoint{BaseURL: "http://70b", Model: "70b"},
		Coder14B: ModelEndpoint{BaseURL: "http://14b", Model: "14b"},
	}, fake, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	out, err := engine.Run(context.Background(), RunInput{
		ContextBundle: "ctx",
		ChangedFiles:  []string{"cache.go", "client.go"},
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if out.Walkthrough.Walkthrough == "" {
		t.Fatal("expected walkthrough text")
	}
	if !strings.Contains(out.Walkthrough.Walkthrough, "caching layer") {
		t.Fatalf("unexpected walkthrough: %q", out.Walkthrough.Walkthrough)
	}
	if len(out.Walkthrough.FileSummaries) != 2 {
		t.Fatalf("expected 2 file summaries, got %d", len(out.Walkthrough.FileSummaries))
	}
	// Walkthrough should use the planner endpoint (call index 2).
	if fake.requests[2].BaseURL != "http://planner" {
		t.Fatalf("expected walkthrough to use planner endpoint, got %s", fake.requests[2].BaseURL)
	}
}

func TestEngineSkipWalkthrough(t *testing.T) {
	fake := &fakeLLM{
		responses: []string{
			`{"change_type":"feature","risk_areas":[],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`,
			`{"summary":"ok","findings":[],"needs_more_context":[]}`,
		},
	}
	engine := New(Config{
		Planner:  ModelEndpoint{BaseURL: "http://planner", Model: "p"},
		Coder32B: ModelEndpoint{BaseURL: "http://32b", Model: "32b"},
		LLM70B:   ModelEndpoint{BaseURL: "http://70b", Model: "70b"},
		Coder14B: ModelEndpoint{BaseURL: "http://14b", Model: "14b"},
	}, fake, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	out, err := engine.Run(context.Background(), RunInput{
		ContextBundle:   "ctx",
		ChangedFiles:    []string{"main.go"},
		SkipWalkthrough: true,
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if out.Walkthrough.Walkthrough != "" {
		t.Fatal("expected empty walkthrough when skipped")
	}
	// Only 2 LLM calls (planner + reviewer), no walkthrough.
	if len(fake.requests) != 2 {
		t.Fatalf("expected 2 requests (no walkthrough), got %d", len(fake.requests))
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

func (f *failingLLM) ChatCompletion(_ context.Context, _ ChatRequest) (ChatResult, error) {
	f.calls++
	if len(f.errors) > 0 {
		err := f.errors[0]
		f.errors = f.errors[1:]
		return ChatResult{}, err
	}
	return ChatResult{Content: `{"ok":true}`}, nil
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
	if result.Content != `{"ok":true}` {
		t.Fatalf("unexpected result: %s", result.Content)
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

func TestCircuitBreakingClientOpensAndFailsFastPerEndpoint(t *testing.T) {
	inner := &failingLLM{
		errors: []error{
			&LLMHTTPError{StatusCode: 500, Status: "500", Body: "err1"},
			&LLMHTTPError{StatusCode: 500, Status: "500", Body: "err2"},
		},
	}
	cb := NewCircuitBreakingClient(inner, circuitbreaker.Config{
		FailureThreshold:    2,
		SuccessThreshold:    1,
		Timeout:             time.Hour,
		MaxHalfOpenRequests: 1,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	req := ChatRequest{BaseURL: "http://llm-a", Model: "model-a"}
	_, _ = cb.ChatCompletion(context.Background(), req)
	_, _ = cb.ChatCompletion(context.Background(), req)

	_, err := cb.ChatCompletion(context.Background(), req)
	if !errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		t.Fatalf("expected circuit open error, got: %v", err)
	}
	if inner.calls != 2 {
		t.Fatalf("expected no inner call once circuit opens, got %d calls", inner.calls)
	}
}

func TestCircuitBreakingClientUsesSeparateBreakerPerModelEndpoint(t *testing.T) {
	inner := &failingLLM{
		errors: []error{&LLMHTTPError{StatusCode: 500, Status: "500", Body: "err1"}},
	}
	cb := NewCircuitBreakingClient(inner, circuitbreaker.Config{
		FailureThreshold:    1,
		SuccessThreshold:    1,
		Timeout:             time.Hour,
		MaxHalfOpenRequests: 1,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	// Open breaker for endpoint/model A.
	_, _ = cb.ChatCompletion(context.Background(), ChatRequest{BaseURL: "http://llm-a", Model: "model-a"})
	_, err := cb.ChatCompletion(context.Background(), ChatRequest{BaseURL: "http://llm-a", Model: "model-a"})
	if !errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		t.Fatalf("expected circuit open for model-a, got: %v", err)
	}

	// Different model endpoint should still call through.
	_, err = cb.ChatCompletion(context.Background(), ChatRequest{BaseURL: "http://llm-b", Model: "model-b"})
	if err != nil {
		t.Fatalf("expected model-b call to proceed, got: %v", err)
	}
}
