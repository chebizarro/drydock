package contextbuilder

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearcherInit(t *testing.T) {
	s := newSearcher()
	s.init()

	// Just verify init doesn't panic; rg may or may not be installed.
	// UseRipgrep is consistent after init.
	_ = s.UseRipgrep()
}

func TestSearcherFallbackToGitGrep(t *testing.T) {
	// Create a temp git repo with a file containing a symbol
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %s", args, err, out)
		}
	}

	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "test")

	os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

func Hello() string {
	return "hello"
}

func Caller() {
	msg := Hello()
	println(msg)
}
`), 0o644)

	os.WriteFile(filepath.Join(dir, "main_test.go"), []byte(`package main

import "testing"

func TestHello(t *testing.T) {
	got := Hello()
	if got != "hello" {
		t.Fatal("wrong")
	}
}
`), 0o644)

	run("add", ".")
	run("commit", "-m", "init")

	// Force git grep fallback by creating a searcher with no rg
	s := &searcher{hasRg: false}

	ctx := context.Background()

	result, err := s.SearchSymbol(ctx, dir, "Hello")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Hello") {
		t.Errorf("expected to find Hello in grep output, got: %s", result)
	}

	testResult, err := s.SearchSymbolTests(ctx, dir, "Hello")
	if err != nil {
		t.Fatal(err)
	}
	// Word-boundary search for "Hello" finds Hello() calls in test files
	// but not "TestHello" (no boundary between Test and Hello)
	if !strings.Contains(testResult, "Hello") {
		t.Errorf("expected to find Hello in test grep output, got: %s", testResult)
	}
	if !strings.Contains(testResult, "_test.go") {
		t.Errorf("expected results from test files, got: %s", testResult)
	}
}

func TestSearcherRipgrep(t *testing.T) {
	// Skip if rg is not installed
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed, skipping ripgrep test")
	}

	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %s", args, err, out)
		}
	}

	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "test")

	os.WriteFile(filepath.Join(dir, "lib.go"), []byte(`package main

func Calculate(x int) int {
	return x * 2
}

func Use() {
	result := Calculate(42)
	_ = result
}
`), 0o644)

	os.WriteFile(filepath.Join(dir, "lib_test.go"), []byte(`package main

import "testing"

func TestCalculate(t *testing.T) {
	if Calculate(2) != 4 {
		t.Fatal("wrong")
	}
}
`), 0o644)

	run("add", ".")
	run("commit", "-m", "init")

	s := newSearcher()
	if !s.UseRipgrep() {
		t.Fatal("expected ripgrep to be available")
	}

	ctx := context.Background()

	result, err := s.SearchSymbol(ctx, dir, "Calculate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Calculate") {
		t.Errorf("expected to find Calculate in rg output, got: %s", result)
	}

	testResult, err := s.SearchSymbolTests(ctx, dir, "Calculate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(testResult, "Calculate") {
		t.Errorf("expected to find Calculate in rg test output, got: %s", testResult)
	}
	if !strings.Contains(testResult, "_test.go") {
		t.Errorf("expected results from test files, got: %s", testResult)
	}
}

func TestSearcherNoMatch(t *testing.T) {
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %s", args, err, out)
		}
	}

	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "test")

	os.WriteFile(filepath.Join(dir, "empty.go"), []byte("package main\n"), 0o644)
	run("add", ".")
	run("commit", "-m", "init")

	s := &searcher{hasRg: false}
	ctx := context.Background()

	// git grep returns exit code 1 for no match, which runGit wraps as error
	result, err := s.SearchSymbol(ctx, dir, "NonExistentSymbol")
	if result != "" && err == nil {
		t.Errorf("expected empty result for non-existent symbol, got: %s", result)
	}
}
