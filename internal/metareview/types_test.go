package metareview

import (
	"testing"

	"drydock/internal/reviewengine"
)

const validMetaReviewJSON = `{"missed_findings":[{"type":"correctness","description":"missed nil check","evidence":"foo dereferences bar","why_missed":"prompt_gap"}],"false_positives":[{"finding_index":0,"reason":"the reported issue is guarded"}],"reasoning_quality":0.8,"context_utilization":0.7,"prompt_gaps":["emphasize nil checks"],"suggested_few_shot":true}`

func TestParseMetaReviewOutputForFindingsDefaultsMissingOptionalFields(t *testing.T) {
	out, err := ParseMetaReviewOutputForFindings(`{"reasoning_quality":"0.8"}`, 1)
	if err != nil {
		t.Fatalf("near-miss output rejected: %v", err)
	}
	if out.ReasoningQuality != 0.8 || out.MissedFindings == nil || out.FalsePositives == nil || out.PromptGaps == nil {
		t.Fatalf("missing fields were not defaulted safely: %+v", out)
	}

	if _, err := ParseMetaReviewOutputForFindings(`{"summary":"nothing usable"}`, 1); err == nil {
		t.Fatal("expected output with no recognized fields to be rejected")
	}
}

func TestParseMetaReviewOutputForFindingsToleratesCorpusNearMisses(t *testing.T) {
	raw := "```json\n" +
		`{"missed_findings":[],"false_positives":{"finding_index":"0","reason":["guarded"]},` +
		`"reasoning_quality":"0.8","context_utilization":["0.7"],"prompt_gaps":42,"suggested_few_shot":[true]}` +
		"\n```"
	out, err := ParseMetaReviewOutputForFindings(raw, 1)
	if err != nil {
		t.Fatalf("tolerant parse failed: %v", err)
	}
	if out.ReasoningQuality != 0.8 || out.ContextUtilization != 0.7 {
		t.Fatalf("numeric strings/arrays were not coerced: %+v", out)
	}
	if len(out.FalsePositives) != 1 || out.FalsePositives[0].FindingIndex != 0 || out.FalsePositives[0].Reason != "guarded" {
		t.Fatalf("scalar object or nested scalar/array slip was not coerced: %+v", out.FalsePositives)
	}
	if len(out.PromptGaps) != 1 || out.PromptGaps[0] != "42" || !out.SuggestedFewShot {
		t.Fatalf("prompt gap or boolean array was not coerced: %+v", out)
	}
}

func TestParseMetaReviewOutputRepairsInvalidEscape(t *testing.T) {
	raw := `{"missed_findings":[],"false_positives":[],"reasoning_quality":0.8,"context_utilization":0.7,"prompt_gaps":["check \q path"],"suggested_few_shot":false}`
	out, err := ParseMetaReviewOutput(raw)
	if err != nil {
		t.Fatalf("invalid escape near-miss was not repaired: %v", err)
	}
	if len(out.PromptGaps) != 1 || out.PromptGaps[0] != `check \q path` {
		t.Fatalf("unexpected repaired prompt gap: %#v", out.PromptGaps)
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
