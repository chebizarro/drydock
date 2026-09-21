package agenticreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/agenttools"
	"git.sharegap.net/cascadia/drydock/internal/contextbuilder"
	"git.sharegap.net/cascadia/drydock/internal/metrics"
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
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

	// TerminalTool is the tool whose successful call ends the loop. Defaults to
	// selection.finalize (the discovery/selection loop).
	TerminalTool string
	// TerminalStop is the stop reason recorded when TerminalTool succeeds.
	// Defaults to StopFinalized.
	TerminalStop StopReason
	// Finalize produces the loop's terminal artifact once TerminalTool succeeds.
	// It reports an error when the terminal handler ran but produced nothing
	// usable. Defaults to reading the Selection's frozen bundle.
	Finalize func() (contextbuilder.ContextBundle, error)

	// EmptyAssistantNudge, when non-empty, is appended once as a corrective user
	// message the first time the assistant returns no tool calls, so the model is
	// re-prompted with guidance instead of an identical request. When
	// EmptyAssistantStop and EmptyAssistantErr are also set, a second consecutive
	// tool-less turn stops the loop with them; otherwise the loop continues until
	// a natural budget limit.
	EmptyAssistantNudge string
	EmptyAssistantStop  StopReason
	EmptyAssistantErr   error

	// OnMessage, when set, receives each assistant and tool message as it is
	// produced (the reviewer's transcript sink). A returned error stops the loop
	// as a transport failure.
	OnMessage func(context.Context, ...reviewengine.CompletionMessage) error
	// OnToolResult, when set, receives every successful non-terminal tool call and
	// its result (the reviewer's evidence ledger).
	OnToolResult func(reviewengine.ToolCall, agenttools.Result)
}

type LoopResult struct {
	Bundle      contextbuilder.ContextBundle
	Trace       LoopTrace
	ServedModel string
}

type LoopRunner struct {
	Client reviewengine.CompletionClient
}

func (r *LoopRunner) Run(ctx context.Context, request LoopRequest) (LoopResult, error) {
	if r == nil || r.Client == nil {
		return LoopResult{}, fmt.Errorf("agentic review: completion client is required")
	}
	if request.Registry == nil || request.Scope == nil {
		return LoopResult{}, fmt.Errorf("agentic review: registry and scope are required")
	}
	if request.Counter == nil {
		return LoopResult{}, contextbuilder.ErrTokenCounterRequired
	}
	limits := normalizeLoopLimits(request.Limits)
	request.Scope.MaxResultBytes = limits.MaxToolResultBytes
	if request.Selection != nil {
		request.Scope.Selection = request.Selection
	}

	terminalTool := request.TerminalTool
	if terminalTool == "" {
		terminalTool = agenttools.ToolSelectionFinalize
	}
	terminalStop := request.TerminalStop
	if terminalStop == "" {
		terminalStop = StopFinalized
	}
	finalize := request.Finalize
	if finalize == nil {
		finalize = func() (contextbuilder.ContextBundle, error) {
			bundle, ok := request.Selection.Bundle()
			if !ok {
				return contextbuilder.ContextBundle{}, fmt.Errorf("%w: finalize handler returned without a frozen bundle", ErrFinalizeMissing)
			}
			return bundle, nil
		}
	}

	definitions := request.Registry.ListForScope(request.Scope)
	request.Completion.Tools = make([]reviewengine.ToolSchema, 0, len(definitions))
	for _, definition := range definitions {
		request.Completion.Tools = append(request.Completion.Tools, reviewengine.ToolSchema{
			Name: definition.Name, Description: definition.Description,
			Parameters: append(json.RawMessage(nil), definition.InputSchema...),
		})
	}

	trace := LoopTrace{}
	servedModel := ""
	emptyNudged := false
	defer observeLoopMetrics(&trace, limits)
	stop := func(reason StopReason, err error) (LoopResult, error) {
		trace.StopReason = reason
		return LoopResult{Trace: trace, ServedModel: servedModel}, err
	}

	for trace.Turns < limits.MaxTurns {
		if err := ctx.Err(); err != nil {
			return stop(StopCancelled, err)
		}
		preflight, err := serializedRequestTokens(request.Completion, request.Counter)
		if err != nil {
			return stop(StopContextExceeded, err)
		}
		if preflight > limits.MaxModelContext {
			return stop(StopContextExceeded, fmt.Errorf("%w: tokens=%d limit=%d", ErrModelContext, preflight, limits.MaxModelContext))
		}
		if trace.CumulativeTokens+preflight > limits.MaxCumulativeTokens {
			return stop(StopTokensExhausted, ErrTokenLimit)
		}

		completion, err := r.Client.Complete(ctx, request.Completion)
		trace.Turns++
		if err != nil {
			if isContextCancellation(ctx, err) {
				return stop(StopCancelled, err)
			}
			return stop(StopTransportError, err)
		}
		if model := strings.TrimSpace(completion.Model); model != "" {
			servedModel = model
		}
		completion.Message.PromptTokens = completion.Usage.PromptTokens
		completion.Message.CompletionTokens = completion.Usage.CompletionTokens
		used := completion.Usage.TotalTokens
		if used <= 0 {
			used = preflight + request.Counter.Count(completion.Message.Content)
			for _, call := range completion.Message.ToolCalls {
				used += request.Counter.Count(call.Function.Name) + request.Counter.Count(call.Function.Arguments)
			}
		}
		trace.CumulativeTokens += used
		if trace.CumulativeTokens > limits.MaxCumulativeTokens {
			return stop(StopTokensExhausted, ErrTokenLimit)
		}

		request.Completion.Messages = append(request.Completion.Messages, completion.Message)
		if request.OnMessage != nil {
			if err := request.OnMessage(ctx, completion.Message); err != nil {
				return stop(StopTransportError, err)
			}
		}

		if len(completion.Message.ToolCalls) == 0 {
			if request.EmptyAssistantNudge != "" && !emptyNudged {
				emptyNudged = true
				request.Completion.Messages = append(request.Completion.Messages, reviewengine.CompletionMessage{
					Role: reviewengine.MessageRoleUser, Content: request.EmptyAssistantNudge,
				})
				continue
			}
			if emptyNudged && request.EmptyAssistantErr != nil {
				return stop(request.EmptyAssistantStop, request.EmptyAssistantErr)
			}
			continue
		}

		for _, call := range completion.Message.ToolCalls {
			if trace.ToolCalls >= limits.MaxToolCalls {
				return stop(StopToolsExhausted, ErrToolCallLimit)
			}
			trace.ToolCalls++
			trace.ToolCallIDs = append(trace.ToolCallIDs, call.ID)
			toolResult, dispatchErr := request.Registry.Dispatch(ctx, agenttools.Invocation{
				ToolCallID: call.ID, Name: call.Function.Name,
				Arguments: json.RawMessage(call.Function.Arguments), Scope: request.Scope,
			})
			if dispatchErr != nil && isContextCancellation(ctx, dispatchErr) {
				return stop(StopCancelled, dispatchErr)
			}
			if dispatchErr != nil {
				toolResult = agenttools.Result{Content: dispatchErr.Error(), IsError: true}
			}
			if request.OnToolResult != nil && dispatchErr == nil && !toolResult.IsError && call.Function.Name != terminalTool {
				request.OnToolResult(call, toolResult)
			}
			encoded, err := json.Marshal(toolResult)
			if err != nil {
				return stop(StopTransportError, err)
			}
			toolMessage := reviewengine.CompletionMessage{
				Role: reviewengine.MessageRoleTool, ToolCallID: call.ID,
				Name: call.Function.Name, Content: string(encoded),
			}
			request.Completion.Messages = append(request.Completion.Messages, toolMessage)
			if request.OnMessage != nil {
				if err := request.OnMessage(ctx, toolMessage); err != nil {
					return stop(StopTransportError, err)
				}
			}
			if call.Function.Name == terminalTool && dispatchErr == nil && !toolResult.IsError {
				bundle, err := finalize()
				if err != nil {
					return stop(StopTransportError, err)
				}
				trace.StopReason = terminalStop
				return LoopResult{Bundle: bundle, Trace: trace, ServedModel: servedModel}, nil
			}
		}
	}
	return stop(StopTurnsExhausted, ErrTurnLimit)
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
