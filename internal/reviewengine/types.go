package reviewengine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type ModelRoute string

const (
	RouteCoder32B    ModelRoute = "coder32b"
	RouteLLM70B      ModelRoute = "llm70b"
	RouteCoder14B    ModelRoute = "coder14b"
	RouteSec70B      ModelRoute = "sec70b"
	RouteSecClassify ModelRoute = "secclassify"
	RouteSecLocalize ModelRoute = "seclocalize"
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

// FileSummary describes one changed file in a walkthrough.
type FileSummary struct {
	File    string `json:"file"`
	Summary string `json:"summary"`
}

// WalkthroughOutput is the structured walkthrough of a patch.
type WalkthroughOutput struct {
	Walkthrough   string        `json:"walkthrough"`
	FileSummaries []FileSummary `json:"file_summaries"`
}

type StepState string

const (
	StepStateSucceeded StepState = "succeeded"
	StepStateSkipped   StepState = "skipped"
	StepStateFailed    StepState = "failed"
)

type StepStatus struct {
	State StepState `json:"state"`
	Error string    `json:"error,omitempty"`
}

type ModelFailure struct {
	Route ModelRoute `json:"route"`
	Error string     `json:"error"`
}

type EnsembleReviewerTrace struct {
	Route ModelRoute    `json:"route"`
	Trace ReviewerTrace `json:"trace"`
}

type EnsembleStatus struct {
	RequiredReviewers  int                     `json:"required_reviewers"`
	SucceededReviewers []ModelRoute            `json:"succeeded_reviewers"`
	FailedReviewers    []ModelFailure          `json:"failed_reviewers,omitempty"`
	ReviewerTraces     []EnsembleReviewerTrace `json:"reviewer_traces,omitempty"`
	Degraded           bool                    `json:"degraded"`
}

// Priority is the canonical review urgency used by agentic review flows.
type Priority string

const (
	PriorityP0 Priority = "P0"
	PriorityP1 Priority = "P1"
	PriorityP2 Priority = "P2"
)

type Finding struct {
	// Priority is canonical. Severity remains on the wire for compatibility
	// with existing scanners, publishers, and stored review artifacts.
	Priority      Priority `json:"priority,omitempty"`
	Severity      string   `json:"severity"`
	Category      string   `json:"category"`
	File          string   `json:"file"`
	Line          int      `json:"line"`
	Evidence      string   `json:"evidence"`
	Explanation   string   `json:"explanation"`
	Suggestion    string   `json:"suggestion"`
	SuggestedDiff string   `json:"suggested_diff,omitempty"`
	SuggestedCode string   `json:"suggested_code,omitempty"`
	Sensitive     bool     `json:"sensitive,omitempty"`
	Confidence    float64  `json:"confidence"`
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

// ParseWalkthroughOutput parses the walkthrough LLM response.
// Lenient: if parsing fails, returns a zero-value output with an error.
// The caller should treat walkthrough failures as non-fatal.
func ParseWalkthroughOutput(raw string) (WalkthroughOutput, error) {
	var out WalkthroughOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return WalkthroughOutput{}, fmt.Errorf("parse walkthrough json: %w", err)
	}
	out.Walkthrough = strings.TrimSpace(out.Walkthrough)
	for i := range out.FileSummaries {
		out.FileSummaries[i].File = strings.TrimSpace(out.FileSummaries[i].File)
		out.FileSummaries[i].Summary = strings.TrimSpace(out.FileSummaries[i].Summary)
	}
	return out, nil
}

func ParseReviewerOutput(raw string) (ReviewerOutput, error) {
	var out ReviewerOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return ReviewerOutput{}, fmt.Errorf("parse reviewer json: %w", err)
	}
	// Sanitize optional suggestion fields and establish the canonical priority
	// while retaining the legacy severity field.
	for i := range out.Findings {
		normalized, err := NormalizeFindingPriority(out.Findings[i])
		if err != nil {
			return ReviewerOutput{}, fmt.Errorf("finding[%d] %w", i, err)
		}
		normalized.SuggestedDiff = sanitizeSuggestedDiff(normalized.SuggestedDiff)
		normalized.SuggestedCode = strings.TrimSpace(normalized.SuggestedCode)
		out.Findings[i] = normalized
	}
	if err := out.Validate(); err != nil {
		return ReviewerOutput{}, err
	}
	return out, nil
}

func (p PlannerOutput) Validate() error {
	switch p.ModelRoute {
	case RouteCoder32B, RouteLLM70B, RouteCoder14B, RouteSec70B, RouteSecClassify, RouteSecLocalize:
	default:
		return fmt.Errorf("invalid planner model_route: %q", p.ModelRoute)
	}
	if strings.TrimSpace(p.ChangeType) == "" {
		return errors.New("planner change_type is required")
	}
	return nil
}

// NormalizePriority returns the canonical spelling of a priority.
func NormalizePriority(priority string) (Priority, bool) {
	switch strings.ToUpper(strings.TrimSpace(priority)) {
	case string(PriorityP0):
		return PriorityP0, true
	case string(PriorityP1):
		return PriorityP1, true
	case string(PriorityP2):
		return PriorityP2, true
	default:
		return "", false
	}
}

// PriorityFromSeverity maps every legacy severity to the canonical three-level
// priority contract. Low and informational findings remain valid legacy inputs,
// but are grouped under P2.
func PriorityFromSeverity(severity string) (Priority, bool) {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical":
		return PriorityP0, true
	case "high":
		return PriorityP1, true
	case "medium", "low", "info":
		return PriorityP2, true
	default:
		if priority, ok := NormalizePriority(severity); ok {
			return priority, true
		}
		return "", false
	}
}

// SeverityFromPriority returns the canonical legacy severity for a priority.
func SeverityFromPriority(priority Priority) (string, bool) {
	normalized, ok := NormalizePriority(string(priority))
	if !ok {
		return "", false
	}
	switch normalized {
	case PriorityP0:
		return "critical", true
	case PriorityP1:
		return "high", true
	case PriorityP2:
		return "medium", true
	default:
		return "", false
	}
}

// NormalizeFindingPriority fills the compatibility field that is absent and
// rejects contradictory priority/severity pairs. Existing low/info severities
// are preserved while receiving canonical P2.
func NormalizeFindingPriority(f Finding) (Finding, error) {
	priority, hasPriority := NormalizePriority(string(f.Priority))
	severity := strings.ToLower(strings.TrimSpace(f.Severity))
	if !hasPriority && severity == "" {
		return Finding{}, errors.New("finding priority or severity is required")
	}
	if !hasPriority {
		var ok bool
		priority, ok = PriorityFromSeverity(severity)
		if !ok {
			return Finding{}, fmt.Errorf("invalid severity %q", f.Severity)
		}
	}
	if severity == "" {
		var ok bool
		severity, ok = SeverityFromPriority(priority)
		if !ok {
			return Finding{}, fmt.Errorf("invalid priority %q", f.Priority)
		}
	} else {
		severityPriority, ok := PriorityFromSeverity(severity)
		if !ok {
			return Finding{}, fmt.Errorf("invalid severity %q", f.Severity)
		}
		if severityPriority != priority {
			return Finding{}, fmt.Errorf("priority %s conflicts with severity %s", priority, severity)
		}
	}
	f.Priority = priority
	f.Severity = severity
	return f, nil
}

// NormalizeFindings returns a copy with canonical priorities populated.
func NormalizeFindings(findings []Finding) ([]Finding, error) {
	normalized := make([]Finding, len(findings))
	for i, finding := range findings {
		var err error
		normalized[i], err = NormalizeFindingPriority(finding)
		if err != nil {
			return nil, fmt.Errorf("finding[%d] %w", i, err)
		}
	}
	return normalized, nil
}

// FindingPriorityRank returns a total rank for canonical priorities and all
// recognized legacy severities. Invalid or missing values rank zero.
func FindingPriorityRank(f Finding) int {
	priority, ok := NormalizePriority(string(f.Priority))
	if !ok {
		priority, ok = PriorityFromSeverity(f.Severity)
	}
	if !ok {
		return 0
	}
	switch priority {
	case PriorityP0:
		return 3
	case PriorityP1:
		return 2
	case PriorityP2:
		return 1
	default:
		return 0
	}
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

// FindingLegacySeverityRank returns the compatibility rank, deriving the
// canonical legacy severity when only Priority is populated.
func FindingLegacySeverityRank(f Finding) int {
	if rank := LegacySeverityRank(f.Severity); rank > 0 {
		return rank
	}
	if severity, ok := SeverityFromPriority(f.Priority); ok {
		return LegacySeverityRank(severity)
	}
	return 0
}

// LegacySeverityRank returns the five-level compatibility rank, or zero for
// an invalid severity.
func LegacySeverityRank(severity string) int {
	order := map[string]int{
		"info": 1, "low": 2, "medium": 3, "high": 4, "critical": 5,
	}
	return order[strings.ToLower(strings.TrimSpace(severity))]
}

// IsAtOrAboveSeverity returns true if severity is at or above the threshold.
func IsAtOrAboveSeverity(severity, threshold string) bool {
	severityRank := LegacySeverityRank(severity)
	thresholdRank := LegacySeverityRank(threshold)
	return severityRank > 0 && thresholdRank > 0 && severityRank >= thresholdRank
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

	for i := range r.Findings {
		f, err := NormalizeFindingPriority(r.Findings[i])
		if err != nil {
			return fmt.Errorf("finding[%d] %w", i, err)
		}
		r.Findings[i] = f
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
