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
}

func (f *FakeLLM) ChatCompletion(_ context.Context, req reviewengine.ChatRequest) (string, error) {
	f.Requests = append(f.Requests, req)
	if len(f.Responses) == 0 {
		return "{}", nil
	}
	r := f.Responses[0]
	f.Responses = f.Responses[1:]
	return r, nil
}
