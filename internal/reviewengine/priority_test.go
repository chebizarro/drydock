package reviewengine

import (
	"log/slog"
	"strings"
	"testing"
)

func TestPrioritySeverityMappingIsTotal(t *testing.T) {
	tests := []struct {
		severity string
		priority Priority
	}{
		{"critical", PriorityP0},
		{"high", PriorityP1},
		{"medium", PriorityP2},
		{"low", PriorityP2},
		{"info", PriorityP2},
	}
	for _, test := range tests {
		got, ok := PriorityFromSeverity(test.severity)
		if !ok || got != test.priority {
			t.Fatalf("PriorityFromSeverity(%q) = %q, %v; want %q, true", test.severity, got, ok, test.priority)
		}
	}
	for priority, severity := range map[Priority]string{
		PriorityP0: "critical",
		PriorityP1: "high",
		PriorityP2: "medium",
	} {
		got, ok := SeverityFromPriority(priority)
		if !ok || got != severity {
			t.Fatalf("SeverityFromPriority(%q) = %q, %v; want %q, true", priority, got, ok, severity)
		}
	}
}

func TestParseReviewerOutputNormalizesPriorityCompatibility(t *testing.T) {
	out, err := ParseReviewerOutput(`{
		"summary":"compat",
		"findings":[
			{"priority":"P0","severity":"critical","category":"security","file":"a.go","line":1,"evidence":"e","explanation":"x","suggestion":"s","confidence":0.9},
			{"severity":"low","category":"style","file":"b.go","line":2,"evidence":"e","explanation":"x","suggestion":"s","confidence":0.9}
		],
		"needs_more_context":[]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.Findings[0].Priority != PriorityP0 || out.Findings[1].Priority != PriorityP2 {
		t.Fatalf("normalized priorities = %q, %q", out.Findings[0].Priority, out.Findings[1].Priority)
	}
}

func TestParseReviewerOutputRejectsConflictingPrioritySeverity(t *testing.T) {
	_, err := ParseReviewerOutput(`{
		"summary":"conflict",
		"findings":[
			{"priority":"P0","severity":"low","category":"correctness","file":"a.go","line":1,"evidence":"e","explanation":"x","suggestion":"s","confidence":0.9}
		],
		"needs_more_context":[]
	}`)
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("error = %v, want priority/severity conflict", err)
	}
}

func TestConsensusPreservesHighestPriorityAcrossConfidence(t *testing.T) {
	reviews := []modelResult{
		{Route: RouteCoder32B, Review: ReviewerOutput{Summary: "critical", Findings: []Finding{{
			Priority: PriorityP0, Severity: "critical", File: "same.go", Category: "security", Line: 10, Confidence: 0.61,
		}}}},
		{Route: RouteLLM70B, Review: ReviewerOutput{Summary: "confident", Findings: []Finding{{
			Priority: PriorityP2, Severity: "medium", File: "same.go", Category: "security", Line: 10, Confidence: 0.99,
		}}}},
	}
	for _, ordered := range [][]modelResult{reviews, {reviews[1], reviews[0]}} {
		got := mergeFindings(ordered, EnsembleConfig{}, slog.Default())
		if len(got) != 1 || got[0].Priority != PriorityP0 || got[0].Severity != "critical" || got[0].Confidence != 1.0 {
			t.Fatalf("consensus downgraded canonical priority: %+v", got)
		}
	}
}

func TestDeduplicateFindingsPreservesHighestPriority(t *testing.T) {
	got := DeduplicateFindings([]Finding{
		{Priority: PriorityP0, Severity: "critical", File: "same.go", Category: "security", Line: 10, Confidence: 0.61},
		{Priority: PriorityP1, Severity: "high", File: "same.go", Category: "security", Line: 11, Confidence: 0.99},
	})
	if len(got) != 1 || got[0].Priority != PriorityP0 || got[0].Severity != "critical" || got[0].Confidence != 0.99 {
		t.Fatalf("deduplication downgraded canonical priority: %+v", got)
	}
}

func TestConsensusSortRecognizesCanonicalPriorities(t *testing.T) {
	reviews := []modelResult{{
		Route: RouteCoder32B,
		Review: ReviewerOutput{Summary: "priority", Findings: []Finding{
			{Priority: PriorityP2, Severity: "medium", File: "p2.go", Category: "correctness", Line: 1, Confidence: 0.99},
			{Priority: PriorityP0, Severity: "critical", File: "p0.go", Category: "correctness", Line: 1, Confidence: 0.61},
			{Priority: PriorityP1, Severity: "high", File: "p1.go", Category: "correctness", Line: 1, Confidence: 0.80},
		}},
	}}
	got := mergeFindings(reviews, EnsembleConfig{}, slog.Default())
	if len(got) != 3 || got[0].Priority != PriorityP0 || got[1].Priority != PriorityP1 || got[2].Priority != PriorityP2 {
		t.Fatalf("priority order = %+v", got)
	}
}
