package metareview

import "testing"

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
