package reviewengine

import (
	"context"
	"fmt"

	"drydock/internal/targetidentity"
)

// ReviewerExecutionRequest is the fully assembled reviewer invocation. The
// engine owns planning, checklist construction, prompt assembly, and route
// resolution before handing this immutable request to an executor.
type ReviewerExecutionRequest struct {
	Route          ModelRoute
	Endpoint       ModelEndpoint
	Temperature    float64
	System         string
	User           string
	Label          string
	ContextBundle  string
	PatchDiff      string
	ChangedFiles   []string
	TargetEnvelope targetidentity.Envelope
}

// ReviewerTrace captures executor-neutral loop metadata. Single-shot
// executors leave it empty; iterative executors populate it for downstream
// meta-review and diagnostics.
type ReviewerTrace struct {
	Turns               int      `json:"turns,omitempty"`
	ToolCalls           int      `json:"tool_calls,omitempty"`
	CumulativeTokens    int      `json:"cumulative_tokens,omitempty"`
	ToolCallIDs         []string `json:"tool_call_ids,omitempty"`
	EvidenceToolCallIDs []string `json:"evidence_tool_call_ids,omitempty"`
	ExaminedFiles       []string `json:"examined_files,omitempty"`
	CoverageOutcome     string   `json:"coverage_outcome,omitempty"`
	CoverageSummary     string   `json:"coverage_summary,omitempty"`
	StopReason          string   `json:"stop_reason,omitempty"`
}

// ReviewerExecutionResult is the executor output consumed by the engine.
type ReviewerExecutionResult struct {
	Review      ReviewerOutput
	ServedModel string
	Trace       ReviewerTrace
}

// ReviewerExecutor executes the reviewer stage only. Planner, checklist,
// prompts, finding scope filtering, consensus, and walkthrough remain owned by
// Engine.
type ReviewerExecutor interface {
	ExecuteReviewer(context.Context, ReviewerExecutionRequest) (ReviewerExecutionResult, error)
}

// ReviewerExecutorFactory must return a fresh executor for each ensemble
// member. This prevents transcripts, evidence ledgers, and loop counters from
// leaking between concurrent reviewers.
type ReviewerExecutorFactory func(ModelRoute) ReviewerExecutor

type singleShotReviewerExecutor struct {
	engine *Engine
}

func (x *singleShotReviewerExecutor) ExecuteReviewer(ctx context.Context, request ReviewerExecutionRequest) (ReviewerExecutionResult, error) {
	if x == nil || x.engine == nil {
		return ReviewerExecutionResult{}, fmt.Errorf("review engine: single-shot executor is unavailable")
	}
	review, servedModel, err := x.engine.completeStructuredReviewer(ctx, ChatRequest{
		BaseURL:     request.Endpoint.BaseURL,
		APIKey:      request.Endpoint.APIKey,
		Model:       request.Endpoint.Model,
		Temperature: request.Temperature,
		System:      request.System,
		User:        request.User,
	}, request.Label)
	if err != nil {
		return ReviewerExecutionResult{}, err
	}
	return ReviewerExecutionResult{Review: review, ServedModel: servedModel}, nil
}

func (e *Engine) singleShotExecutor() ReviewerExecutor {
	return &singleShotReviewerExecutor{engine: e}
}

func normalizeReviewerExecution(result ReviewerExecutionResult) (ReviewerExecutionResult, error) {
	findings, err := NormalizeFindings(result.Review.Findings)
	if err != nil {
		return ReviewerExecutionResult{}, fmt.Errorf("reviewer executor output: %w", err)
	}
	result.Review.Findings = findings
	if err := result.Review.Validate(); err != nil {
		return ReviewerExecutionResult{}, fmt.Errorf("reviewer executor output: %w", err)
	}
	result.Trace.ToolCallIDs = append([]string(nil), result.Trace.ToolCallIDs...)
	result.Trace.EvidenceToolCallIDs = append([]string(nil), result.Trace.EvidenceToolCallIDs...)
	result.Trace.ExaminedFiles = append([]string(nil), result.Trace.ExaminedFiles...)
	return result, nil
}
