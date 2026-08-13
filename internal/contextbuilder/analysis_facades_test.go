package contextbuilder

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExportedAnalysisFacadesMatchDeterministicProviders(t *testing.T) {
	diff := "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,1 +1,2 @@\n package main\n+func Added() {}\n"
	in := BuildInput{PatchEventContent: diff}

	patch, err := AnalyzePatch(in)
	if err != nil {
		t.Fatal(err)
	}
	providerPatch, err := (patchDiffProvider{}).Build(context.Background(), in)
	if err != nil || patch != providerPatch {
		t.Fatalf("patch facade/provider differ: %q vs %q, err=%v", patch, providerPatch, err)
	}
	files, err := AnalyzePatchFiles(diff)
	if err != nil || len(files) != 1 || files[0].Path != "main.go" || len(files[0].AddedLines) != 1 {
		t.Fatalf("patch file analysis = %+v, err=%v", files, err)
	}

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("project guidance"), 0o600); err != nil {
		t.Fatal(err)
	}
	docsInput := BuildInput{RepoPath: repo}
	docs, err := AnalyzeDocs(docsInput)
	if err != nil {
		t.Fatal(err)
	}
	providerDocs, err := (projectDocsProvider{}).Build(context.Background(), docsInput)
	if err != nil || docs != providerDocs {
		t.Fatalf("docs facade/provider differ: %q vs %q, err=%v", docs, providerDocs, err)
	}
}

func TestExportedParameterizedFacadesHandleEmptyInputs(t *testing.T) {
	ctx := context.Background()
	search := NewSearcher()
	if search == nil {
		t.Fatal("NewSearcher returned nil")
	}
	if got, err := AnalyzeSymbols(ctx, BuildInput{}, nil, search); err != nil || got != "" {
		t.Fatalf("AnalyzeSymbols = %q, %v", got, err)
	}
	if got, err := AnalyzeTests(ctx, BuildInput{}, search); err != nil || got != "" {
		t.Fatalf("AnalyzeTests = %q, %v", got, err)
	}
	if got, err := AnalyzeHistory(ctx, BuildInput{}); err != nil || got != "" {
		t.Fatalf("AnalyzeHistory = %q, %v", got, err)
	}
	if got, err := AnalyzeImportsExports(BuildInput{}); err != nil || got != "" {
		t.Fatalf("AnalyzeImportsExports = %q, %v", got, err)
	}
	lsp := AnalyzeLSP(ctx, nil, BuildInput{}, nil)
	if lsp.Status == "" {
		t.Fatalf("nil LSP facade must report degradation: %+v", lsp)
	}
}
