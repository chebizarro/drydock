package contextbuilder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"drydock/internal/lspbridge"
)

func TestSymbolsProviderLSPIntegration(t *testing.T) {
	// Stand up a fake LSP bridge HTTP server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			json.NewEncoder(w).Encode(lspbridge.HealthResponse{Status: "ok"})
			return
		}
		if r.URL.Path == "/analyze" {
			var req lspbridge.AnalyzeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request", 400)
				return
			}
			// Return mock definitions and references for any request.
			resp := lspbridge.AnalyzeResponse{
				Definitions: []lspbridge.SymbolInfo{
					{Name: "handleEvent", Kind: "function", File: "handler.go", Line: 42, Detail: "func(ctx context.Context, e Event) error", Language: "go"},
					{Name: "EventProcessor", Kind: "type", File: "processor.go", Line: 10, Detail: "struct", Language: "go"},
				},
				References: []lspbridge.Reference{
					{Symbol: "handleEvent", File: "main.go", Line: 15, Column: 4},
					{Symbol: "handleEvent", File: "handler_test.go", Line: 28, Column: 8},
				},
				Diagnostics: []lspbridge.Diagnostic{
					{File: "handler.go", Line: 50, Severity: "warning", Message: "unused variable", Source: "gopls"},
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := lspbridge.NewClient(srv.URL)

	provider := symbolsCallsitesProvider{
		lspClient: client,
		// No extractor — will fall back to regex symbol extraction.
	}

	// A minimal unified diff that extractChangedSymbols can parse.
	// Lines starting with + and containing func/type trigger regex extraction.
	diff := `diff --git a/handler.go b/handler.go
--- a/handler.go
+++ b/handler.go
@@ -40,1 +40,4 @@
+func handleEvent(ctx context.Context, e Event) error {
+	// new code
+	return nil
 }
`

	result, err := provider.Build(context.Background(), BuildInput{
		PatchEventContent: diff,
		RepoPath:          "/fake/repo",
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	// Should contain LSP-sourced content.
	if !strings.Contains(result, "Definitions (LSP)") {
		t.Fatalf("expected LSP definitions in result, got:\n%s", result)
	}
	if !strings.Contains(result, "handleEvent") {
		t.Fatalf("expected handleEvent symbol in result, got:\n%s", result)
	}
	if !strings.Contains(result, "References (LSP)") {
		t.Fatalf("expected LSP references in result, got:\n%s", result)
	}
	if !strings.Contains(result, "Diagnostics (LSP)") {
		t.Fatalf("expected LSP diagnostics in result, got:\n%s", result)
	}
	if !strings.Contains(result, "handler_test.go") {
		t.Fatalf("expected reference to handler_test.go in result, got:\n%s", result)
	}
}

func TestSymbolsProviderFallsBackOnLSPError(t *testing.T) {
	// LSP bridge that always returns 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", 500)
	}))
	defer srv.Close()

	client := lspbridge.NewClient(srv.URL)

	provider := symbolsCallsitesProvider{
		lspClient: client,
		search:    newSearcher(),
	}

	diff := `diff --git a/handler.go b/handler.go
--- a/handler.go
+++ b/handler.go
@@ -40,1 +40,4 @@
+func handleEvent(ctx context.Context, e Event) error {
+	// new code
+	return nil
 }
`

	result, err := provider.Build(context.Background(), BuildInput{
		PatchEventContent: diff,
		RepoPath:          "/fake/repo",
	})
	if err != nil {
		t.Fatalf("Build should not error on LSP failure: %v", err)
	}

	// Should NOT contain LSP content — fell back to git grep.
	if strings.Contains(result, "Definitions (LSP)") {
		t.Fatalf("should have fallen back from LSP, but got LSP content:\n%s", result)
	}
	// Should still have the symbols header from regex extraction.
	if !strings.Contains(result, "symbols:") {
		t.Fatalf("expected symbols header, got:\n%s", result)
	}
}

func TestSymbolsProviderNoLSPUsesGitGrep(t *testing.T) {
	// No LSP client set — should go straight to git grep path.
	provider := symbolsCallsitesProvider{
		lspClient: nil,
		search:    newSearcher(),
	}

	diff := `diff --git a/handler.go b/handler.go
--- a/handler.go
+++ b/handler.go
@@ -40,1 +40,4 @@
+func handleEvent(ctx context.Context, e Event) error {
+	// new code
+	return nil
 }
`

	result, err := provider.Build(context.Background(), BuildInput{
		PatchEventContent: diff,
		RepoPath:          "/fake/repo",
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	if strings.Contains(result, "Definitions (LSP)") {
		t.Fatalf("should not have LSP content when client is nil:\n%s", result)
	}
}

func TestSymbolsProviderLSPEmptyResponseFallsBack(t *testing.T) {
	// LSP bridge returns success but with empty results.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/analyze" {
			json.NewEncoder(w).Encode(lspbridge.AnalyzeResponse{})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := lspbridge.NewClient(srv.URL)

	provider := symbolsCallsitesProvider{
		lspClient: client,
		search:    newSearcher(),
	}

	diff := `diff --git a/handler.go b/handler.go
--- a/handler.go
+++ b/handler.go
@@ -40,1 +40,4 @@
+func handleEvent(ctx context.Context, e Event) error {
+	// new code
+	return nil
 }
`

	result, err := provider.Build(context.Background(), BuildInput{
		PatchEventContent: diff,
		RepoPath:          "/fake/repo",
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	// Empty LSP response should fall back to git grep.
	if strings.Contains(result, "Definitions (LSP)") {
		t.Fatalf("should fall back on empty LSP response:\n%s", result)
	}
}
