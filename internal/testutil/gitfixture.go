// Package testutil provides test doubles shared across drydock packages.
// This package must ONLY be imported by test files (*_test.go).
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// RunGit runs `git -C dir args...` and fails the test on error. It is the one
// shared git-invocation helper for fixture setup across packages.
func RunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// InitRepo creates a temp git repository, writes files (relative path -> content,
// nested directories created as needed), and makes a single "fixture" commit
// under a fixed test identity. It returns the repository path. A commit's tree
// hash is content-only, so the fixed identity does not perturb tree-keyed
// golden or cache assertions.
func InitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	for path, content := range files {
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	RunGit(t, repo, "init", "-q")
	RunGit(t, repo, "config", "user.email", "test@example.test")
	RunGit(t, repo, "config", "user.name", "Test")
	RunGit(t, repo, "add", ".")
	RunGit(t, repo, "commit", "-qm", "fixture")
	return repo
}
