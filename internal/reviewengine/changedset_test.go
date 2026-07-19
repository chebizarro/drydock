package reviewengine

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestFilterFindingsToChangedFiles(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	findings := []Finding{
		{File: "src/lib/html.ts", Severity: "high"},
		{File: "README.md", Severity: "high"},         // context layer, not changed
		{File: "CONTRIBUTING.md", Severity: "medium"}, // context layer, not changed
		{File: "./package.json", Severity: "low"},     // normalized match
		{File: "", Severity: "info"},                  // unanchored: kept
	}
	changed := []string{"src/lib/html.ts", "package.json"}

	kept := filterFindingsToChangedFiles(findings, changed, logger, "test")
	if len(kept) != 3 {
		t.Fatalf("expected 3 kept findings, got %d: %+v", len(kept), kept)
	}
	for _, f := range kept {
		if f.File == "README.md" || f.File == "CONTRIBUTING.md" {
			t.Fatalf("finding for unchanged doc %s survived the filter", f.File)
		}
	}

	// Empty changed set: no-op (eval harnesses without a deterministic set).
	if got := filterFindingsToChangedFiles(findings, nil, logger, "test"); len(got) != len(findings) {
		t.Fatalf("expected no filtering with empty changed set, got %d", len(got))
	}
}

func TestFilterWalkthroughToChangedFiles(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := WalkthroughOutput{
		Walkthrough: "text",
		FileSummaries: []FileSummary{
			{File: "src/lib/html.ts", Summary: "changed"},
			{File: "README.md", Summary: "hallucinated"},
		},
	}
	got := filterWalkthroughToChangedFiles(w, []string{"src/lib/html.ts"}, logger)
	if len(got.FileSummaries) != 1 || got.FileSummaries[0].File != "src/lib/html.ts" {
		t.Fatalf("expected only changed file summary, got %+v", got.FileSummaries)
	}
	if got.Walkthrough != "text" {
		t.Fatalf("walkthrough text must be preserved")
	}
}

func TestEngineRunDropsFindingsForUnchangedFiles(t *testing.T) {
	fake := &fakeLLM{
		responses: []string{
			`{"change_type":"feature","risk_areas":[],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`,
			`{"summary":"ok","findings":[` +
				`{"severity":"high","category":"correctness","file":"src/lib/html.ts","line":3,"evidence":"x","explanation":"y","suggestion":"z","confidence":0.9},` +
				`{"severity":"high","category":"style","file":"README.md","line":1,"evidence":"doc","explanation":"doc","suggestion":"doc","confidence":0.92}` +
				`],"needs_more_context":[]}`,
			`{"walkthrough":"Adds html helper.","file_summaries":[{"file":"src/lib/html.ts","summary":"changed"},{"file":"CONTRIBUTING.md","summary":"hallucinated"}]}`,
		},
	}
	engine := New(Config{
		Planner:  ModelEndpoint{BaseURL: "http://planner", Model: "planner-model"},
		Coder32B: ModelEndpoint{BaseURL: "http://32b", Model: "32b-model"},
		LLM70B:   ModelEndpoint{BaseURL: "http://70b", Model: "70b-model"},
		Coder14B: ModelEndpoint{BaseURL: "http://14b", Model: "14b-model"},
	}, fake, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	out, err := engine.Run(context.Background(), RunInput{
		ContextBundle: "ctx",
		ChangedFiles:  []string{"src/lib/html.ts"},
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(out.Review.Findings) != 1 || out.Review.Findings[0].File != "src/lib/html.ts" {
		t.Fatalf("expected only the changed-file finding, got %+v", out.Review.Findings)
	}
	if len(out.Walkthrough.FileSummaries) != 1 || out.Walkthrough.FileSummaries[0].File != "src/lib/html.ts" {
		t.Fatalf("expected walkthrough summaries filtered to changed files, got %+v", out.Walkthrough.FileSummaries)
	}
}
