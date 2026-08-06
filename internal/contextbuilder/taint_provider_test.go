package contextbuilder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/lspbridge"
)

func TestTaintProviderMeta(t *testing.T) {
	p := taintProvider{}
	if p.LayerName() != LayerTaint {
		t.Fatalf("LayerName() = %q, want %q", p.LayerName(), LayerTaint)
	}
	if p.Priority() != 2 {
		t.Fatalf("Priority() = %d, want 2", p.Priority())
	}
}

func TestTaintProviderTreeSitterCallGraph(t *testing.T) {
	repo := t.TempDir()
	source := `package sample

import "os/exec"

func Handle(req string) {
	process(req)
}

func process(input string) {
	run(input)
}

func run(value string) {
	exec.Command(value)
}
`
	if err := os.WriteFile(filepath.Join(repo, "handler.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := `diff --git a/handler.go b/handler.go
--- a/handler.go
+++ b/handler.go
@@ -1,1 +1,2 @@
 package sample
+// changed
`

	got, err := (taintProvider{}).Build(context.Background(), BuildInput{RepoPath: repo, PatchEventContent: patch})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	for _, want := range []string{"tree-sitter call-graph approximation", "SOURCE [request input]", "Handle", "process", "run", "SINK [command-execution]"} {
		if !strings.Contains(got, want) {
			t.Errorf("Build() missing %q:\n%s", want, got)
		}
	}
}

func TestTaintProviderUsesLSPReferences(t *testing.T) {
	repo := t.TempDir()
	source := `package sample

import "os/exec"

func Handle(req string) {
	callback := dangerous
	_ = callback
}

func dangerous(value string) {
	exec.Command(value)
}
`
	if err := os.WriteFile(filepath.Join(repo, "handler.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/analyze" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(lspbridge.AnalyzeResponse{
			Status:       "ok",
			LSPAvailable: true,
			References: []lspbridge.Reference{{
				Symbol: "dangerous",
				File:   "handler.go",
				Line:   6,
			}},
		})
	}))
	defer server.Close()

	patch := `diff --git a/handler.go b/handler.go
--- a/handler.go
+++ b/handler.go
@@ -1,1 +1,2 @@
 package sample
+// changed
`
	provider := taintProvider{lspClient: lspbridge.NewClient(server.URL)}
	got, err := provider.Build(context.Background(), BuildInput{RepoPath: repo, PatchEventContent: patch})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if !strings.Contains(got, "LSP-assisted call hierarchy") {
		t.Fatalf("Build() did not report LSP assistance:\n%s", got)
	}
	if !strings.Contains(got, "Handle -> handler.go:") || !strings.Contains(got, " dangerous -> SINK") {
		t.Fatalf("Build() missing LSP-provided edge:\n%s", got)
	}
}

func TestTaintProviderLimitsPathsToChangedFiles(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "unsafe.go"), []byte(`package sample
import "os/exec"
func Unsafe(input string) { exec.Command(input) }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "safe.go"), []byte("package sample\nfunc Safe() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := `diff --git a/safe.go b/safe.go
--- a/safe.go
+++ b/safe.go
@@ -1,1 +1,2 @@
 package sample
+// changed
`

	got, err := (taintProvider{}).Build(context.Background(), BuildInput{RepoPath: repo, PatchEventContent: patch})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got != "" {
		t.Fatalf("Build() = %q, want no unrelated path", got)
	}
}
