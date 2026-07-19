package reviewengine

import (
	"context"
	"fmt"
	"strings"

	"drydock/internal/llmutil"
)

const maxStructuredRepairAttempts = 2

type structuredParser[T any] func(string) (T, error)

func (e *Engine) completeStructured(ctx context.Context, req ChatRequest, label string, parse structuredParser[PlannerOutput]) (PlannerOutput, error) {
	out, _, err := completeStructuredWithParser(ctx, e, req, label, parse)
	return out, err
}

// completeStructuredReviewer also returns the model identifier the endpoint
// reported serving for the successful completion, so review output can be
// labeled with the exact model that produced it.
func (e *Engine) completeStructuredReviewer(ctx context.Context, req ChatRequest, label string) (ReviewerOutput, string, error) {
	return completeStructuredWithParser(ctx, e, req, label, ParseReviewerOutput)
}

func (e *Engine) completeStructuredWalkthrough(ctx context.Context, req ChatRequest, label string) (WalkthroughOutput, error) {
	out, _, err := completeStructuredWithParser(ctx, e, req, label, ParseWalkthroughOutput)
	return out, err
}

func completeStructuredWithParser[T any](ctx context.Context, e *Engine, req ChatRequest, label string, parse structuredParser[T]) (T, string, error) {
	req.JSONMode = true
	res, err := e.client.ChatCompletion(ctx, req)
	if err != nil {
		var zero T
		return zero, "", fmt.Errorf("%s completion: %w", label, err)
	}

	out, parseErr := parseExtracted(res.Content, parse)
	if parseErr == nil {
		return out, res.Model, nil
	}

	originalErr := parseErr
	lastRaw := res.Content
	lastErr := parseErr
	for attempt := 1; attempt <= maxStructuredRepairAttempts; attempt++ {
		if e.logger != nil {
			e.logger.Warn("structured llm output invalid, requesting repair",
				"label", label,
				"attempt", attempt,
				"max_attempts", maxStructuredRepairAttempts,
				"error", lastErr,
			)
		}

		repairReq := req
		repairReq.Temperature = 0
		repairReq.System = jsonRepairSystemPrompt(label)
		repairReq.User = jsonRepairUserPrompt(label, lastRaw, lastErr)
		repairReq.JSONMode = true

		repaired, repairErr := e.client.ChatCompletion(ctx, repairReq)
		if repairErr != nil {
			var zero T
			return zero, "", fmt.Errorf("%s repair completion attempt %d: %w (original parse/validation error: %v)", label, attempt, repairErr, originalErr)
		}

		out, parseErr = parseExtracted(repaired.Content, parse)
		if parseErr == nil {
			return out, repaired.Model, nil
		}
		lastRaw = repaired.Content
		lastErr = parseErr
	}

	var zero T
	return zero, "", fmt.Errorf("%s output invalid after %d repair attempt(s): %w", label, maxStructuredRepairAttempts, lastErr)
}

func parseExtracted[T any](raw string, parse structuredParser[T]) (T, error) {
	return parse(llmutil.ExtractJSON(raw))
}

func jsonRepairSystemPrompt(label string) string {
	return "You repair malformed or schema-invalid JSON emitted by an LLM. Return JSON ONLY, with no markdown, prose, comments, or code fences. Preserve the intended content, but make it valid and conformant for the " + label + " schema."
}

func jsonRepairUserPrompt(label string, raw string, parseErr error) string {
	return fmt.Sprintf(`The previous %s response could not be parsed or validated.

Error:
%s

Required schema:
%s

Invalid response:
%s

Return only the corrected JSON object.`, label, parseErr, schemaHint(label), truncateForRepair(raw))
}

func schemaHint(label string) string {
	switch {
	case strings.Contains(label, "planner"):
		return `{"change_type":"string","risk_areas":["string"],"needed_context":["string"],"review_focus":"string","model_route":"coder32b|llm70b|coder14b"}`
	case strings.Contains(label, "walkthrough"):
		return `{"walkthrough":"string","file_summaries":[{"file":"string","summary":"string"}]}`
	default:
		return `{"summary":"string","findings":[{"severity":"critical|high|medium|low|info","category":"security|correctness|architecture|style|test-coverage","file":"string","line":1,"evidence":"string","explanation":"string","suggestion":"string","confidence":0.9}],"needs_more_context":["string"]}`
	}
}

func truncateForRepair(raw string) string {
	const max = 12000
	raw = strings.TrimSpace(raw)
	if len(raw) <= max {
		return raw
	}
	return raw[:max] + "\n... [truncated]"
}
