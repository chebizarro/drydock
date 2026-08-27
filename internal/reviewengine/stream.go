package reviewengine

import "context"

// CompletionStreamEventKind identifies an incremental completion event.
type CompletionStreamEventKind string

const (
	// CompletionStreamContentDelta carries incremental assistant content.
	CompletionStreamContentDelta CompletionStreamEventKind = "content_delta"
	// CompletionStreamUsage carries cumulative token usage.
	CompletionStreamUsage CompletionStreamEventKind = "usage"
	// CompletionStreamOperation carries tool or operation progress.
	CompletionStreamOperation CompletionStreamEventKind = "operation"
	// CompletionStreamComplete terminates a successful stream.
	CompletionStreamComplete CompletionStreamEventKind = "completion"
	// CompletionStreamCanceled terminates a canceled stream.
	CompletionStreamCanceled CompletionStreamEventKind = "cancellation"
	// CompletionStreamFailed terminates a failed stream.
	CompletionStreamFailed CompletionStreamEventKind = "failure"
)

// CompletionOperation reports the state of a provider tool or operation.
type CompletionOperation struct {
	ID     string
	Name   string
	Status string
	Detail string
}

// CompletionStreamEvent is an incremental provider-neutral completion event.
type CompletionStreamEvent struct {
	Kind      CompletionStreamEventKind
	Delta     string
	Usage     CompletionUsage
	Operation CompletionOperation
	Result    CompletionResult
	Err       error
}

// StreamingCompletionClient is the optional incremental counterpart to
// CompletionClient. Implementations must close the returned channel.
type StreamingCompletionClient interface {
	StreamCompletion(context.Context, CompletionRequest) (<-chan CompletionStreamEvent, error)
}

// StreamCompletion uses a client's native stream when available and otherwise
// adapts its existing request/response completion into a short event stream.
func StreamCompletion(ctx context.Context, client CompletionClient, req CompletionRequest) (<-chan CompletionStreamEvent, error) {
	if streaming, ok := client.(StreamingCompletionClient); ok {
		return streaming.StreamCompletion(ctx, req)
	}

	events := make(chan CompletionStreamEvent, 3)
	go func() {
		defer close(events)
		result, err := client.Complete(ctx, req)
		if err != nil {
			kind := CompletionStreamFailed
			if ctx.Err() != nil {
				kind = CompletionStreamCanceled
			}
			events <- CompletionStreamEvent{Kind: kind, Err: err}
			return
		}
		if result.Message.Content != "" {
			events <- CompletionStreamEvent{Kind: CompletionStreamContentDelta, Delta: result.Message.Content}
		}
		events <- CompletionStreamEvent{Kind: CompletionStreamUsage, Usage: result.Usage}
		events <- CompletionStreamEvent{Kind: CompletionStreamComplete, Result: result}
	}()
	return events, nil
}
