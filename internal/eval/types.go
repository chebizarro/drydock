package eval

import (
	"context"

	"drydock/internal/reviewengine"
)

type Dataset struct {
	ID    string    `json:"id"`
	Cases []PatchCase `json:"cases"`
}

type PatchCase struct {
	CaseID            string             `json:"case_id"`
	PatchDiff         string             `json:"patch_diff"`
	ChangedFiles      []string           `json:"changed_files"`
	ContextBundle     string             `json:"context_bundle"`
	ExpectedFindings  []ExpectedFinding  `json:"expected_findings"`
}

type ExpectedFinding struct {
	Category string `json:"category"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
}

type ReviewRunner interface {
	ReviewCase(ctx context.Context, in RunCaseInput) (reviewengine.ReviewerOutput, error)
}

type RunCaseInput struct {
	PatchDiff     string
	ChangedFiles  []string
	ContextBundle string
}

type Metrics struct {
	TotalCases            int     `json:"total_cases"`
	ExpectedFindings      int     `json:"expected_findings"`
	PredictedFindings     int     `json:"predicted_findings"`
	TruePositives         int     `json:"true_positives"`
	FalsePositives        int     `json:"false_positives"`
	FalseNegatives        int     `json:"false_negatives"`
	Recall                float64 `json:"recall"`
	FalsePositiveRate     float64 `json:"false_positive_rate"`
	CalibrationMAE        float64 `json:"calibration_mae"`
	HighConfidencePrecision float64 `json:"high_conf_precision"`
}

