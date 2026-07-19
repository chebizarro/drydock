// Package testutil provides test doubles shared across drydock packages.
// This package must ONLY be imported by test files (*_test.go).
package testutil

import (
	"context"

	"drydock/internal/reviewengine"
)

// FakeLLM is a test double for reviewengine.LLMClient that returns canned responses.
type FakeLLM struct {
	Responses []string
	Requests  []reviewengine.ChatRequest
	// ServedModel, when set, is reported as the model that served every
	// completion (mirrors the `model` field of real provider responses).
	ServedModel string
}

func (f *FakeLLM) ChatCompletion(_ context.Context, req reviewengine.ChatRequest) (reviewengine.ChatResult, error) {
	f.Requests = append(f.Requests, req)
	if len(f.Responses) == 0 {
		return reviewengine.ChatResult{Content: "{}", Model: f.ServedModel}, nil
	}
	r := f.Responses[0]
	f.Responses = f.Responses[1:]
	return reviewengine.ChatResult{Content: r, Model: f.ServedModel}, nil
}
