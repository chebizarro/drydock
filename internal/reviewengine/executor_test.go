package reviewengine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

type plannerWalkthroughClient struct {
	mu                sync.Mutex
	requests          []ChatRequest
	plannerCalls      int
	walkthroughCalls  int
	unexpectedReviews int
}

func (c *plannerWalkthroughClient) ChatCompletion(_ context.Context, request ChatRequest) (ChatResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, request)
	switch {
	case strings.Contains(request.System, "route the review") || strings.Contains(request.System, "planner"):
		c.plannerCalls++
		return ChatResult{Content: `{"change_type":"feature","risk_areas":[],"needed_context":[],"review_focus":"correctness","model_route":"coder32b"}`}, nil
	case strings.Contains(request.System, "walkthrough"):
		c.walkthroughCalls++
		return ChatResult{Content: `{"walkthrough":"one walkthrough","file_summaries":[{"file":"main.go","summary":"changed"}]}`}, nil
	default:
		c.unexpectedReviews++
		return ChatResult{}, errors.New("reviewer should have been delegated to executor")
	}
}

type reviewerExecutorFunc func(context.Context, ReviewerExecutionRequest) (ReviewerExecutionResult, error)

func (f reviewerExecutorFunc) ExecuteReviewer(ctx context.Context, request ReviewerExecutionRequest) (ReviewerExecutionResult, error) {
	return f(ctx, request)
}

type recordingReviewerExecutor struct {
	mu       sync.Mutex
	requests []ReviewerExecutionRequest
	result   ReviewerExecutionResult
	err      error
}

func (x *recordingReviewerExecutor) ExecuteReviewer(_ context.Context, request ReviewerExecutionRequest) (ReviewerExecutionResult, error) {
	x.mu.Lock()
	x.requests = append(x.requests, request)
	x.mu.Unlock()
	return x.result, x.err
}

func newExecutorTestEngine(client LLMClient) *Engine {
	return New(Config{
		Planner:      ModelEndpoint{BaseURL: "planner", Model: "planner"},
		Coder32B:     ModelEndpoint{BaseURL: "coder", Model: "coder32b"},
		LLM70B:       ModelEndpoint{BaseURL: "large", Model: "llm70b"},
		ReviewerTemp: 0.25,
	}, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRunWithExecutorKeepsEngineOwnedStages(t *testing.T) {
	client := &plannerWalkthroughClient{}
	executor := &recordingReviewerExecutor{result: ReviewerExecutionResult{
		Review: ReviewerOutput{
			Summary: "executor review",
			Findings: []Finding{
				{Priority: PriorityP1, Category: "correctness", File: "main.go", Line: 3, Confidence: 0.9},
				{Priority: PriorityP0, Category: "security", File: "context.go", Line: 7, Confidence: 0.95},
			},
		},
		ServedModel: "served-agent",
		Trace:       ReviewerTrace{Turns: 2, ToolCalls: 3, StopReason: "review_submitted"},
	}}
	engine := newExecutorTestEngine(client)
	out, err := engine.RunWithExecutor(context.Background(), RunInput{
		ContextBundle:          "finalized package",
		PatchDiff:              "diff --git a/main.go b/main.go",
		ChangedFiles:           []string{"main.go"},
		AdditionalInstructions: "Preserve compatibility.",
		TestCoverageGaps:       []string{"Handle"},
	}, executor)
	if err != nil {
		t.Fatal(err)
	}
	if len(executor.requests) != 1 {
		t.Fatalf("executor calls = %d", len(executor.requests))
	}
	request := executor.requests[0]
	if request.Route != RouteCoder32B || request.Endpoint.Model != "coder32b" || request.Temperature != 0.25 {
		t.Fatalf("execution request routing = %#v", request)
	}
	if request.ContextBundle != "finalized package" ||
		request.PatchDiff != "diff --git a/main.go b/main.go" ||
		len(request.ChangedFiles) != 1 || request.ChangedFiles[0] != "main.go" {
		t.Fatalf("authoritative execution materials = %#v", request)
	}
	if !strings.Contains(request.System, "Preserve compatibility") ||
		!strings.Contains(request.System, "Handle") ||
		!strings.Contains(request.User, "finalized package") {
		t.Fatalf("engine-owned prompt assembly missing: %#v", request)
	}
	if len(out.Review.Findings) != 1 || out.Review.Findings[0].File != "main.go" {
		t.Fatalf("engine-owned finding filtering failed: %#v", out.Review.Findings)
	}
	if out.Review.Findings[0].Severity != "high" || out.Review.Findings[0].Priority != PriorityP1 {
		t.Fatalf("priority compatibility mapping missing: %#v", out.Review.Findings[0])
	}
	if out.Walkthrough.Walkthrough != "one walkthrough" || client.walkthroughCalls != 1 {
		t.Fatalf("walkthrough = %#v calls=%d", out.Walkthrough, client.walkthroughCalls)
	}
	if client.unexpectedReviews != 0 || out.ServedModel != "served-agent" || out.ReviewerTrace.Turns != 2 {
		t.Fatalf("unexpected output/client state: out=%#v client=%#v", out, client)
	}
}

func TestRunWithExecutorReturnsFailureTraceWithoutPartialReview(t *testing.T) {
	client := &plannerWalkthroughClient{}
	executor := &recordingReviewerExecutor{
		result: ReviewerExecutionResult{Trace: ReviewerTrace{
			Turns: 20, ToolCalls: 9, StopReason: "turn_limit",
		}},
		err: errors.New("review.submit missing"),
	}
	out, err := newExecutorTestEngine(client).RunWithExecutor(context.Background(), RunInput{
		ContextBundle: "bundle", PatchDiff: "patch", ChangedFiles: []string{"main.go"},
		SkipWalkthrough: true,
	}, executor)
	if err == nil {
		t.Fatal("expected executor failure")
	}
	if out.Review.Summary != "" || len(out.Review.Findings) != 0 ||
		out.ReviewerTrace.StopReason != "turn_limit" || out.ReviewerTrace.Turns != 20 {
		t.Fatalf("failure output leaked review or lost trace: %#v", out)
	}
	if client.walkthroughCalls != 0 {
		t.Fatalf("walkthrough ran after reviewer failure: %d", client.walkthroughCalls)
	}
}

func TestRunEnsembleWithExecutorsUsesIsolatedMembersAndOneWalkthrough(t *testing.T) {
	client := &plannerWalkthroughClient{}
	engine := newExecutorTestEngine(client)

	var factoryMu sync.Mutex
	var members []*recordingReviewerExecutor
	factory := func(route ModelRoute) ReviewerExecutor {
		member := &recordingReviewerExecutor{result: ReviewerExecutionResult{
			Review: ReviewerOutput{Summary: string(route), Findings: []Finding{{
				Priority: PriorityP2, Category: "correctness", File: "main.go",
				Line: 4, Confidence: 0.8,
			}}},
			ServedModel: "served-" + string(route),
			Trace:       ReviewerTrace{Turns: 1, ToolCalls: 1, StopReason: "review_submitted"},
		}}
		factoryMu.Lock()
		members = append(members, member)
		factoryMu.Unlock()
		return member
	}
	out, err := engine.RunEnsembleWithExecutors(context.Background(), RunInput{
		ContextBundle: "shared finalized package",
		ChangedFiles:  []string{"main.go"},
	}, EnsembleConfig{Models: []ModelRoute{RouteCoder32B, RouteLLM70B}}, factory)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 || members[0] == members[1] {
		t.Fatalf("factory did not create isolated members: %#v", members)
	}
	for _, member := range members {
		if len(member.requests) != 1 {
			t.Fatalf("member requests = %d", len(member.requests))
		}
		if !strings.Contains(member.requests[0].User, "shared finalized package") {
			t.Fatalf("member did not receive shared package: %#v", member.requests[0])
		}
	}
	if client.plannerCalls != 1 || client.walkthroughCalls != 1 || client.unexpectedReviews != 0 {
		t.Fatalf("client calls planner=%d walkthrough=%d reviewer=%d",
			client.plannerCalls, client.walkthroughCalls, client.unexpectedReviews)
	}
	if len(out.EnsembleStatus.ReviewerTraces) != 2 || out.EnsembleStatus.Degraded {
		t.Fatalf("ensemble status = %#v", out.EnsembleStatus)
	}
}

func TestRunEnsembleWithExecutorsTreatsParentCancellationAsRunFailure(t *testing.T) {
	client := &plannerWalkthroughClient{}
	engine := newExecutorTestEngine(client)
	successDone := make(chan struct{})
	cancelReady := make(chan struct{})
	success := reviewerExecutorFunc(func(context.Context, ReviewerExecutionRequest) (ReviewerExecutionResult, error) {
		close(successDone)
		return ReviewerExecutionResult{Review: ReviewerOutput{Summary: "must not publish"}}, nil
	})
	cancelled := reviewerExecutorFunc(func(ctx context.Context, _ ReviewerExecutionRequest) (ReviewerExecutionResult, error) {
		<-successDone
		close(cancelReady)
		<-ctx.Done()
		return ReviewerExecutionResult{Trace: ReviewerTrace{StopReason: "cancelled"}}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	type runResult struct {
		output RunOutput
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		output, err := engine.RunEnsembleWithExecutors(ctx, RunInput{
			ContextBundle: "shared", ChangedFiles: []string{"main.go"}, SkipWalkthrough: true,
		}, EnsembleConfig{Models: []ModelRoute{RouteCoder32B, RouteLLM70B}}, func(route ModelRoute) ReviewerExecutor {
			if route == RouteCoder32B {
				return success
			}
			return cancelled
		})
		done <- runResult{output: output, err: err}
	}()
	<-cancelReady
	cancel()
	result := <-done
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("parent cancellation error = %v", result.err)
	}
	if result.output.Review.Summary != "" || len(result.output.EnsembleStatus.SucceededReviewers) != 1 ||
		len(result.output.EnsembleStatus.FailedReviewers) != 1 {
		t.Fatalf("cancellation output = %#v", result.output)
	}
}

func TestRunEnsembleWithExecutorsDropsFailuresAndFailsOnlyWhenAllFail(t *testing.T) {
	client := &plannerWalkthroughClient{}
	engine := newExecutorTestEngine(client)
	success := &recordingReviewerExecutor{result: ReviewerExecutionResult{
		Review: ReviewerOutput{Summary: "survivor", Findings: nil},
	}}
	failure := &recordingReviewerExecutor{
		result: ReviewerExecutionResult{Trace: ReviewerTrace{Turns: 20, ToolCalls: 8, StopReason: "turn_limit"}},
		err:    errors.New("loop exhausted without review.submit"),
	}
	out, err := engine.RunEnsembleWithExecutors(context.Background(), RunInput{
		ContextBundle: "shared", ChangedFiles: []string{"main.go"}, SkipWalkthrough: true,
	}, EnsembleConfig{Models: []ModelRoute{RouteCoder32B, RouteLLM70B}}, func(route ModelRoute) ReviewerExecutor {
		if route == RouteCoder32B {
			return success
		}
		return failure
	})
	if err != nil {
		t.Fatalf("degraded ensemble failed: %v", err)
	}
	if !out.EnsembleStatus.Degraded || len(out.EnsembleStatus.FailedReviewers) != 1 ||
		out.Review.Summary != "survivor" {
		t.Fatalf("degraded output = %#v", out)
	}
	if len(out.EnsembleStatus.ReviewerTraces) != 1 ||
		out.EnsembleStatus.ReviewerTraces[0].Trace.StopReason != "turn_limit" {
		t.Fatalf("failed member trace missing: %#v", out.EnsembleStatus.ReviewerTraces)
	}

	_, err = engine.RunEnsembleWithExecutors(context.Background(), RunInput{
		ContextBundle: "shared", ChangedFiles: []string{"main.go"}, SkipWalkthrough: true,
	}, EnsembleConfig{Models: []ModelRoute{RouteCoder32B, RouteLLM70B}}, func(ModelRoute) ReviewerExecutor {
		return &recordingReviewerExecutor{err: errors.New("no submission")}
	})
	if err == nil || !strings.Contains(err.Error(), "all 2 ensemble reviewer(s) failed") {
		t.Fatalf("all-failed error = %v", err)
	}
}
