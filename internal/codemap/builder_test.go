package codemap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildWholeTreeGraphsAndCache(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"main.go": `package main

import "fmt"

func leaf() {
	fmt.Println("leaf")
}

func helper() {
	leaf()
}

func main() {
	helper()
}
`,
		"other.go": `package main

func unrelated() {}
`,
		"README.md": "not source",
	})
	cacheDir := t.TempDir()
	builder := New(WithCacheDir(cacheDir))

	first, err := builder.Build(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if first.Cache.TreeHit {
		t.Fatal("first build unexpectedly hit tree cache")
	}
	if first.Cache.ParsedFiles != 2 || first.Cache.ReusedFiles != 0 {
		t.Fatalf("unexpected first-build cache stats: %+v", first.Cache)
	}
	if len(first.Files) != 2 {
		t.Fatalf("got %d indexed files, want 2", len(first.Files))
	}
	if got := first.ImportGraph["main.go"]; len(got) != 1 || got[0] != "fmt" {
		t.Fatalf("unexpected import graph: %#v", first.ImportGraph)
	}

	mainSymbol := onlySymbol(t, first, "main")
	helperSymbol := onlySymbol(t, first, "helper")
	leafSymbol := onlySymbol(t, first, "leaf")
	if !contains(first.Callees(mainSymbol.ID), helperSymbol.ID) {
		t.Fatalf("main callees = %#v, want helper", first.Callees(mainSymbol.ID))
	}
	if !contains(first.Callees(helperSymbol.ID), leafSymbol.ID) {
		t.Fatalf("helper callees = %#v, want leaf", first.Callees(helperSymbol.ID))
	}
	if !contains(first.Callers(leafSymbol.ID), helperSymbol.ID) {
		t.Fatalf("leaf callers = %#v, want helper", first.Callers(leafSymbol.ID))
	}
	if len(first.RepoMap) != 4 {
		t.Fatalf("repo map has %d entries, want 4", len(first.RepoMap))
	}
	if first.RepoMap[0].Name != "leaf" {
		t.Fatalf("top-ranked symbol = %s, want leaf; map=%#v", first.RepoMap[0].Name, first.RepoMap)
	}

	second, err := builder.Build(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Cache.TreeHit || second.TreeHash != first.TreeHash {
		t.Fatalf("second build did not hit tree cache: %+v", second.Cache)
	}

	writeAndCommit(t, repo, "main.go", strings.Replace(
		mustRead(t, filepath.Join(repo, "main.go")),
		`func main() {
	helper()`,
		`func main() {
	helper()
	unrelated()`,
		1,
	))
	third, err := builder.Build(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if third.TreeHash == first.TreeHash {
		t.Fatal("tree hash did not change")
	}
	if third.Cache.TreeHit || third.Cache.ParsedFiles != 1 || third.Cache.ReusedFiles != 1 {
		t.Fatalf("per-blob invalidation stats = %+v, want one parsed and one reused", third.Cache)
	}
}

func TestBuildValidation(t *testing.T) {
	tests := []struct {
		name string
		repo string
		ref  string
	}{
		{name: "missing repo"},
		{name: "not a repository", repo: t.TempDir()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Build(context.Background(), tt.repo, tt.ref, WithCacheDir(t.TempDir())); err == nil {
				t.Fatal("Build() error = nil")
			}
		})
	}
}

func onlySymbol(t *testing.T, result *Map, name string) Symbol {
	t.Helper()
	declarations := result.SymbolIndex[name]
	if len(declarations) != 1 {
		t.Fatalf("symbols named %q = %#v, want one", name, declarations)
	}
	return declarations[0]
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func initRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.email", "codemap@example.com")
	git(t, repo, "config", "user.name", "Codemap Test")
	for path, content := range files {
		abs := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	return repo
}

func writeAndCommit(t *testing.T, repo, path, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", path)
	git(t, repo, "commit", "-m", "update")
}

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
