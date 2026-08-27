// Package review provides provider-neutral conversational review streaming.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

// Role identifies a conversation message's author.
type Role string

const (
	// RoleSystem is an instruction message.
	RoleSystem Role = "system"
	// RoleUser is a caller message.
	RoleUser Role = "user"
	// RoleAssistant is a model message.
	RoleAssistant Role = "assistant"
	// RoleTool is a tool result message.
	RoleTool Role = "tool"
)

// Message is one provider-neutral conversation message.
type Message struct {
	// Role identifies the message author.
	Role Role
	// Content is the message text.
	Content string
	// Name optionally identifies the author or tool.
	Name string
	// ToolCallID associates a tool result with its request.
	ToolCallID string
}

// ContextItem is one named piece of review context.
type ContextItem struct {
	// Name is the context section heading.
	Name string
	// Content is the context section body.
	Content string
}

// ModelSelection chooses a registered provider and one of its models.
type ModelSelection struct {
	// Provider is the registered provider name.
	Provider string
	// Model is the provider-specific model identifier.
	Model string
}

// Tool describes an operation the model may request.
type Tool struct {
	// Name is the operation name.
	Name string
	// Description explains the operation to the model.
	Description string
	// Parameters is the operation's JSON Schema.
	Parameters json.RawMessage
}

// RequestOptions controls provider generation behavior.
type RequestOptions struct {
	// Temperature controls provider sampling.
	Temperature float64
	// Tools lists operations available to the model.
	Tools []Tool
}

// Request is a provider-neutral review completion request.
type Request struct {
	// Model selects the registered provider and model.
	Model ModelSelection
	// Messages is the ordered conversation input.
	Messages []Message
	// Context is prepended as a deterministic system message.
	Context []ContextItem
	// Options controls generation and available tools.
	Options RequestOptions
}

// Usage is cumulative token accounting reported by a provider.
type Usage struct {
	// InputTokens is the provider-reported prompt token count.
	InputTokens int
	// OutputTokens is the provider-reported generated token count.
	OutputTokens int
	// TotalTokens is the provider-reported total token count.
	TotalTokens int
}

// OperationStatus reports progress for a tool call or provider operation.
type OperationStatus struct {
	// ID is the provider or model-assigned operation identifier.
	ID string
	// Name is the operation name.
	Name string
	// Status is the provider-neutral operation state.
	Status string
	// Detail contains optional progress or failure detail.
	Detail string
}

// EventKind identifies a stream event.
type EventKind string

const (
	// EventContentDelta carries incremental assistant text.
	EventContentDelta EventKind = "content_delta"
	// EventUsage carries cumulative provider token accounting.
	EventUsage EventKind = "usage"
	// EventOperationStatus carries tool or provider operation progress.
	EventOperationStatus EventKind = "operation_status"
	// EventCompletion is a successful terminal event.
	EventCompletion EventKind = "completion"
	// EventCancellation is a canceled terminal event.
	EventCancellation EventKind = "cancellation"
	// EventFailure is a failed terminal event.
	EventFailure EventKind = "failure"
)

// TerminalStatus identifies how a stream ended.
type TerminalStatus string

const (
	// StatusCompleted indicates successful completion.
	StatusCompleted TerminalStatus = "completed"
	// StatusCanceled indicates context cancellation.
	StatusCanceled TerminalStatus = "canceled"
	// StatusFailed indicates provider or transport failure.
	StatusFailed TerminalStatus = "failed"
)

// Result is the terminal snapshot of a stream. Content and Usage include all
// partial data observed before completion, cancellation, or failure.
type Result struct {
	// Status identifies the terminal outcome.
	Status TerminalStatus
	// Content is all accumulated assistant content.
	Content string
	// Usage is the latest cumulative token accounting.
	Usage Usage
	// Model is the provider-reported model or requested model fallback.
	Model string
}

// Error is a stable provider-neutral failure description.
type Error struct {
	// Code is a stable provider-neutral error code.
	Code string
	// Message is a human-readable failure description.
	Message string
	// Provider identifies the selected provider.
	Provider string
	// StatusCode is an HTTP status when applicable.
	StatusCode int
	// Retryable indicates whether a later attempt may succeed.
	Retryable bool
}

// Event is one incremental or terminal stream item. Result is populated only
// for terminal completion, cancellation, and failure events.
type Event struct {
	// Kind identifies the event payload and terminal semantics.
	Kind EventKind
	// Delta contains text for EventContentDelta.
	Delta string
	// Usage contains accounting for EventUsage.
	Usage Usage
	// Operation contains progress for EventOperationStatus.
	Operation *OperationStatus
	// Result contains the accumulated terminal snapshot.
	Result *Result
	// Error contains mapped details for cancellation or failure.
	Error *Error
}

// ProviderRequest is the request passed to a registered provider. It is owned
// by this package and contains no drydock internal types.
type ProviderRequest struct {
	// Model is the selected provider-specific model identifier.
	Model string
	// Messages is the ordered conversation, including rendered context.
	Messages []Message
	// Temperature controls provider sampling.
	Temperature float64
	// Tools lists operations available to the model.
	Tools []Tool
}

// ProviderResponse is the synchronous provider result used by compatibility
// providers that do not implement StreamingProvider.
type ProviderResponse struct {
	// Content is the complete assistant response.
	Content string
	// Usage is provider-reported cumulative accounting.
	Usage Usage
	// Model is the model the provider reports serving.
	Model string
}

// Provider performs a whole-response completion. Engine adapts it to the event
// model when native streaming is unavailable.
type Provider interface {
	Complete(context.Context, ProviderRequest) (ProviderResponse, error)
}

// StreamingProvider optionally adds native incremental completion streaming.
// The provider must close the returned channel and may use terminal events to
// report completion or failure.
type StreamingProvider interface {
	Provider
	Stream(context.Context, ProviderRequest) (<-chan Event, error)
}

// ProviderFailure lets provider adapters supply stable error metadata.
type ProviderFailure struct {
	// Code is a stable adapter-defined failure code.
	Code string
	// Message is the human-readable failure description.
	Message string
	// StatusCode is an HTTP status when applicable.
	StatusCode int
	// Retryable indicates whether a later attempt may succeed.
	Retryable bool
}

// Error implements error.
func (e *ProviderFailure) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// Engine routes requests to registered providers and normalizes their streams.
// It is safe for concurrent use when its providers are safe for concurrent use.
type Engine struct {
	providers map[string]Provider
}

// New constructs an Engine from a provider registry. The registry is copied.
func New(providers map[string]Provider) (*Engine, error) {
	copyProviders := make(map[string]Provider, len(providers))
	for name, provider := range providers {
		name = strings.TrimSpace(name)
		if name == "" || provider == nil {
			return nil, fmt.Errorf("review: provider names and implementations are required")
		}
		copyProviders[name] = provider
	}
	return &Engine{providers: copyProviders}, nil
}

// Stream begins a review completion. Validation and provider lookup errors are
// returned immediately; runtime outcomes arrive as exactly one terminal event.
func (e *Engine) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	if ctx == nil {
		return nil, fmt.Errorf("review: context is required")
	}
	if e == nil {
		return nil, fmt.Errorf("review: engine is not initialized")
	}
	providerName := strings.TrimSpace(req.Model.Provider)
	model := strings.TrimSpace(req.Model.Model)
	if providerName == "" || model == "" {
		return nil, fmt.Errorf("review: provider and model are required")
	}
	provider, ok := e.providers[providerName]
	if !ok {
		return nil, fmt.Errorf("review: provider %q is not registered", providerName)
	}
	req.Model.Provider = providerName
	req.Model.Model = model
	providerReq, err := buildProviderRequest(req)
	if err != nil {
		return nil, err
	}

	client := completionAdapter{provider: provider, request: providerReq}
	var internalClient reviewengine.CompletionClient = client
	if streaming, ok := provider.(StreamingProvider); ok {
		internalClient = streamingCompletionAdapter{completionAdapter: client, streaming: streaming}
	}
	internalEvents, err := reviewengine.StreamCompletion(ctx, internalClient, client.internalRequest())
	if err != nil {
		return nil, err
	}
	out := make(chan Event)
	go normalizeStream(ctx, providerName, model, internalEvents, out)
	return out, nil
}

func buildProviderRequest(req Request) (ProviderRequest, error) {
	messages := append([]Message(nil), req.Messages...)
	for i, message := range messages {
		switch message.Role {
		case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		default:
			return ProviderRequest{}, fmt.Errorf("review: message[%d] has invalid role %q", i, message.Role)
		}
	}
	if len(req.Context) > 0 {
		var b strings.Builder
		b.WriteString("Review context:\n")
		for _, item := range req.Context {
			if item.Name != "" {
				fmt.Fprintf(&b, "\n## %s\n", item.Name)
			}
			b.WriteString(item.Content)
			if !strings.HasSuffix(item.Content, "\n") {
				b.WriteByte('\n')
			}
		}
		messages = append([]Message{{Role: RoleSystem, Content: b.String()}}, messages...)
	}
	tools := make([]Tool, len(req.Options.Tools))
	for i, tool := range req.Options.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return ProviderRequest{}, fmt.Errorf("review: tool[%d] name is required", i)
		}
		if len(tool.Parameters) > 0 && !json.Valid(tool.Parameters) {
			return ProviderRequest{}, fmt.Errorf("review: tool[%d] parameters are not valid JSON", i)
		}
		tools[i] = tool
		tools[i].Parameters = append(json.RawMessage(nil), tool.Parameters...)
	}
	return ProviderRequest{Model: req.Model.Model, Messages: messages, Temperature: req.Options.Temperature, Tools: tools}, nil
}

type completionAdapter struct {
	provider Provider
	request  ProviderRequest
}

func (a completionAdapter) internalRequest() reviewengine.CompletionRequest {
	messages := make([]reviewengine.CompletionMessage, len(a.request.Messages))
	for i, message := range a.request.Messages {
		messages[i] = reviewengine.CompletionMessage{Role: reviewengine.MessageRole(message.Role), Content: message.Content, Name: message.Name, ToolCallID: message.ToolCallID}
	}
	tools := make([]reviewengine.ToolSchema, len(a.request.Tools))
	for i, tool := range a.request.Tools {
		tools[i] = reviewengine.ToolSchema{Name: tool.Name, Description: tool.Description, Parameters: append(json.RawMessage(nil), tool.Parameters...)}
	}
	return reviewengine.CompletionRequest{Model: a.request.Model, Temperature: a.request.Temperature, Messages: messages, Tools: tools}
}

func (a completionAdapter) Complete(ctx context.Context, _ reviewengine.CompletionRequest) (reviewengine.CompletionResult, error) {
	response, err := a.provider.Complete(ctx, a.request)
	if err != nil {
		return reviewengine.CompletionResult{}, err
	}
	return reviewengine.CompletionResult{Message: reviewengine.CompletionMessage{Role: reviewengine.MessageRoleAssistant, Content: response.Content}, Usage: toInternalUsage(response.Usage), Model: response.Model}, nil
}

type streamingCompletionAdapter struct {
	completionAdapter
	streaming StreamingProvider
}

func (a streamingCompletionAdapter) StreamCompletion(ctx context.Context, _ reviewengine.CompletionRequest) (<-chan reviewengine.CompletionStreamEvent, error) {
	events, err := a.streaming.Stream(ctx, a.request)
	if err != nil {
		return nil, err
	}
	out := make(chan reviewengine.CompletionStreamEvent)
	go func() {
		defer close(out)
		for event := range events {
			mapped := reviewengine.CompletionStreamEvent{Delta: event.Delta, Usage: toInternalUsage(event.Usage)}
			switch event.Kind {
			case EventContentDelta:
				mapped.Kind = reviewengine.CompletionStreamContentDelta
			case EventUsage:
				mapped.Kind = reviewengine.CompletionStreamUsage
			case EventOperationStatus:
				mapped.Kind = reviewengine.CompletionStreamOperation
				if event.Operation != nil {
					mapped.Operation = reviewengine.CompletionOperation{ID: event.Operation.ID, Name: event.Operation.Name, Status: event.Operation.Status, Detail: event.Operation.Detail}
				}
			case EventCompletion:
				mapped.Kind = reviewengine.CompletionStreamComplete
				if event.Result != nil {
					mapped.Result.Model = event.Result.Model
				}
			case EventCancellation:
				mapped.Kind = reviewengine.CompletionStreamCanceled
				mapped.Err = context.Canceled
			case EventFailure:
				mapped.Kind = reviewengine.CompletionStreamFailed
				mapped.Err = errorFromEvent(event)
			default:
				mapped.Kind = reviewengine.CompletionStreamFailed
				mapped.Err = fmt.Errorf("provider emitted unknown event kind %q", event.Kind)
			}
			select {
			case out <- mapped:
			case <-ctx.Done():
				return
			}
			if mapped.Kind == reviewengine.CompletionStreamComplete || mapped.Kind == reviewengine.CompletionStreamCanceled || mapped.Kind == reviewengine.CompletionStreamFailed {
				return
			}
		}
	}()
	return out, nil
}

func errorFromEvent(event Event) error {
	if event.Error == nil {
		return errors.New("provider stream failed")
	}
	return &ProviderFailure{Code: event.Error.Code, Message: event.Error.Message, StatusCode: event.Error.StatusCode, Retryable: event.Error.Retryable}
}

func normalizeStream(ctx context.Context, provider, requestedModel string, in <-chan reviewengine.CompletionStreamEvent, out chan<- Event) {
	defer close(out)
	var content strings.Builder
	var usage Usage
	model := requestedModel
	for {
		select {
		case <-ctx.Done():
			emitTerminal(out, EventCancellation, StatusCanceled, content.String(), usage, model, mapError(provider, ctx.Err()))
			return
		case event, ok := <-in:
			if !ok {
				emitTerminal(out, EventCompletion, StatusCompleted, content.String(), usage, model, nil)
				return
			}
			switch event.Kind {
			case reviewengine.CompletionStreamContentDelta:
				content.WriteString(event.Delta)
				out <- Event{Kind: EventContentDelta, Delta: event.Delta}
			case reviewengine.CompletionStreamUsage:
				usage = fromInternalUsage(event.Usage)
				out <- Event{Kind: EventUsage, Usage: usage}
			case reviewengine.CompletionStreamOperation:
				op := event.Operation
				out <- Event{Kind: EventOperationStatus, Operation: &OperationStatus{ID: op.ID, Name: op.Name, Status: op.Status, Detail: op.Detail}}
			case reviewengine.CompletionStreamComplete:
				if event.Result.Model != "" {
					model = event.Result.Model
				}
				emitTerminal(out, EventCompletion, StatusCompleted, content.String(), usage, model, nil)
				return
			case reviewengine.CompletionStreamCanceled:
				err := event.Err
				if err == nil {
					err = context.Canceled
				}
				emitTerminal(out, EventCancellation, StatusCanceled, content.String(), usage, model, mapError(provider, err))
				return
			case reviewengine.CompletionStreamFailed:
				emitTerminal(out, EventFailure, StatusFailed, content.String(), usage, model, mapError(provider, event.Err))
				return
			}
		}
	}
}

func emitTerminal(out chan<- Event, kind EventKind, status TerminalStatus, content string, usage Usage, model string, failure *Error) {
	result := &Result{Status: status, Content: content, Usage: usage, Model: model}
	out <- Event{Kind: kind, Result: result, Error: failure}
}

func mapError(provider string, err error) *Error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: "canceled", Message: err.Error(), Provider: provider}
	}
	var providerErr *ProviderFailure
	if errors.As(err, &providerErr) {
		return &Error{Code: providerErr.Code, Message: providerErr.Message, Provider: provider, StatusCode: providerErr.StatusCode, Retryable: providerErr.Retryable}
	}
	var httpErr *reviewengine.LLMHTTPError
	if errors.As(err, &httpErr) {
		code := "provider_http_error"
		if httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden {
			code = "authentication_failed"
		} else if httpErr.StatusCode == http.StatusTooManyRequests {
			code = "rate_limited"
		}
		return &Error{Code: code, Message: httpErr.Error(), Provider: provider, StatusCode: httpErr.StatusCode, Retryable: reviewengine.IsTransient(err)}
	}
	return &Error{Code: "provider_error", Message: err.Error(), Provider: provider, Retryable: reviewengine.IsTransient(err)}
}

func toInternalUsage(usage Usage) reviewengine.CompletionUsage {
	return reviewengine.CompletionUsage{PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens}
}

func fromInternalUsage(usage reviewengine.CompletionUsage) Usage {
	return Usage{InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens}
}
