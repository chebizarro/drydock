package metareview

import (
	"encoding/json"
	"fmt"
	"strings"

	"drydock/internal/reviewengine"
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

type SecurityVerifyOutcome string

const (
	SecurityConfirmed SecurityVerifyOutcome = "confirmed"
	SecurityRefuted   SecurityVerifyOutcome = "refuted"
)

// SecurityFinding records a security-review candidate and the outcome of the
// adversarial verify stage. Category is normalized to "security" before the
// finding is included in meta-review analysis.
type SecurityFinding struct {
	Category      string                `json:"category"`
	CWE           string                `json:"cwe,omitempty"`
	Severity      string                `json:"severity,omitempty"`
	File          string                `json:"file,omitempty"`
	Line          int                   `json:"line,omitempty"`
	Evidence      string                `json:"evidence,omitempty"`
	Description   string                `json:"description,omitempty"`
	Confidence    float64               `json:"confidence,omitempty"`
	VerifyOutcome SecurityVerifyOutcome `json:"verify_outcome"`
	RefuteVotes   int                   `json:"refute_votes,omitempty"`
	VerifyVotes   int                   `json:"verify_votes,omitempty"`
}

// SecurityReviewAnalysis provides deterministic quality signals for the
// meta-review prompt and few-shot store.
type SecurityReviewAnalysis struct {
	Findings        []SecurityFinding `json:"findings"`
	TotalCandidates int               `json:"total_candidates"`
	Confirmed       int               `json:"confirmed"`
	Refuted         int               `json:"refuted"`
	RefuteRate      float64           `json:"refute_rate"`
	CWECounts       map[string]int    `json:"cwe_counts"`
	ConfirmedByCWE  map[string]int    `json:"confirmed_by_cwe"`
	RefutedByCWE    map[string]int    `json:"refuted_by_cwe"`
}

// AnalyzeSecurityFindings normalizes findings and derives verify-quality and
// CWE recurrence signals for meta-review.
func AnalyzeSecurityFindings(findings []SecurityFinding) SecurityReviewAnalysis {
	analysis := SecurityReviewAnalysis{
		Findings:        make([]SecurityFinding, 0, len(findings)),
		TotalCandidates: len(findings),
		CWECounts:       make(map[string]int),
		ConfirmedByCWE:  make(map[string]int),
		RefutedByCWE:    make(map[string]int),
	}
	for _, finding := range findings {
		finding.Category = "security"
		finding.CWE = strings.ToUpper(strings.TrimSpace(finding.CWE))
		analysis.Findings = append(analysis.Findings, finding)
		if finding.CWE != "" {
			analysis.CWECounts[finding.CWE]++
		}
		switch finding.VerifyOutcome {
		case SecurityConfirmed:
			analysis.Confirmed++
			if finding.CWE != "" {
				analysis.ConfirmedByCWE[finding.CWE]++
			}
		case SecurityRefuted:
			analysis.Refuted++
			if finding.CWE != "" {
				analysis.RefutedByCWE[finding.CWE]++
			}
		}
	}
	classified := analysis.Confirmed + analysis.Refuted
	if classified > 0 {
		analysis.RefuteRate = float64(analysis.Refuted) / float64(classified)
	}
	return analysis
}

// ConfirmedSecurityFindings adapts verified PR security findings into
// meta-review inputs without mutating the security-review result.
func ConfirmedSecurityFindings(findings []reviewengine.Finding) []SecurityFinding {
	out := make([]SecurityFinding, 0, len(findings))
	for _, finding := range findings {
		out = append(out, SecurityFinding{
			Category:      "security",
			CWE:           cweFromEvidence(finding.Evidence),
			Severity:      finding.Severity,
			File:          finding.File,
			Line:          finding.Line,
			Evidence:      finding.Evidence,
			Description:   finding.Explanation,
			Confidence:    finding.Confidence,
			VerifyOutcome: SecurityConfirmed,
		})
	}
	return out
}

func cweFromEvidence(evidence string) string {
	upper := strings.ToUpper(strings.TrimSpace(evidence))
	if !strings.HasPrefix(upper, "[CWE-") {
		return ""
	}
	end := strings.IndexByte(upper, ']')
	if end < 0 {
		return ""
	}
	return upper[1:end]
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
