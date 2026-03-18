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
	Severity    string  `json:"severity"`
	Category    string  `json:"category"`
	File        string  `json:"file"`
	Line        int     `json:"line"`
	Evidence    string  `json:"evidence"`
	Explanation string  `json:"explanation"`
	Suggestion  string  `json:"suggestion"`
	Confidence  float64 `json:"confidence"`
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

func (r ReviewerOutput) Validate() error {
	if strings.TrimSpace(r.Summary) == "" {
		return errors.New("reviewer summary is required")
	}
	validSeverity := map[string]bool{"critical": true, "high": true, "medium": true, "low": true, "info": true}
	validCategory := map[string]bool{"security": true, "correctness": true, "architecture": true, "style": true, "test-coverage": true}

	for i, f := range r.Findings {
		if !validSeverity[f.Severity] {
			return fmt.Errorf("finding[%d] invalid severity %q", i, f.Severity)
		}
		if !validCategory[f.Category] {
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

