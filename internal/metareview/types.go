package metareview

import (
	"encoding/json"
	"fmt"
	"strconv"
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
	candidate := extractMetaReviewJSON(raw)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(candidate), &fields); err != nil {
		repaired := repairInvalidJSONEscapes(candidate)
		if repairErr := json.Unmarshal([]byte(repaired), &fields); repairErr != nil {
			return MetaReviewOutput{}, fmt.Errorf("parse meta review output: %w", err)
		}
	}
	if fields == nil {
		return MetaReviewOutput{}, fmt.Errorf("parse meta review output: expected a JSON object")
	}

	recognized := 0
	for _, key := range []string{"missed_findings", "false_positives", "reasoning_quality", "context_utilization", "prompt_gaps", "suggested_few_shot"} {
		if _, ok := fields[key]; ok {
			recognized++
		}
	}
	if recognized == 0 {
		return MetaReviewOutput{}, fmt.Errorf("parse meta review output: no recognized fields")
	}

	out := MetaReviewOutput{
		MissedFindings: make([]MissedFinding, 0),
		FalsePositives: make([]FalsePositive, 0),
		PromptGaps:     make([]string, 0),
	}
	var err error
	if rawValue, ok := fields["missed_findings"]; ok {
		out.MissedFindings, err = parseMissedFindings(rawValue)
		if err != nil {
			return MetaReviewOutput{}, fmt.Errorf("missed_findings: %w", err)
		}
	}
	if rawValue, ok := fields["false_positives"]; ok {
		out.FalsePositives, err = parseFalsePositives(rawValue)
		if err != nil {
			return MetaReviewOutput{}, fmt.Errorf("false_positives: %w", err)
		}
	}
	if rawValue, ok := fields["reasoning_quality"]; ok {
		out.ReasoningQuality, err = parseFlexibleFloat(rawValue)
		if err != nil {
			return MetaReviewOutput{}, fmt.Errorf("reasoning_quality: %w", err)
		}
	}
	if rawValue, ok := fields["context_utilization"]; ok {
		out.ContextUtilization, err = parseFlexibleFloat(rawValue)
		if err != nil {
			return MetaReviewOutput{}, fmt.Errorf("context_utilization: %w", err)
		}
	}
	if rawValue, ok := fields["prompt_gaps"]; ok {
		out.PromptGaps, err = parseFlexibleStrings(rawValue)
		if err != nil {
			return MetaReviewOutput{}, fmt.Errorf("prompt_gaps: %w", err)
		}
	}
	if rawValue, ok := fields["suggested_few_shot"]; ok {
		out.SuggestedFewShot, err = parseFlexibleBool(rawValue)
		if err != nil {
			return MetaReviewOutput{}, fmt.Errorf("suggested_few_shot: %w", err)
		}
	}

	if err := out.Validate(findingCount); err != nil {
		return MetaReviewOutput{}, err
	}
	return out, nil
}

func extractMetaReviewJSON(raw string) string {
	candidate := strings.TrimSpace(raw)
	if strings.HasPrefix(candidate, "```") {
		if newline := strings.IndexByte(candidate, '\n'); newline >= 0 {
			candidate = candidate[newline+1:]
		}
		if fence := strings.LastIndex(candidate, "```"); fence >= 0 {
			candidate = candidate[:fence]
		}
		candidate = strings.TrimSpace(candidate)
	}
	start := strings.IndexByte(candidate, '{')
	end := strings.LastIndexByte(candidate, '}')
	if start >= 0 && end > start {
		return candidate[start : end+1]
	}
	return candidate
}

func repairInvalidJSONEscapes(raw string) string {
	var out strings.Builder
	out.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' || i+1 >= len(raw) {
			out.WriteByte(raw[i])
			continue
		}
		switch raw[i+1] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't', 'u':
			out.WriteByte(raw[i])
		default:
			out.WriteString(`\\`)
		}
	}
	return out.String()
}

func parseMissedFindings(raw json.RawMessage) ([]MissedFinding, error) {
	items, err := flexibleItems(raw)
	if err != nil {
		return nil, err
	}
	out := make([]MissedFinding, 0, len(items))
	for i, item := range items {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil || fields == nil {
			return nil, fmt.Errorf("item %d must be an object", i)
		}
		finding := MissedFinding{}
		if finding.Type, err = parseFlexibleString(fields["type"]); err != nil {
			return nil, fmt.Errorf("item %d type: %w", i, err)
		}
		if finding.Description, err = parseFlexibleString(fields["description"]); err != nil {
			return nil, fmt.Errorf("item %d description: %w", i, err)
		}
		if finding.Evidence, err = parseFlexibleString(fields["evidence"]); err != nil {
			return nil, fmt.Errorf("item %d evidence: %w", i, err)
		}
		if finding.WhyMissed, err = parseFlexibleString(fields["why_missed"]); err != nil {
			return nil, fmt.Errorf("item %d why_missed: %w", i, err)
		}
		out = append(out, finding)
	}
	return out, nil
}

func parseFalsePositives(raw json.RawMessage) ([]FalsePositive, error) {
	items, err := flexibleItems(raw)
	if err != nil {
		return nil, err
	}
	out := make([]FalsePositive, 0, len(items))
	for i, item := range items {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil || fields == nil {
			return nil, fmt.Errorf("item %d must be an object", i)
		}
		fp := FalsePositive{}
		if fp.FindingIndex, err = parseFlexibleInt(fields["finding_index"]); err != nil {
			return nil, fmt.Errorf("item %d finding_index: %w", i, err)
		}
		if fp.Reason, err = parseFlexibleString(fields["reason"]); err != nil {
			return nil, fmt.Errorf("item %d reason: %w", i, err)
		}
		out = append(out, fp)
	}
	return out, nil
}

func parseFlexibleStrings(raw json.RawMessage) ([]string, error) {
	items, err := flexibleItems(raw)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		value, err := parseFlexibleString(item)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		out = append(out, value)
	}
	return out, nil
}

func flexibleItems(raw json.RawMessage) ([]json.RawMessage, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return []json.RawMessage{}, nil
	}
	if text[0] != '[' {
		return []json.RawMessage{json.RawMessage(text)}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(text), &items); err != nil {
		return nil, err
	}
	if items == nil {
		return []json.RawMessage{}, nil
	}
	return items, nil
}

func firstFlexibleItem(raw json.RawMessage) (json.RawMessage, error) {
	items, err := flexibleItems(raw)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	return items[0], nil
}

func parseFlexibleString(raw json.RawMessage) (string, error) {
	item, err := firstFlexibleItem(raw)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(item))
	if text == "" || text == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(item, &value); err == nil {
		return value, nil
	}
	if text == "true" || text == "false" {
		return text, nil
	}
	if _, err := strconv.ParseFloat(text, 64); err == nil {
		return text, nil
	}
	return "", fmt.Errorf("expected a string-compatible scalar")
}

func parseFlexibleFloat(raw json.RawMessage) (float64, error) {
	item, err := firstFlexibleItem(raw)
	if err != nil {
		return 0, err
	}
	text := strings.TrimSpace(string(item))
	if text == "" || text == "null" {
		return 0, nil
	}
	var encoded string
	if err := json.Unmarshal(item, &encoded); err == nil {
		value, parseErr := strconv.ParseFloat(strings.TrimSpace(encoded), 64)
		if parseErr != nil {
			return 0, parseErr
		}
		return value, nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("expected a number or numeric string")
	}
	return value, nil
}

func parseFlexibleInt(raw json.RawMessage) (int, error) {
	value, err := parseFlexibleFloat(raw)
	if err != nil {
		return 0, err
	}
	asInt := int(value)
	if float64(asInt) != value {
		return 0, fmt.Errorf("expected an integer")
	}
	return asInt, nil
}

func parseFlexibleBool(raw json.RawMessage) (bool, error) {
	item, err := firstFlexibleItem(raw)
	if err != nil {
		return false, err
	}
	text := strings.TrimSpace(string(item))
	if text == "" || text == "null" {
		return false, nil
	}
	var value bool
	if err := json.Unmarshal(item, &value); err == nil {
		return value, nil
	}
	var encoded string
	if err := json.Unmarshal(item, &encoded); err == nil {
		switch strings.ToLower(strings.TrimSpace(encoded)) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no":
			return false, nil
		}
	}
	switch text {
	case "1":
		return true, nil
	case "0":
		return false, nil
	default:
		return false, fmt.Errorf("expected a boolean-compatible scalar")
	}
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
