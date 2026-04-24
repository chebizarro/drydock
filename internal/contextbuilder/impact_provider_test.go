package contextbuilder

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/symbols"
)

func initImpactTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", dir)
	return dir
}

func writeFile(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitAddCommit(t *testing.T, dir, msg string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("add", "-A")
	run("commit", "-m", msg)
}

func TestChangeImpactProviderMeta(t *testing.T) {
	p := changeImpactProvider{}
	if p.LayerName() != LayerChangeImpact {
		t.Errorf("expected %q, got %q", LayerChangeImpact, p.LayerName())
	}
	if p.Priority() != 2 {
		t.Errorf("expected priority 2, got %d", p.Priority())
	}
}

func TestChangeImpactEmptyPatch(t *testing.T) {
	p := changeImpactProvider{search: newSearcher()}
	result, err := p.Build(context.Background(), BuildInput{
		PatchEventContent: "",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Error("expected empty result for empty patch")
	}
}

func TestComputeRiskScore(t *testing.T) {
	tests := []struct {
		name  string
		s     impactSummary
		files int
		want  int
	}{
		{"zero impact", impactSummary{}, 1, 0},
		{"downstream only", impactSummary{DownstreamFiles: []string{"a", "b", "c"}}, 1, 3},
		{"cross workspace", impactSummary{
			DownstreamFiles:     []string{"a"},
			CrossWorkspaceRoots: []string{"pkg1", "pkg2"},
		}, 1, 3},
		{"structural changes", impactSummary{
			DownstreamFiles:   []string{"a"},
			StructuralChanges: 3,
		}, 1, 3}, // min(2,3) = 2 + 1 downstream
		{"many files", impactSummary{
			DownstreamFiles: []string{"a", "b", "c", "d", "e"},
		}, 5, 5}, // min(4,5)=4 + 1 for 5+ files
		{"capped at 10", impactSummary{
			DownstreamFiles:     []string{"1", "2", "3", "4", "5"},
			CrossWorkspaceRoots: []string{"a", "b", "c", "d"},
			StructuralChanges:   3,
			DownstreamTests:     []string{"t1", "t2", "t3"},
		}, 10, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeRiskScore(tt.s, tt.files)
			if got != tt.want {
				t.Errorf("score = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRiskLevel(t *testing.T) {
	tests := []struct {
		score int
		want  string
	}{
		{0, "low"}, {2, "low"},
		{3, "medium"}, {5, "medium"},
		{6, "high"}, {8, "high"},
		{9, "critical"}, {10, "critical"},
	}
	for _, tt := range tests {
		if got := riskLevel(tt.score); got != tt.want {
			t.Errorf("riskLevel(%d) = %q, want %q", tt.score, got, tt.want)
		}
	}
}

func TestIsStructuralKind(t *testing.T) {
	if !isStructuralKind(symbols.KindInterface) {
		t.Error("interface should be structural")
	}
	if !isStructuralKind(symbols.KindStruct) {
		t.Error("struct should be structural")
	}
	if isStructuralKind(symbols.KindFunction) {
		t.Error("function should not be structural")
	}
}

func TestNoisySymbolFiltering(t *testing.T) {
	// init, main, run should be filtered out.
	for _, name := range []string{"init", "main", "run", "new", "get", "set"} {
		if !noisySymbols[strings.ToLower(name)] {
			t.Errorf("%q should be in noisy symbols", name)
		}
	}
	// "UserStore" should not be noisy.
	if noisySymbols["userstore"] {
		t.Error("UserStore should not be noisy")
	}
}

func TestIsTestFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"main_test.go", true},
		{"main.go", false},
		{"src/test/helper.go", true},
		{"app.spec.js", true},
		{"app.test.ts", true},
		{"test_utils.py", true},
		{"internal/auth/store.go", false},
	}
	for _, tt := range tests {
		if got := isTestFile(tt.path); got != tt.want {
			t.Errorf("isTestFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestWorkspaceOf(t *testing.T) {
	roots := []string{"packages/web", "packages/api", "services/auth"}
	tests := []struct {
		path string
		want string
	}{
		{"packages/web/src/index.ts", "packages/web"},
		{"packages/api/handler.go", "packages/api"},
		{"services/auth/store.go", "services/auth"},
		{"cmd/server/main.go", "."},
	}
	for _, tt := range tests {
		if got := workspaceOf(tt.path, roots); got != tt.want {
			t.Errorf("workspaceOf(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestCrossWorkspaceImpact(t *testing.T) {
	changed := []string{"packages/api/handler.go"}
	downstream := []string{"packages/web/src/client.ts", "cmd/server/main.go"}
	roots := []string{"packages/api", "packages/web"}

	cross := crossWorkspaceImpact(changed, downstream, roots)
	if len(cross) != 1 || cross[0] != "packages/web" {
		t.Errorf("expected [packages/web], got %v", cross)
	}
}

func TestRenderImpact(t *testing.T) {
	s := impactSummary{
		Score: 7,
		Level: "high",
		ChangedSymbols: []impactSymbol{
			{Name: "UserStore", Kind: symbols.KindInterface, File: "auth/store.go"},
		},
		DownstreamFiles:     []string{"cmd/main.go", "api/handler.go", "web/client.go"},
		DirectImporters:     []string{"cmd/main.go", "api/handler.go"},
		SymbolConsumers:     []string{"web/client.go"},
		DownstreamTests:     []string{"auth/store_test.go"},
		CrossWorkspaceRoots: []string{"web"},
		StructuralChanges:   1,
	}
	output := renderImpact(s)

	if !strings.Contains(output, "7/10 (high)") {
		t.Error("missing score/level header")
	}
	if !strings.Contains(output, "UserStore (interface) in auth/store.go") {
		t.Error("missing changed symbol")
	}
	if !strings.Contains(output, "Downstream non-test consumers: 3") {
		t.Error("missing downstream count")
	}
	if !strings.Contains(output, "Cross-workspace impact: 1") {
		t.Error("missing cross-workspace count")
	}
	if !strings.Contains(output, "Planner hint:") {
		t.Error("missing planner hint for high-impact")
	}
}

func TestRenderImpactTruncation(t *testing.T) {
	var files []string
	for i := 0; i < 20; i++ {
		files = append(files, fmt.Sprintf("pkg/file%d.go", i))
	}
	s := impactSummary{
		Score:           5,
		Level:           "medium",
		DirectImporters: files,
		DownstreamFiles: files,
	}
	output := renderImpact(s)
	if !strings.Contains(output, "... and 15 more") {
		t.Error("expected truncation marker")
	}
}

func TestFileModuleIDsGo(t *testing.T) {
	ids := fileModuleIDs("internal/auth/store.go", "github.com/example/drydock", "/tmp/repo")
	if len(ids) != 1 {
		t.Fatalf("expected 1 ID, got %v", ids)
	}
	if ids[0] != "github.com/example/drydock/internal/auth" {
		t.Errorf("got %q", ids[0])
	}
}

func TestFileModuleIDsPython(t *testing.T) {
	ids := fileModuleIDs("myapp/auth/store.py", "", "/tmp/repo")
	if len(ids) != 1 || ids[0] != "myapp.auth.store" {
		t.Errorf("expected [myapp.auth.store], got %v", ids)
	}

	ids2 := fileModuleIDs("myapp/__init__.py", "", "/tmp/repo")
	if len(ids2) != 1 || ids2[0] != "myapp" {
		t.Errorf("expected [myapp], got %v", ids2)
	}
}

func TestFileModuleIDsJS(t *testing.T) {
	ids := fileModuleIDs("src/auth/store.ts", "", "/tmp/repo")
	if len(ids) != 2 {
		t.Fatalf("expected 2 IDs, got %v", ids)
	}
	if ids[0] != "./src/auth/store" || ids[1] != "./src/auth/store.ts" {
		t.Errorf("got %v", ids)
	}
}

func TestGoImporterBlastRadius(t *testing.T) {
	dir := initImpactTestRepo(t)

	// Write a go.mod.
	writeFile(t, dir, "go.mod", "module example.com/test\n\ngo 1.21\n")

	// Write a package that will be changed.
	writeFile(t, dir, "internal/auth/store.go", `package auth

type UserStore interface {
	GetUser(id string) (string, error)
}
`)

	// Write an importer package.
	writeFile(t, dir, "cmd/server/main.go", `package main

import "example.com/test/internal/auth"

func main() {
	var _ auth.UserStore
}
`)

	// Write another importer.
	writeFile(t, dir, "internal/api/handler.go", `package api

import "example.com/test/internal/auth"

func handle(s auth.UserStore) {}
`)

	gitAddCommit(t, dir, "initial")

	// Simulate a patch that modifies the auth store.
	patch := `diff --git a/internal/auth/store.go b/internal/auth/store.go
--- a/internal/auth/store.go
+++ b/internal/auth/store.go
@@ -1,5 +1,6 @@
 package auth
 
 type UserStore interface {
 	GetUser(id string) (string, error)
+	DeleteUser(id string) error
 }
`

	p := changeImpactProvider{search: newSearcher()}
	result, err := p.Build(context.Background(), BuildInput{
		PatchEventContent: patch,
		RepoPath:          dir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should find direct importers.
	if !strings.Contains(result, "cmd/server/main.go") || !strings.Contains(result, "internal/api/handler.go") {
		t.Errorf("expected importers in output:\n%s", result)
	}

	// Should report non-zero blast radius.
	if !strings.Contains(result, "Blast radius:") {
		t.Error("expected blast radius header")
	}
}

func TestLowImpactSuppression(t *testing.T) {
	dir := initImpactTestRepo(t)

	// Single isolated file, no importers, no consumers.
	writeFile(t, dir, "standalone.go", "package main\nfunc main() {}\n")
	gitAddCommit(t, dir, "initial")

	patch := `diff --git a/standalone.go b/standalone.go
--- a/standalone.go
+++ b/standalone.go
@@ -1,2 +1,3 @@
 package main
+// added comment
 func main() {}
`

	p := changeImpactProvider{search: newSearcher()}
	result, err := p.Build(context.Background(), BuildInput{
		PatchEventContent: patch,
		RepoPath:          dir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result for low-impact isolated change, got:\n%s", result)
	}
}

func TestParseSearchHits(t *testing.T) {
	raw := "main.go:42:func main() {\nauth/store.go:10:type UserStore interface {"
	hits := parseSearchHits(raw)
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0].File != "main.go" || hits[0].Line != 42 {
		t.Errorf("hit[0] = %+v", hits[0])
	}
	if hits[1].File != "auth/store.go" || hits[1].Line != 10 {
		t.Errorf("hit[1] = %+v", hits[1])
	}
}
