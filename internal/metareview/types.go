package metareview

import (
	"encoding/json"
	"fmt"
)

type MetaReviewOutput struct {
	MissedFindings []MissedFinding `json:"missed_findings"`
	FalsePositives []FalsePositive `json:"false_positives"`
	ReasoningQuality float64       `json:"reasoning_quality"`
	ContextUtilization float64     `json:"context_utilization"`
	PromptGaps []string            `json:"prompt_gaps"`
	SuggestedFewShot bool          `json:"suggested_few_shot"`
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
	var out MetaReviewOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return MetaReviewOutput{}, fmt.Errorf("parse meta review output: %w", err)
	}
	if out.ReasoningQuality < 0 || out.ReasoningQuality > 1 {
		return MetaReviewOutput{}, fmt.Errorf("reasoning_quality must be in [0,1]")
	}
	if out.ContextUtilization < 0 || out.ContextUtilization > 1 {
		return MetaReviewOutput{}, fmt.Errorf("context_utilization must be in [0,1]")
	}
	return out, nil
}

