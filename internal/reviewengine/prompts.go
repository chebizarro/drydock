package reviewengine

import (
	"fmt"
	"strings"
)

func plannerSystemPrompt() string {
	return `You are a code review planner.
Return JSON ONLY with keys:
change_type, risk_areas, needed_context, review_focus, model_route.
Allowed model_route: coder32b | llm70b | coder14b.`
}

func plannerUserPrompt(contextBundle string, changedFiles []string) string {
	return fmt.Sprintf("Changed files:\n%s\n\nContext:\n%s", strings.Join(changedFiles, "\n"), contextBundle)
}

func reviewerSystemPrompt(checklist []string, securitySensitive bool, fewShot []string) string {
	var b strings.Builder
	b.WriteString(`You are a code review agent.
Return JSON ONLY with keys:
summary, findings, needs_more_context.
Each finding must include:
severity, category, file, line, evidence, explanation, suggestion, confidence.
If confidence < 0.6, add required items to needs_more_context instead of asserting uncertain findings.`)

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

func reviewerUserPrompt(contextBundle string, planner PlannerOutput) string {
	return fmt.Sprintf(
		"Review focus: %s\nRisk areas: %s\nNeeded context hints: %s\n\nContext bundle:\n%s",
		planner.ReviewFocus,
		strings.Join(planner.RiskAreas, ", "),
		strings.Join(planner.NeededContext, ", "),
		contextBundle,
	)
}

