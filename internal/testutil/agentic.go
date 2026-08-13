// Package testutil provides test doubles shared across drydock packages.
package testutil

import (
	"context"
	"fmt"
	"sync"

	"drydock/internal/reviewengine"
)

// CompletionStep scripts one Complete call. Model is matched exactly when set;
// an empty Model is a wildcard for discovery or single-model tests.
type CompletionStep struct {
	Model         string
	Result        reviewengine.CompletionResult
	Err           error
	Inspect       func(reviewengine.CompletionRequest) error
	WaitForCancel bool
	Started       chan<- struct{}
}

// ScriptedAgenticClient is a concurrency-safe client for agentic integration
// tests. It supports the legacy structured planner and the Complete tool loop.
type ScriptedAgenticClient struct {
	mu sync.Mutex

	ChatResults []reviewengine.ChatResult
	Steps       []CompletionStep

	ChatRequests       []reviewengine.ChatRequest
	CompletionRequests []reviewengine.CompletionRequest
}

func (c *ScriptedAgenticClient) ChatCompletion(_ context.Context, request reviewengine.ChatRequest) (reviewengine.ChatResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ChatRequests = append(c.ChatRequests, request)
	if len(c.ChatResults) == 0 {
		return reviewengine.ChatResult{}, fmt.Errorf("scripted agentic client: unexpected ChatCompletion for model %q", request.Model)
	}
	result := c.ChatResults[0]
	c.ChatResults = c.ChatResults[1:]
	return result, nil
}

func (c *ScriptedAgenticClient) Complete(ctx context.Context, request reviewengine.CompletionRequest) (reviewengine.CompletionResult, error) {
	c.mu.Lock()
	c.CompletionRequests = append(c.CompletionRequests, request)
	index := -1
	for i, step := range c.Steps {
		if step.Model == "" || step.Model == request.Model {
			index = i
			break
		}
	}
	if index < 0 {
		c.mu.Unlock()
		return reviewengine.CompletionResult{}, fmt.Errorf("scripted agentic client: unexpected Complete for model %q", request.Model)
	}
	step := c.Steps[index]
	c.Steps = append(c.Steps[:index], c.Steps[index+1:]...)
	c.mu.Unlock()

	if step.Inspect != nil {
		if err := step.Inspect(request); err != nil {
			return reviewengine.CompletionResult{}, err
		}
	}
	if step.Started != nil {
		select {
		case step.Started <- struct{}{}:
		default:
		}
	}
	if step.WaitForCancel {
		<-ctx.Done()
		return reviewengine.CompletionResult{}, ctx.Err()
	}
	return step.Result, step.Err
}

func (c *ScriptedAgenticClient) CompletionCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.CompletionRequests)
}

func (c *ScriptedAgenticClient) RequestsForModel(model string) []reviewengine.CompletionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	var requests []reviewengine.CompletionRequest
	for _, request := range c.CompletionRequests {
		if request.Model == model {
			requests = append(requests, request)
		}
	}
	return requests
}

func (c *ScriptedAgenticClient) RemainingSteps() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.Steps)
}

// AgenticFixturePatch returns the canonical patch shared by the pipeline, IDE,
// and security-audit entry-point tests.
func AgenticFixturePatch() string {
	return "diff --git a/main.go b/main.go\n" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1 +1,2 @@\n" +
		" package main\n" +
		"+func x() int { return 0 }\n"
}

func AgenticFixtureBaseFiles() map[string]string {
	return map[string]string{
		"main.go":    "package main\n",
		"context.go": "package main\nfunc contextOnly() {}\n",
	}
}

func AgenticFixtureFinalFiles() map[string]string {
	return map[string]string{
		"main.go":    "package main\nfunc x() int { return 0 }\n",
		"context.go": "package main\nfunc contextOnly() {}\n",
	}
}
