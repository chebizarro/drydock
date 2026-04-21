package reviewengine

import "context"

// FakeLLMForTest is a test double for LLMClient that returns canned responses.
//
// Deprecated: Use testutil.FakeLLM instead. This type remains for backward
// compatibility with existing test files but will be removed in a future version.
// It should not be used in new test code.
type FakeLLMForTest struct {
	Responses []string
	Requests  []ChatRequest
}

func (f *FakeLLMForTest) ChatCompletion(_ context.Context, req ChatRequest) (string, error) {
	f.Requests = append(f.Requests, req)
	if len(f.Responses) == 0 {
		return "{}", nil
	}
	r := f.Responses[0]
	f.Responses = f.Responses[1:]
	return r, nil
}
