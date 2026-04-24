package reviewengine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type ModelRoute string

const (
	RouteCoder32B ModelRoute = "coder32b"
	RouteLLM70B   ModelRoute = "llm70b"
	RouteCoder14B ModelRoute = "coder14b"
)

type PlannerOutput struct {
	ChangeType    string     `json:"change_type"`
	RiskAreas     []string   `json:"risk_areas"`
	NeededContext []string   `json:"needed_context"`
	ReviewFocus   string     `json:"review_focus"`
	ModelRoute    ModelRoute `json:"model_route"`
}

type ReviewerOutput struct {
	Summary          string    `json:"summary"`
	Findings         []Finding `json:"findings"`
	NeedsMoreContext []string  `json:"needs_more_context"`
}

type Finding struct {
	Severity      string  `json:"severity"`
	Category      string  `json:"category"`
	File          string  `json:"file"`
	Line          int     `json:"line"`
	Evidence      string  `json:"evidence"`
	Explanation   string  `json:"explanation"`
	Suggestion    string  `json:"suggestion"`
	SuggestedDiff string  `json:"suggested_diff,omitempty"`
	SuggestedCode string  `json:"suggested_code,omitempty"`
	Confidence    float64 `json:"confidence"`
}

func ParsePlannerOutput(raw string) (PlannerOutput, error) {
	var out PlannerOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return PlannerOutput{}, fmt.Errorf("parse planner json: %w", err)
	}
	if err := out.Validate(); err != nil {
		return PlannerOutput{}, err
	}
	return out, nil
}

func ParseReviewerOutput(raw string) (ReviewerOutput, error) {
	var out ReviewerOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return ReviewerOutput{}, fmt.Errorf("parse reviewer json: %w", err)
	}
	// Sanitize optional suggestion fields (non-fatal).
	for i := range out.Findings {
		out.Findings[i].SuggestedDiff = sanitizeSuggestedDiff(out.Findings[i].SuggestedDiff)
		out.Findings[i].SuggestedCode = strings.TrimSpace(out.Findings[i].SuggestedCode)
	}
	if err := out.Validate(); err != nil {
		return ReviewerOutput{}, err
	}
	return out, nil
}

func (p PlannerOutput) Validate() error {
	switch p.ModelRoute {
	case RouteCoder32B, RouteLLM70B, RouteCoder14B:
	default:
		return fmt.Errorf("invalid planner model_route: %q", p.ModelRoute)
	}
	if strings.TrimSpace(p.ChangeType) == "" {
		return errors.New("planner change_type is required")
	}
	return nil
}

// IsValidSeverity returns true if severity is a recognized level.
func IsValidSeverity(severity string) bool {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "high", "medium", "low", "info":
		return true
	}
	return false
}

// IsValidCategory returns true if category is a recognized review category.
func IsValidCategory(category string) bool {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "security", "correctness", "architecture", "style", "test-coverage":
		return true
	}
	return false
}

// IsAtOrAboveSeverity returns true if severity is at or above the threshold.
func IsAtOrAboveSeverity(severity, threshold string) bool {
	order := map[string]int{
		"info": 1, "low": 2, "medium": 3, "high": 4, "critical": 5,
	}
	return order[strings.ToLower(severity)] >= order[strings.ToLower(threshold)]
}

// sanitizeSuggestedDiff clears the suggested diff if it doesn't look like a
// valid unified diff hunk. This is a permissive heuristic — we accept partial
// diffs rather than rejecting the whole review.
func sanitizeSuggestedDiff(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Accept if it starts with a diff header or hunk header.
	if strings.HasPrefix(s, "@@") || strings.HasPrefix(s, "diff --git") {
		return s
	}
	// Accept if it contains at least one +/- hunk line (not +++/--- headers).
	for _, line := range strings.Split(s, "\n") {
		if len(line) == 0 {
			continue
		}
		if line[0] == '+' && !strings.HasPrefix(line, "+++") {
			return s
		}
		if line[0] == '-' && !strings.HasPrefix(line, "---") {
			return s
		}
	}
	// Doesn't look like a diff — clear it.
	return ""
}

// HasSuggestion returns true if the finding has an actionable code suggestion.
func (f Finding) HasSuggestion() bool {
	return f.SuggestedDiff != "" || f.SuggestedCode != ""
}

func (r ReviewerOutput) Validate() error {
	if strings.TrimSpace(r.Summary) == "" {
		return errors.New("reviewer summary is required")
	}

	for i, f := range r.Findings {
		if !IsValidSeverity(f.Severity) {
			return fmt.Errorf("finding[%d] invalid severity %q", i, f.Severity)
		}
		if !IsValidCategory(f.Category) {
			return fmt.Errorf("finding[%d] invalid category %q", i, f.Category)
		}
		if strings.TrimSpace(f.File) == "" {
			return fmt.Errorf("finding[%d] file is required", i)
		}
		if f.Line <= 0 {
			return fmt.Errorf("finding[%d] line must be > 0", i)
		}
		if f.Confidence < 0 || f.Confidence > 1 {
			return fmt.Errorf("finding[%d] confidence must be in [0,1]", i)
		}
		if f.Confidence < 0.6 {
			if len(r.NeedsMoreContext) == 0 {
				return fmt.Errorf("finding[%d] confidence<0.6 requires needs_more_context", i)
			}
		}
	}
	return nil
}

