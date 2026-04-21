package reviewengine

import "context"

// FakeLLMForTest is a test double for LLMClient that returns canned responses.
// Exported so other packages can use it in their tests.
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
