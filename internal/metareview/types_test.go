package metareview

import (
	"testing"

	"drydock/internal/reviewengine"
)

const validMetaReviewJSON = `{"missed_findings":[{"type":"correctness","description":"missed nil check","evidence":"foo dereferences bar","why_missed":"prompt_gap"}],"false_positives":[{"finding_index":0,"reason":"the reported issue is guarded"}],"reasoning_quality":0.8,"context_utilization":0.7,"prompt_gaps":["emphasize nil checks"],"suggested_few_shot":true}`

func TestParseMetaReviewOutputForFindingsValidatesRequiredFields(t *testing.T) {
	if _, err := ParseMetaReviewOutputForFindings(`{"missed_findings":[],"false_positives":[],"reasoning_quality":0.8,"context_utilization":0.7,"suggested_few_shot":false}`, 1); err == nil {
		t.Fatal("expected missing prompt_gaps to be rejected")
	}
}

func TestParseMetaReviewOutputForFindingsRejectsInvalidNestedFields(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "invalid missed finding type",
			raw:  `{"missed_findings":[{"type":"nonsense","description":"x","evidence":"y","why_missed":"prompt_gap"}],"false_positives":[],"reasoning_quality":0.8,"context_utilization":0.7,"prompt_gaps":[],"suggested_few_shot":false}`,
		},
		{
			name: "empty missed finding evidence",
			raw:  `{"missed_findings":[{"type":"security","description":"x","evidence":"","why_missed":"prompt_gap"}],"false_positives":[],"reasoning_quality":0.8,"context_utilization":0.7,"prompt_gaps":[],"suggested_few_shot":false}`,
		},
		{
			name: "invalid why missed enum",
			raw:  `{"missed_findings":[{"type":"security","description":"x","evidence":"y","why_missed":"unknown"}],"false_positives":[],"reasoning_quality":0.8,"context_utilization":0.7,"prompt_gaps":[],"suggested_few_shot":false}`,
		},
		{
			name: "empty prompt gap",
			raw:  `{"missed_findings":[],"false_positives":[],"reasoning_quality":0.8,"context_utilization":0.7,"prompt_gaps":[""],"suggested_few_shot":false}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseMetaReviewOutputForFindings(tt.raw, 1); err == nil {
				t.Fatal("expected invalid meta-review output to be rejected")
			}
		})
	}
}

func TestAnalyzeSecurityFindingsTracksCWEOutcomes(t *testing.T) {
	analysis := AnalyzeSecurityFindings([]SecurityFinding{
		{Category: "CWE-89", CWE: "cwe-89", VerifyOutcome: SecurityConfirmed},
		{CWE: "CWE-89", VerifyOutcome: SecurityRefuted, RefuteVotes: 2, VerifyVotes: 3},
		{CWE: "CWE-22", VerifyOutcome: SecurityConfirmed},
	})
	if analysis.TotalCandidates != 3 || analysis.Confirmed != 2 || analysis.Refuted != 1 {
		t.Fatalf("unexpected outcome counts: %+v", analysis)
	}
	if analysis.RefuteRate != 1.0/3.0 {
		t.Fatalf("refute rate = %v, want %v", analysis.RefuteRate, 1.0/3.0)
	}
	if analysis.CWECounts["CWE-89"] != 2 || analysis.ConfirmedByCWE["CWE-22"] != 1 || analysis.RefutedByCWE["CWE-89"] != 1 {
		t.Fatalf("unexpected CWE counts: %+v", analysis)
	}
	for _, finding := range analysis.Findings {
		if finding.Category != "security" {
			t.Fatalf("category = %q, want security", finding.Category)
		}
	}
}

func TestConfirmedSecurityFindingsPreservesCWEMetadata(t *testing.T) {
	got := ConfirmedSecurityFindings([]reviewengine.Finding{{
		Severity: "high", File: "auth.go", Line: 12, Evidence: "[CWE-89] user input reaches query", Explanation: "SQL injection", Confidence: 0.96,
	}})
	if len(got) != 1 || got[0].CWE != "CWE-89" || got[0].VerifyOutcome != SecurityConfirmed || got[0].Category != "security" {
		t.Fatalf("unexpected security finding: %+v", got)
	}
}

func TestParseMetaReviewOutputForFindingsRejectsFalsePositiveIndexOutOfBounds(t *testing.T) {
	if _, err := ParseMetaReviewOutputForFindings(validMetaReviewJSON, 1); err != nil {
		t.Fatalf("valid output rejected: %v", err)
	}

	tooHigh := `{"missed_findings":[],"false_positives":[{"finding_index":1,"reason":"not actually wrong"}],"reasoning_quality":0.8,"context_utilization":0.7,"prompt_gaps":[],"suggested_few_shot":false}`
	if _, err := ParseMetaReviewOutputForFindings(tooHigh, 1); err == nil {
		t.Fatal("expected finding_index >= finding count to be rejected")
	}

	negative := `{"missed_findings":[],"false_positives":[{"finding_index":-1,"reason":"not actually wrong"}],"reasoning_quality":0.8,"context_utilization":0.7,"prompt_gaps":[],"suggested_few_shot":false}`
	if _, err := ParseMetaReviewOutputForFindings(negative, 1); err == nil {
		t.Fatal("expected negative finding_index to be rejected")
	}
}
