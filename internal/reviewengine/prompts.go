package reviewengine

import (
	"fmt"
	"strings"
)

// DefaultReviewerSystemPrompt returns the baseline reviewer system prompt.
// This is the single source of truth — promptrefine and engine both use it.
func DefaultReviewerSystemPrompt() string {
	return `You are a code review agent.
Return JSON ONLY with keys:
summary, findings, needs_more_context.
Each finding must include:
severity, category, file, line, evidence, explanation, suggestion, confidence.

Optional structured suggestion fields (include ONLY when the fix is concrete and localized):
- suggested_diff: a minimal unified diff hunk showing the exact change. Start with @@ header.
- suggested_code: the replacement code block that should replace the current code.
Include at most one of suggested_diff or suggested_code per finding.
Omit both if the fix requires broad refactoring or is ambiguous.

Example finding with suggested_diff:
{"severity":"medium","category":"correctness","file":"auth.go","line":42,"evidence":"err is not checked","explanation":"The error from Validate() is discarded, which could allow invalid tokens to pass.","suggestion":"Check the error return value.","suggested_diff":"@@ -42,1 +42,3 @@\n-\tValidate(token)\n+\tif err := Validate(token); err != nil {\n+\t\treturn fmt.Errorf(\"invalid token: %w\", err)\n+\t}","confidence":0.95}

If confidence < 0.6, add required items to needs_more_context instead of asserting uncertain findings.
If the context includes a change-impact section with high or critical blast radius, mention the downstream impact explicitly in the summary and consider compatibility and architecture implications.`
}

func plannerSystemPrompt() string {
	return `You are a code review planner.
Return JSON ONLY with keys:
change_type, risk_areas, needed_context, review_focus, model_route.
Allowed model_route: coder32b | llm70b | coder14b.
If the context includes a change-impact section with high or critical blast radius, factor that into review_focus and prefer a route that can reason about downstream compatibility.`
}

func plannerUserPrompt(contextBundle string, changedFiles []string) string {
	return fmt.Sprintf("Changed files:\n%s\n\nContext:\n%s", strings.Join(changedFiles, "\n"), contextBundle)
}

// reviewerSystemPrompt builds the reviewer system prompt. If baseOverride is
// non-empty it replaces the default preamble; additionalInstructions, checklist,
// security, and few-shot sections are always appended.
func reviewerSystemPrompt(baseOverride string, additionalInstructions string, checklist []string, securitySensitive bool, fewShot []string) string {
	var b strings.Builder
	if baseOverride != "" {
		b.WriteString(baseOverride)
	} else {
		b.WriteString(DefaultReviewerSystemPrompt())
	}

	if additionalInstructions != "" {
		b.WriteString("\n\nRepository-specific instructions:\n")
		b.WriteString(additionalInstructions)
	}

	if len(checklist) > 0 {
		b.WriteString("\n\nChecklist:\n- ")
		b.WriteString(strings.Join(checklist, "\n- "))
	}
	if securitySensitive {
		b.WriteString("\n\nBefore findings: trace data flow from entry to storage/output and identify trust boundaries crossed.")
	}
	if len(fewShot) > 0 {
		b.WriteString("\n\nFew-shot examples:\n")
		for i, ex := range fewShot {
			b.WriteString(fmt.Sprintf("Example %d:\n%s\n", i+1, ex))
		}
	}
	return b.String()
}

func walkthroughSystemPrompt() string {
	return `You are a code change summarizer.
Describe what this patch does in 2-4 clear, concise sentences aimed at a reviewer who has not seen the code yet.
Then list each changed file with a one-line summary of what changed in that file.

Return JSON ONLY with keys:
walkthrough, file_summaries.
file_summaries is an array of {file, summary}.

Example:
{"walkthrough":"This patch adds retry logic to the HTTP client and updates the configuration to support backoff settings. Tests are added for the retry behavior.","file_summaries":[{"file":"client.go","summary":"Added exponential backoff retry wrapper around HTTP requests"},{"file":"config.go","summary":"Added RetryMaxAttempts and RetryBaseDelay configuration fields"},{"file":"client_test.go","summary":"Added tests for retry on transient errors and immediate failure on non-transient errors"}]}`
}

func walkthroughUserPrompt(contextBundle string, changedFiles []string) string {
	return fmt.Sprintf("Changed files:\n%s\n\nPatch context:\n%s", strings.Join(changedFiles, "\n"), contextBundle)
}

func reviewerUserPrompt(contextBundle string, planner PlannerOutput) string {
	return fmt.Sprintf(
		"Review focus: %s\nRisk areas: %s\nNeeded context hints: %s\n\nContext bundle:\n%s",
		planner.ReviewFocus,
		strings.Join(planner.RiskAreas, ", "),
		strings.Join(planner.NeededContext, ", "),
		contextBundle,
	)
}
