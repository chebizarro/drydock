package review

import (
	"context"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

type fakeProvider struct {
	complete func(context.Context, ProviderRequest) (ProviderResponse, error)
	stream   func(context.Context, ProviderRequest) (<-chan Event, error)
}

func (f fakeProvider) Complete(ctx context.Context, req ProviderRequest) (ProviderResponse, error) {
	if f.complete != nil {
		return f.complete(ctx, req)
	}
	return ProviderResponse{}, nil
}

func (f fakeProvider) Stream(ctx context.Context, req ProviderRequest) (<-chan Event, error) {
	return f.stream(ctx, req)
}

type synchronousProvider struct {
	complete func(context.Context, ProviderRequest) (ProviderResponse, error)
}

func (p synchronousProvider) Complete(ctx context.Context, req ProviderRequest) (ProviderResponse, error) {
	return p.complete(ctx, req)
}

func TestStreamCompletionHappyPath(t *testing.T) {
	provider := fakeProvider{stream: func(_ context.Context, req ProviderRequest) (<-chan Event, error) {
		if req.Model != "model-a" {
			t.Fatalf("provider model = %q", req.Model)
		}
		if len(req.Messages) != 2 || req.Messages[0].Role != RoleSystem || req.Messages[1].Content != "review this" {
			t.Fatalf("provider messages = %#v", req.Messages)
		}
		events := make(chan Event, 5)
		events <- Event{Kind: EventContentDelta, Delta: "hello "}
		events <- Event{Kind: EventOperationStatus, Operation: &OperationStatus{ID: "op-1", Name: "search", Status: "completed"}}
		events <- Event{Kind: EventContentDelta, Delta: "world"}
		events <- Event{Kind: EventUsage, Usage: Usage{InputTokens: 12, OutputTokens: 2, TotalTokens: 14}}
		events <- Event{Kind: EventCompletion, Result: &Result{Model: "served-model"}}
		close(events)
		return events, nil
	}}
	engine, err := New(map[string]Provider{"stub": provider})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := engine.Stream(context.Background(), Request{
		Model:    ModelSelection{Provider: "stub", Model: "model-a"},
		Messages: []Message{{Role: RoleUser, Content: "review this"}},
		Context:  []ContextItem{{Name: "diff", Content: "+new line"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(stream)
	if len(events) != 5 {
		t.Fatalf("event count = %d, want 5: %#v", len(events), events)
	}
	terminal := events[len(events)-1]
	if terminal.Kind != EventCompletion || terminal.Result == nil {
		t.Fatalf("terminal = %#v", terminal)
	}
	if terminal.Result.Status != StatusCompleted || terminal.Result.Content != "hello world" || terminal.Result.Usage.TotalTokens != 14 || terminal.Result.Model != "served-model" {
		t.Fatalf("result = %#v", terminal.Result)
	}
}

func TestStreamFailurePreservesPartialContentAndUsage(t *testing.T) {
	provider := fakeProvider{stream: func(context.Context, ProviderRequest) (<-chan Event, error) {
		events := make(chan Event, 3)
		events <- Event{Kind: EventContentDelta, Delta: "partial"}
		events <- Event{Kind: EventUsage, Usage: Usage{InputTokens: 8, OutputTokens: 1, TotalTokens: 9}}
		events <- Event{Kind: EventFailure, Error: &Error{Code: "overloaded", Message: "try later", StatusCode: 503, Retryable: true}}
		close(events)
		return events, nil
	}}
	engine, _ := New(map[string]Provider{"stub": provider})
	stream, err := engine.Stream(context.Background(), Request{Model: ModelSelection{Provider: "stub", Model: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(stream)
	terminal := events[len(events)-1]
	if terminal.Kind != EventFailure || terminal.Result == nil || terminal.Error == nil {
		t.Fatalf("terminal = %#v", terminal)
	}
	if terminal.Result.Status != StatusFailed || terminal.Result.Content != "partial" || terminal.Result.Usage.TotalTokens != 9 {
		t.Fatalf("partial result = %#v", terminal.Result)
	}
	if terminal.Error.Code != "overloaded" || terminal.Error.StatusCode != 503 || !terminal.Error.Retryable {
		t.Fatalf("mapped error = %#v", terminal.Error)
	}
}

func TestStreamCancellationPreservesPartials(t *testing.T) {
	provider := fakeProvider{stream: func(ctx context.Context, _ ProviderRequest) (<-chan Event, error) {
		events := make(chan Event)
		go func() {
			defer close(events)
			events <- Event{Kind: EventContentDelta, Delta: "started"}
			events <- Event{Kind: EventUsage, Usage: Usage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5}}
			<-ctx.Done()
		}()
		return events, nil
	}}
	engine, _ := New(map[string]Provider{"stub": provider})
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := engine.Stream(ctx, Request{Model: ModelSelection{Provider: "stub", Model: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	first := <-stream
	second := <-stream
	if first.Kind != EventContentDelta || second.Kind != EventUsage {
		t.Fatalf("pre-cancel events = %#v, %#v", first, second)
	}
	cancel()
	terminal := <-stream
	if terminal.Kind != EventCancellation || terminal.Result == nil || terminal.Error == nil {
		t.Fatalf("terminal = %#v", terminal)
	}
	if terminal.Result.Status != StatusCanceled || terminal.Result.Content != "started" || terminal.Result.Usage.TotalTokens != 5 || terminal.Error.Code != "canceled" {
		t.Fatalf("canceled result = %#v, error = %#v", terminal.Result, terminal.Error)
	}
	if _, ok := <-stream; ok {
		t.Fatal("stream did not close after cancellation")
	}
}

func TestProviderHTTPErrorMapping(t *testing.T) {
	provider := synchronousProvider{complete: func(context.Context, ProviderRequest) (ProviderResponse, error) {
		return ProviderResponse{}, &reviewengine.LLMHTTPError{StatusCode: 429, Status: "429 Too Many Requests", Body: "rate limited"}
	}}
	engine, _ := New(map[string]Provider{"stub": provider})
	stream, err := engine.Stream(context.Background(), Request{Model: ModelSelection{Provider: "stub", Model: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(stream)
	terminal := events[len(events)-1]
	if terminal.Kind != EventFailure || terminal.Error == nil {
		t.Fatalf("terminal = %#v", terminal)
	}
	if terminal.Error.Code != "rate_limited" || terminal.Error.StatusCode != 429 || !terminal.Error.Retryable || terminal.Error.Provider != "stub" {
		t.Fatalf("mapped error = %#v", terminal.Error)
	}
}

func collect(stream <-chan Event) []Event {
	var events []Event
	for event := range stream {
		events = append(events, event)
	}
	return events
}
