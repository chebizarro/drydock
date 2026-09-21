package contextbuilder

import (
	"context"
	"testing"
)

// TestPatchFacadeParsesStructure exercises the one surviving public facade,
// PatchFacade, which owns structured diff parsing (added-line ranges).
func TestPatchFacadeParsesStructure(t *testing.T) {
	diff := "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,1 +1,2 @@\n package main\n+func Added() {}\n"

	result, err := NewPatchFacade().Analyze(PatchAnalysisRequest{Diff: diff})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].Path != "main.go" || len(result.Files[0].AddedLines) != 1 {
		t.Fatalf("patch structure = %+v", result.Files)
	}
}

// TestDeterministicProvidersHandleEmptyInputs covers the production path: the
// deterministic providers must return empty content (not an error) when handed
// an empty BuildInput, and the LSP layer must report degradation rather than
// error when no bridge is configured.
func TestDeterministicProvidersHandleEmptyInputs(t *testing.T) {
	ctx := context.Background()
	search := NewSearcher()
	if search == nil {
		t.Fatal("NewSearcher returned nil")
	}

	providers := []Provider{
		patchDiffProvider{},
		fileContextProvider{},
		symbolsCallsitesProvider{search: search},
		testsProvider{search: search},
		importsExportsProvider{},
		commitHistoryProvider{},
		projectDocsProvider{},
	}
	for _, p := range providers {
		got, err := p.Build(ctx, BuildInput{})
		if err != nil || got != "" {
			t.Fatalf("%s provider on empty input = %q, %v", p.LayerName(), got, err)
		}
	}

	lsp := analyzeLSP(ctx, nil, BuildInput{}, nil)
	if lsp.Status == "" {
		t.Fatalf("nil LSP client must report degradation: %+v", lsp)
	}
}
