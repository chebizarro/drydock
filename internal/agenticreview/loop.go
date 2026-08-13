package agenticreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"drydock/internal/agenttools"
	"drydock/internal/contextbuilder"
	"drydock/internal/metrics"
	"drydock/internal/reviewengine"
)

const (
	DefaultMaxTurns            = 24
	DefaultMaxToolCalls        = 96
	DefaultMaxCumulativeTokens = 256_000
	DefaultMaxToolResultBytes  = 16 * 1024
	DefaultMaxModelContext     = 256_000
)

var (
	ErrTurnLimit       = errors.New("agentic review: turn limit exhausted")
	ErrToolCallLimit   = errors.New("agentic review: tool call limit exhausted")
	ErrTokenLimit      = errors.New("agentic review: cumulative token limit exhausted")
	ErrModelContext    = errors.New("agentic review: model context preflight failed")
	ErrFinalizeMissing = errors.New("agentic review: selection.finalize was not called successfully")
)

type LoopLimits struct {
	MaxTurns            int
	MaxToolCalls        int
	MaxCumulativeTokens int
	MaxToolResultBytes  int
	MaxModelContext     int
}

func DefaultLoopLimits() LoopLimits {
	return LoopLimits{
		MaxTurns: DefaultMaxTurns, MaxToolCalls: DefaultMaxToolCalls,
		MaxCumulativeTokens: DefaultMaxCumulativeTokens,
		MaxToolResultBytes:  DefaultMaxToolResultBytes,
		MaxModelContext:     DefaultMaxModelContext,
	}
}

type StopReason string

const (
	StopFinalized       StopReason = "selection_finalized"
	StopTurnsExhausted  StopReason = "turn_limit"
	StopToolsExhausted  StopReason = "tool_call_limit"
	StopTokensExhausted StopReason = "cumulative_token_limit"
	StopContextExceeded StopReason = "model_context_limit"
	StopTransportError  StopReason = "transport_error"
)

type LoopTrace struct {
	Turns            int        `json:"turns"`
	ToolCalls        int        `json:"tool_calls"`
	CumulativeTokens int        `json:"cumulative_tokens"`
	StopReason       StopReason `json:"stop_reason"`
	ToolCallIDs      []string   `json:"tool_call_ids,omitempty"`
}

type LoopRequest struct {
	Completion reviewengine.CompletionRequest
	Registry   *agenttools.Registry
	Scope      *agenttools.Scope
	Selection  *agenttools.Selection
	Counter    contextbuilder.TokenCounter
	Limits     LoopLimits
}

type LoopResult struct {
	Bundle contextbuilder.ContextBundle
	Trace  LoopTrace
}

type LoopRunner struct {
	Client reviewengine.CompletionClient
}

func (r *LoopRunner) Run(ctx context.Context, request LoopRequest) (LoopResult, error) {
	if r == nil || r.Client == nil {
		return LoopResult{}, fmt.Errorf("agentic review: completion client is required")
	}
	if request.Registry == nil || request.Scope == nil || request.Selection == nil {
		return LoopResult{}, fmt.Errorf("agentic review: registry, scope, and selection are required")
	}
	if request.Counter == nil {
		return LoopResult{}, contextbuilder.ErrTokenCounterRequired
	}
	limits := normalizeLoopLimits(request.Limits)
	request.Scope.MaxResultBytes = limits.MaxToolResultBytes
	request.Scope.Selection = request.Selection

	definitions := request.Registry.ListForScope(request.Scope)
	request.Completion.Tools = make([]reviewengine.ToolSchema, 0, len(definitions))
	for _, definition := range definitions {
		request.Completion.Tools = append(request.Completion.Tools, reviewengine.ToolSchema{
			Name: definition.Name, Description: definition.Description,
			Parameters: append(json.RawMessage(nil), definition.InputSchema...),
		})
	}

	trace := LoopTrace{}
	defer observeLoopMetrics(&trace, limits)
	for trace.Turns < limits.MaxTurns {
		if err := ctx.Err(); err != nil {
			trace.StopReason = StopCancelled
			return LoopResult{Trace: trace}, err
		}
		preflight, err := serializedRequestTokens(request.Completion, request.Counter)
		if err != nil {
			trace.StopReason = StopContextExceeded
			return LoopResult{Trace: trace}, err
		}
		if preflight > limits.MaxModelContext {
			trace.StopReason = StopContextExceeded
			return LoopResult{Trace: trace}, fmt.Errorf("%w: tokens=%d limit=%d", ErrModelContext, preflight, limits.MaxModelContext)
		}
		if trace.CumulativeTokens+preflight > limits.MaxCumulativeTokens {
			trace.StopReason = StopTokensExhausted
			return LoopResult{Trace: trace}, ErrTokenLimit
		}

		completion, err := r.Client.Complete(ctx, request.Completion)
		trace.Turns++
		if err != nil {
			if isContextCancellation(ctx, err) {
				trace.StopReason = StopCancelled
			} else {
				trace.StopReason = StopTransportError
			}
			return LoopResult{Trace: trace}, err
		}
		used := completion.Usage.TotalTokens
		if used <= 0 {
			used = preflight + request.Counter.Count(completion.Message.Content)
			for _, call := range completion.Message.ToolCalls {
				used += request.Counter.Count(call.Function.Name) + request.Counter.Count(call.Function.Arguments)
			}
		}
		trace.CumulativeTokens += used
		if trace.CumulativeTokens > limits.MaxCumulativeTokens {
			trace.StopReason = StopTokensExhausted
			return LoopResult{Trace: trace}, ErrTokenLimit
		}

		request.Completion.Messages = append(request.Completion.Messages, completion.Message)
		for _, call := range completion.Message.ToolCalls {
			if trace.ToolCalls >= limits.MaxToolCalls {
				trace.StopReason = StopToolsExhausted
				return LoopResult{Trace: trace}, ErrToolCallLimit
			}
			trace.ToolCalls++
			trace.ToolCallIDs = append(trace.ToolCallIDs, call.ID)
			toolResult, dispatchErr := request.Registry.Dispatch(ctx, agenttools.Invocation{
				ToolCallID: call.ID, Name: call.Function.Name,
				Arguments: json.RawMessage(call.Function.Arguments), Scope: request.Scope,
			})
			if dispatchErr != nil && isContextCancellation(ctx, dispatchErr) {
				trace.StopReason = StopCancelled
				return LoopResult{Trace: trace}, dispatchErr
			}
			if dispatchErr != nil {
				toolResult = agenttools.Result{Content: dispatchErr.Error(), IsError: true}
			}
			encoded, err := json.Marshal(toolResult)
			if err != nil {
				trace.StopReason = StopTransportError
				return LoopResult{Trace: trace}, err
			}
			request.Completion.Messages = append(request.Completion.Messages, reviewengine.CompletionMessage{
				Role: reviewengine.MessageRoleTool, ToolCallID: call.ID,
				Name: call.Function.Name, Content: string(encoded),
			})
			if call.Function.Name == agenttools.ToolSelectionFinalize && dispatchErr == nil && !toolResult.IsError {
				bundle, ok := request.Selection.Bundle()
				if !ok {
					trace.StopReason = StopTransportError
					return LoopResult{Trace: trace}, fmt.Errorf("%w: finalize handler returned without a frozen bundle", ErrFinalizeMissing)
				}
				trace.StopReason = StopFinalized
				return LoopResult{Bundle: bundle, Trace: trace}, nil
			}
		}
	}
	trace.StopReason = StopTurnsExhausted
	return LoopResult{Trace: trace}, ErrTurnLimit
}

func isContextCancellation(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil
}

func normalizeLoopLimits(limits LoopLimits) LoopLimits {
	defaults := DefaultLoopLimits()
	if limits.MaxTurns <= 0 {
		limits.MaxTurns = defaults.MaxTurns
	}
	if limits.MaxToolCalls <= 0 {
		limits.MaxToolCalls = defaults.MaxToolCalls
	}
	if limits.MaxCumulativeTokens <= 0 {
		limits.MaxCumulativeTokens = defaults.MaxCumulativeTokens
	}
	if limits.MaxToolResultBytes <= 0 {
		limits.MaxToolResultBytes = defaults.MaxToolResultBytes
	}
	if limits.MaxModelContext <= 0 {
		limits.MaxModelContext = defaults.MaxModelContext
	}
	return limits
}

func observeLoopMetrics(trace *LoopTrace, limits LoopLimits) {
	if trace == nil || trace.StopReason == "" {
		return
	}
	metrics.AgenticLoopTurns.Add(int64(trace.Turns))
	metrics.AgenticStopReasons.With(string(trace.StopReason)).Inc()
	if limits.MaxTurns > 0 {
		metrics.AgenticBudgetUtilization.With("turns").Observe(float64(trace.Turns) / float64(limits.MaxTurns))
	}
	if limits.MaxToolCalls > 0 {
		metrics.AgenticBudgetUtilization.With("tool_calls").Observe(float64(trace.ToolCalls) / float64(limits.MaxToolCalls))
	}
	if limits.MaxCumulativeTokens > 0 {
		metrics.AgenticBudgetUtilization.With("cumulative_tokens").Observe(float64(trace.CumulativeTokens) / float64(limits.MaxCumulativeTokens))
	}
}

func serializedRequestTokens(request reviewengine.CompletionRequest, counter contextbuilder.TokenCounter) (int, error) {
	payload := struct {
		Messages []reviewengine.CompletionMessage `json:"messages"`
		Tools    []reviewengine.ToolSchema        `json:"tools,omitempty"`
	}{Messages: request.Messages, Tools: request.Tools}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("agentic review: serialize model context: %w", err)
	}
	return counter.Count(string(encoded)), nil
}
