package metareview

import (
	"encoding/json"
	"fmt"
	"strings"
)

type MetaReviewOutput struct {
	MissedFindings     []MissedFinding `json:"missed_findings"`
	FalsePositives     []FalsePositive `json:"false_positives"`
	ReasoningQuality   float64         `json:"reasoning_quality"`
	ContextUtilization float64         `json:"context_utilization"`
	PromptGaps         []string        `json:"prompt_gaps"`
	SuggestedFewShot   bool            `json:"suggested_few_shot"`
}

type MissedFinding struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Evidence    string `json:"evidence"`
	WhyMissed   string `json:"why_missed"`
}

type FalsePositive struct {
	FindingIndex int    `json:"finding_index"`
	Reason       string `json:"reason"`
}

func ParseMetaReviewOutput(raw string) (MetaReviewOutput, error) {
	return parseMetaReviewOutput(raw, -1)
}

func ParseMetaReviewOutputForFindings(raw string, findingCount int) (MetaReviewOutput, error) {
	return parseMetaReviewOutput(raw, findingCount)
}

func parseMetaReviewOutput(raw string, findingCount int) (MetaReviewOutput, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return MetaReviewOutput{}, fmt.Errorf("parse meta review output: %w", err)
	}
	for _, key := range []string{"missed_findings", "false_positives", "reasoning_quality", "context_utilization", "prompt_gaps", "suggested_few_shot"} {
		if _, ok := fields[key]; !ok {
			return MetaReviewOutput{}, fmt.Errorf("%s is required", key)
		}
	}

	var out MetaReviewOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return MetaReviewOutput{}, fmt.Errorf("parse meta review output: %w", err)
	}
	if err := out.Validate(findingCount); err != nil {
		return MetaReviewOutput{}, err
	}
	return out, nil
}

func (out MetaReviewOutput) Validate(findingCount int) error {
	if out.MissedFindings == nil {
		return fmt.Errorf("missed_findings is required")
	}
	if out.FalsePositives == nil {
		return fmt.Errorf("false_positives is required")
	}
	if out.PromptGaps == nil {
		return fmt.Errorf("prompt_gaps is required")
	}
	if out.ReasoningQuality < 0 || out.ReasoningQuality > 1 {
		return fmt.Errorf("reasoning_quality must be in [0,1]")
	}
	if out.ContextUtilization < 0 || out.ContextUtilization > 1 {
		return fmt.Errorf("context_utilization must be in [0,1]")
	}
	for i, finding := range out.MissedFindings {
		if !allowedMissedFindingType(finding.Type) {
			return fmt.Errorf("missed_findings[%d].type is invalid: %q", i, finding.Type)
		}
		if strings.TrimSpace(finding.Description) == "" {
			return fmt.Errorf("missed_findings[%d].description is required", i)
		}
		if strings.TrimSpace(finding.Evidence) == "" {
			return fmt.Errorf("missed_findings[%d].evidence is required", i)
		}
		if !allowedWhyMissed(finding.WhyMissed) {
			return fmt.Errorf("missed_findings[%d].why_missed is invalid: %q", i, finding.WhyMissed)
		}
	}
	for i, fp := range out.FalsePositives {
		if fp.FindingIndex < 0 {
			return fmt.Errorf("false_positives[%d].finding_index must be non-negative", i)
		}
		if findingCount >= 0 && fp.FindingIndex >= findingCount {
			return fmt.Errorf("false_positives[%d].finding_index %d out of bounds for %d findings", i, fp.FindingIndex, findingCount)
		}
		if strings.TrimSpace(fp.Reason) == "" {
			return fmt.Errorf("false_positives[%d].reason is required", i)
		}
	}
	for i, gap := range out.PromptGaps {
		if strings.TrimSpace(gap) == "" {
			return fmt.Errorf("prompt_gaps[%d] must be non-empty", i)
		}
	}
	return nil
}

func allowedMissedFindingType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "correctness", "security", "performance", "reliability", "maintainability", "style", "testing", "documentation", "docs", "context", "other":
		return true
	default:
		return false
	}
}

func allowedWhyMissed(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "prompt_gap", "context_missing", "reviewer_error", "tool_error", "ambiguous_code", "other":
		return true
	default:
		return false
	}
}
