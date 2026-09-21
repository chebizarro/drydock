package reviewengine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/llmutil"
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
	out, model, _, err := CompleteStructured(ctx, e.client, e.logger, req, label, parse)
	return out, model, err
}

// CompleteStructured runs a JSON-mode completion and parses it with parse,
// re-prompting the model with a schema hint up to maxStructuredRepairAttempts
// times when the output is invalid. It is the shared structured-output path for
// callers that hold only an LLMClient rather than a full Engine, so no caller
// needs to hand-roll its own tolerant-JSON stack. It returns the parsed value,
// the model the successful completion reported, and the raw content that parsed
// (or, on failure, the last raw content received, for audit/debugging).
func CompleteStructured[T any](ctx context.Context, client LLMClient, logger *slog.Logger, req ChatRequest, label string, parse func(string) (T, error)) (T, string, string, error) {
	req.JSONMode = true
	res, err := client.ChatCompletion(ctx, req)
	if err != nil {
		var zero T
		return zero, "", "", fmt.Errorf("%s completion: %w", label, err)
	}

	out, parseErr := parseExtracted(res.Content, parse)
	if parseErr == nil {
		return out, res.Model, res.Content, nil
	}

	originalErr := parseErr
	lastRaw := res.Content
	lastErr := parseErr
	for attempt := 1; attempt <= maxStructuredRepairAttempts; attempt++ {
		if logger != nil {
			logger.Warn("structured llm output invalid, requesting repair",
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

		repaired, repairErr := client.ChatCompletion(ctx, repairReq)
		if repairErr != nil {
			var zero T
			return zero, "", lastRaw, fmt.Errorf("%s repair completion attempt %d: %w (original parse/validation error: %v)", label, attempt, repairErr, originalErr)
		}

		out, parseErr = parseExtracted(repaired.Content, parse)
		if parseErr == nil {
			return out, repaired.Model, repaired.Content, nil
		}
		lastRaw = repaired.Content
		lastErr = parseErr
	}

	var zero T
	return zero, "", lastRaw, fmt.Errorf("%s output invalid after %d repair attempt(s): %w", label, maxStructuredRepairAttempts, lastErr)
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
	case strings.Contains(label, "meta"):
		return `{"missed_findings":[{"type":"correctness|security|performance|reliability|maintainability|style|testing|documentation|context|other","description":"string","evidence":"string","why_missed":"prompt_gap|context_missing|reviewer_error|tool_error|ambiguous_code|other"}],"false_positives":[{"finding_index":0,"reason":"string"}],"reasoning_quality":0.9,"context_utilization":0.9,"prompt_gaps":["string"],"suggested_few_shot":true}`
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
