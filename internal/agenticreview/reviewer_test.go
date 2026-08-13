package agenticreview

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"drydock/internal/agenttools"
	"drydock/internal/reviewengine"
	"drydock/internal/targetidentity"
	"drydock/internal/testutil"
	"drydock/internal/workspacesnapshot"
)

func reviewerPatch() string {
	return "diff --git a/changed.go b/changed.go\n" +
		"--- a/changed.go\n+++ b/changed.go\n@@ -1,2 +1,2 @@\n-old\n+new\n keep\n"
}

func reviewerSnapshot(t *testing.T) *workspacesnapshot.Snapshot {
	t.Helper()
	return discoverySnapshot(t, reviewerPatch(), map[string]string{
		"changed.go": "new\nkeep\n",
		"context.go": "package p\nfunc Context() {}\n",
	})
}

func reviewerToolCall(t *testing.T, id, name string, arguments any) reviewengine.ToolCall {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	return reviewengine.ToolCall{
		ID: id, Type: "function",
		Function: reviewengine.ToolCallFunction{Name: name, Arguments: string(encoded)},
	}
}

func reviewerCompletion(calls ...reviewengine.ToolCall) reviewengine.CompletionResult {
	return reviewengine.CompletionResult{
		Message: reviewengine.CompletionMessage{
			Role: reviewengine.MessageRoleAssistant, ToolCalls: calls,
		},
		Usage: reviewengine.CompletionUsage{TotalTokens: 1},
		Model: "served-reviewer",
	}
}

func findingSubmission(file string, line int, evidenceID string) map[string]any {
	return map[string]any{
		"summary": "reviewed",
		"findings": []any{map[string]any{
			"priority": "P1", "category": "correctness", "file": file, "line": line,
			"explanation": "verified defect", "suggestion": "fix it", "confidence": 0.9,
			"evidence_tool_call_ids": []string{evidenceID},
		}},
		"coverage": map[string]any{
			"examined_files": []string{file}, "outcome": "findings", "summary": "examined source",
		},
	}
}

func noFindingSubmission(outcome string) map[string]any {
	return map[string]any{
		"summary":  "no issues",
		"findings": []any{},
		"coverage": map[string]any{
			"examined_files": []string{"changed.go"}, "outcome": outcome,
			"summary": "examined the full changed file",
		},
	}
}

func newTestReviewer(t *testing.T, client reviewengine.CompletionClient, scope FindingScope, limits LoopLimits) *Reviewer {
	t.Helper()
	reviewer, err := NewReviewer(ReviewerConfig{
		Client: client, Registry: agenttools.NewRegistry(), Counter: testCounter{},
		Snapshot: reviewerSnapshot(t), Scope: scope, Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	return reviewer
}

func testReviewerExecutionRequest() reviewengine.ReviewerExecutionRequest {
	const bundle = "finalized context"
	patch := reviewerPatch()
	return reviewengine.ReviewerExecutionRequest{
		Route:       reviewengine.RouteCoder32B,
		Endpoint:    reviewengine.ModelEndpoint{BaseURL: "http://reviewer", Model: "review-model"},
		Temperature: 0.2, System: "engine rubric", User: bundle,
		ContextBundle: bundle, PatchDiff: patch, ChangedFiles: []string{"changed.go"},
		TargetEnvelope: targetidentity.New(
			"repo", "root", "patch-event", "remote", "", "", "", patch, bundle,
		),
	}
}

func executeTestReviewer(reviewer *Reviewer) (reviewengine.ReviewerExecutionResult, error) {
	return reviewer.ExecuteReviewer(context.Background(), testReviewerExecutionRequest())
}

func TestReviewerRejectsMismatchedAuthoritativeMaterialsBeforeToolUse(t *testing.T) {
	client := &scriptedClient{}
	reviewer := newTestReviewer(t, client, FindingScopePatch, LoopLimits{})
	request := testReviewerExecutionRequest()
	request.PatchDiff += "\nmutation"
	result, err := reviewer.ExecuteReviewer(context.Background(), request)
	if !errors.Is(err, ErrReviewerTargetMismatch) {
		t.Fatalf("target mismatch error = %v", err)
	}
	if result.Trace.StopReason != string(StopTargetMismatch) || len(client.requests) != 0 {
		t.Fatalf("target mismatch result=%#v calls=%d", result, len(client.requests))
	}

	request = testReviewerExecutionRequest()
	request.ChangedFiles = []string{"context.go"}
	result, err = reviewer.ExecuteReviewer(context.Background(), request)
	if !errors.Is(err, ErrReviewerTargetMismatch) ||
		result.Trace.StopReason != string(StopTargetMismatch) {
		t.Fatalf("changed-set mismatch result=%#v err=%v", result, err)
	}
}

func TestReviewerSubmitsEvidenceBackedFindingAndConvertsPriority(t *testing.T) {
	client := &scriptedClient{results: []reviewengine.CompletionResult{
		reviewerCompletion(reviewerToolCall(t, "evidence-1", agenttools.ToolCodeRead, map[string]any{
			"path": "changed.go", "start_line": 1, "end_line": 2,
		})),
		reviewerCompletion(reviewerToolCall(t, "submit-1", agenttools.ToolReviewSubmit,
			findingSubmission("changed.go", 1, "evidence-1"))),
	}}
	reviewer := newTestReviewer(t, client, FindingScopePatch, LoopLimits{})
	result, err := executeTestReviewer(reviewer)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Review.Findings) != 1 {
		t.Fatalf("findings = %#v", result.Review.Findings)
	}
	finding := result.Review.Findings[0]
	if finding.Priority != reviewengine.PriorityP1 || finding.Severity != "high" ||
		!strings.Contains(finding.Evidence, "evidence-1") {
		t.Fatalf("converted finding = %#v", finding)
	}
	if result.ServedModel != "served-reviewer" || result.Trace.StopReason != string(StopReviewSubmitted) ||
		result.Trace.Turns != 2 || result.Trace.ToolCalls != 2 {
		t.Fatalf("result trace = %#v", result)
	}
	if !slices.Equal(result.Trace.EvidenceToolCallIDs, []string{"evidence-1"}) ||
		!slices.Equal(result.Trace.ExaminedFiles, []string{"changed.go"}) ||
		result.Trace.CoverageOutcome != "findings" {
		t.Fatalf("evidence/coverage trace = %#v", result.Trace)
	}

	var names []string
	for _, tool := range client.requests[0].Tools {
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, agenttools.ToolReviewSubmit) ||
		!slices.Contains(names, agenttools.ToolCodeRead) ||
		slices.Contains(names, agenttools.ToolSelectionAdd) {
		t.Fatalf("reviewer tools = %v", names)
	}
	if !strings.Contains(client.requests[0].Messages[0].Content, "review.submit") ||
		!strings.Contains(client.requests[0].Messages[0].Content, "engine rubric") {
		t.Fatalf("reviewer system prompt = %q", client.requests[0].Messages[0].Content)
	}
}

func TestReviewerReturnsValidationFailureAsToolErrorThenCorrects(t *testing.T) {
	invalid := findingSubmission("changed.go", 1, "not-from-this-run")
	valid := findingSubmission("changed.go", 1, "evidence-1")
	client := &scriptedClient{results: []reviewengine.CompletionResult{
		reviewerCompletion(reviewerToolCall(t, "evidence-1", agenttools.ToolCodeRead, map[string]any{"path": "changed.go"})),
		reviewerCompletion(reviewerToolCall(t, "bad-submit", agenttools.ToolReviewSubmit, invalid)),
		reviewerCompletion(reviewerToolCall(t, "good-submit", agenttools.ToolReviewSubmit, valid)),
	}}
	result, err := executeTestReviewer(newTestReviewer(t, client, FindingScopePatch, LoopLimits{}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Trace.ToolCalls != 3 || result.Trace.StopReason != string(StopReviewSubmitted) {
		t.Fatalf("trace = %#v", result.Trace)
	}
	var validationMessage *reviewengine.CompletionMessage
	for i := range client.requests[2].Messages {
		message := &client.requests[2].Messages[i]
		if message.Role == reviewengine.MessageRoleTool && message.ToolCallID == "bad-submit" {
			validationMessage = message
			break
		}
	}
	if validationMessage == nil || !strings.Contains(validationMessage.Content, `"is_error":true`) ||
		!strings.Contains(validationMessage.Content, "unknown current-run evidence") {
		t.Fatalf("validation tool message = %#v", validationMessage)
	}
}

func TestReviewerPatchAndSnapshotFindingScopes(t *testing.T) {
	patchClient := &scriptedClient{results: []reviewengine.CompletionResult{
		reviewerCompletion(reviewerToolCall(t, "context-evidence", agenttools.ToolCodeRead, map[string]any{"path": "context.go"})),
		reviewerCompletion(reviewerToolCall(t, "outside-submit", agenttools.ToolReviewSubmit,
			findingSubmission("context.go", 1, "context-evidence"))),
		reviewerCompletion(reviewerToolCall(t, "changed-evidence", agenttools.ToolCodeRead, map[string]any{"path": "changed.go"})),
		reviewerCompletion(reviewerToolCall(t, "inside-submit", agenttools.ToolReviewSubmit,
			findingSubmission("changed.go", 1, "changed-evidence"))),
	}}
	patchResult, err := executeTestReviewer(newTestReviewer(t, patchClient, FindingScopePatch, LoopLimits{}))
	if err != nil {
		t.Fatal(err)
	}
	if patchResult.Review.Findings[0].File != "changed.go" {
		t.Fatalf("patch result = %#v", patchResult.Review)
	}
	var outsideRejected bool
	for _, message := range patchClient.requests[2].Messages {
		if message.ToolCallID == "outside-submit" && strings.Contains(message.Content, "outside") {
			outsideRejected = true
		}
	}
	if !outsideRejected {
		t.Fatal("patch-scope outside finding was not returned as a tool error")
	}

	auditClient := &scriptedClient{results: []reviewengine.CompletionResult{
		reviewerCompletion(reviewerToolCall(t, "context-evidence", agenttools.ToolCodeRead, map[string]any{"path": "context.go"})),
		reviewerCompletion(reviewerToolCall(t, "audit-submit", agenttools.ToolReviewSubmit,
			findingSubmission("context.go", 1, "context-evidence"))),
	}}
	auditResult, err := executeTestReviewer(newTestReviewer(t, auditClient, FindingScopeSnapshot, LoopLimits{}))
	if err != nil {
		t.Fatal(err)
	}
	if auditResult.Review.Findings[0].File != "context.go" {
		t.Fatalf("snapshot-scope result = %#v", auditResult.Review)
	}
}

func TestReviewerRequiresExplicitNoFindingCoverageOutcome(t *testing.T) {
	client := &scriptedClient{results: []reviewengine.CompletionResult{
		reviewerCompletion(reviewerToolCall(t, "bad-empty", agenttools.ToolReviewSubmit, noFindingSubmission("findings"))),
		reviewerCompletion(reviewerToolCall(t, "good-empty", agenttools.ToolReviewSubmit, noFindingSubmission("no_findings"))),
	}}
	result, err := executeTestReviewer(newTestReviewer(t, client, FindingScopePatch, LoopLimits{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Review.Findings) != 0 || result.Trace.CoverageOutcome != "no_findings" {
		t.Fatalf("no-finding result = %#v", result)
	}
	if len(client.requests) != 2 {
		t.Fatalf("completion calls = %d", len(client.requests))
	}
}

func TestReviewerNudgesOnceThenRejectsRepeatedToollessResponses(t *testing.T) {
	client := &scriptedClient{results: []reviewengine.CompletionResult{
		{Message: reviewengine.CompletionMessage{Role: reviewengine.MessageRoleAssistant, Content: "I found something."}, Usage: reviewengine.CompletionUsage{TotalTokens: 1}},
		{Message: reviewengine.CompletionMessage{Role: reviewengine.MessageRoleAssistant, Content: "Here is my review."}, Usage: reviewengine.CompletionUsage{TotalTokens: 1}},
	}}
	result, err := executeTestReviewer(newTestReviewer(t, client, FindingScopePatch, LoopLimits{}))
	if !errors.Is(err, ErrReviewerEmptyResponse) {
		t.Fatalf("empty response error = %v", err)
	}
	if result.Review.Summary != "" || result.Trace.StopReason != string(StopEmptyAssistant) {
		t.Fatalf("partial result leaked: %#v", result)
	}
	if len(client.requests) != 2 {
		t.Fatalf("completion calls = %d", len(client.requests))
	}
	var nudged bool
	for _, message := range client.requests[1].Messages {
		if message.Role == reviewengine.MessageRoleUser && strings.Contains(message.Content, "only by calling review.submit") {
			nudged = true
		}
	}
	if !nudged {
		t.Fatal("corrective nudge was not added")
	}
}

func TestReviewerLimitsAndCancellationReturnNoPartialFindings(t *testing.T) {
	client := &scriptedClient{results: []reviewengine.CompletionResult{
		reviewerCompletion(reviewerToolCall(t, "read-only", agenttools.ToolCodeRead, map[string]any{"path": "changed.go"})),
	}}
	reviewer := newTestReviewer(t, client, FindingScopePatch, LoopLimits{
		MaxTurns: 1, MaxToolCalls: 4, MaxCumulativeTokens: 100_000, MaxModelContext: 100_000,
	})
	result, err := executeTestReviewer(reviewer)
	if !errors.Is(err, ErrReviewSubmitMissing) || !errors.Is(err, ErrTurnLimit) {
		t.Fatalf("turn-limit error = %v", err)
	}
	if result.Review.Summary != "" || len(result.Review.Findings) != 0 ||
		result.Trace.StopReason != string(StopTurnsExhausted) {
		t.Fatalf("partial limit result = %#v", result)
	}

	cancelledClient := &scriptedClient{}
	cancelled := newTestReviewer(t, cancelledClient, FindingScopePatch, LoopLimits{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelledResult, err := cancelled.ExecuteReviewer(ctx, reviewengine.ReviewerExecutionRequest{})
	if !errors.Is(err, context.Canceled) || cancelledResult.Trace.StopReason != string(StopCancelled) {
		t.Fatalf("cancellation result=%#v err=%v", cancelledResult, err)
	}
	if len(cancelledClient.requests) != 0 {
		t.Fatalf("client called after cancellation: %d", len(cancelledClient.requests))
	}
}

func TestReviewerCancellationMidLoopReturnsNoPartialReview(t *testing.T) {
	started := make(chan struct{}, 1)
	client := &testutil.ScriptedAgenticClient{Steps: []testutil.CompletionStep{
		{
			Model: "review-model",
			Result: reviewerCompletion(reviewerToolCall(t, "read-before-cancel", agenttools.ToolCodeRead,
				map[string]any{"path": "changed.go"})),
		},
		{Model: "review-model", WaitForCancel: true, Started: started},
	}}
	reviewer := newTestReviewer(t, client, FindingScopePatch, LoopLimits{
		MaxTurns: 4, MaxToolCalls: 8, MaxCumulativeTokens: 100_000, MaxModelContext: 100_000,
	})
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		result reviewengine.ReviewerExecutionResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := reviewer.ExecuteReviewer(ctx, testReviewerExecutionRequest())
		done <- outcome{result: result, err: err}
	}()
	<-started
	cancel()
	got := <-done
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("mid-loop cancellation error = %v", got.err)
	}
	if got.result.Review.Summary != "" || len(got.result.Review.Findings) != 0 {
		t.Fatalf("partial review leaked after cancellation: %#v", got.result)
	}
	// A cancellation returned by Complete is currently classified as a
	// transport stop; this test pins the fail-closed behavior while the final
	// review epic decides whether to normalize it to StopCancelled.
	if got.result.Trace.StopReason != string(StopTransportError) ||
		!slices.Contains(got.result.Trace.ToolCallIDs, "read-before-cancel") {
		t.Fatalf("cancellation trace = %#v", got.result.Trace)
	}
}

func TestReviewerToolAndTokenLimitsReturnNoPartialFindings(t *testing.T) {
	toolClient := &scriptedClient{results: []reviewengine.CompletionResult{
		reviewerCompletion(
			reviewerToolCall(t, "read-1", agenttools.ToolCodeRead, map[string]any{"path": "changed.go"}),
			reviewerToolCall(t, "read-2", agenttools.ToolCodeRead, map[string]any{"path": "changed.go"}),
		),
	}}
	toolReviewer := newTestReviewer(t, toolClient, FindingScopePatch, LoopLimits{
		MaxTurns: 2, MaxToolCalls: 1, MaxCumulativeTokens: 100_000, MaxModelContext: 100_000,
	})
	toolResult, err := executeTestReviewer(toolReviewer)
	if !errors.Is(err, ErrToolCallLimit) || !errors.Is(err, ErrReviewSubmitMissing) {
		t.Fatalf("tool-limit error = %v", err)
	}
	if toolResult.Review.Summary != "" || toolResult.Trace.StopReason != string(StopToolsExhausted) {
		t.Fatalf("partial tool-limit result = %#v", toolResult)
	}

	tokenClient := &scriptedClient{results: []reviewengine.CompletionResult{{
		Message: reviewengine.CompletionMessage{Role: reviewengine.MessageRoleAssistant},
		Usage:   reviewengine.CompletionUsage{TotalTokens: 100_001},
	}}}
	tokenReviewer := newTestReviewer(t, tokenClient, FindingScopePatch, LoopLimits{
		MaxTurns: 2, MaxToolCalls: 4, MaxCumulativeTokens: 100_000, MaxModelContext: 100_000,
	})
	tokenResult, err := executeTestReviewer(tokenReviewer)
	if !errors.Is(err, ErrTokenLimit) {
		t.Fatalf("token-limit error = %v", err)
	}
	if tokenResult.Review.Summary != "" || tokenResult.Trace.StopReason != string(StopTokensExhausted) {
		t.Fatalf("partial token-limit result = %#v", tokenResult)
	}
}

func TestDefaultReviewerLoopLimits(t *testing.T) {
	limits := DefaultReviewerLoopLimits()
	if limits.MaxTurns != 20 || limits.MaxToolCalls != 96 ||
		limits.MaxCumulativeTokens != 384_000 {
		t.Fatalf("reviewer defaults = %#v", limits)
	}
}
