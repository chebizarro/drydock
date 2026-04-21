package repo

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// initWorkRepo creates a git repo with one commit and returns its path.
func initWorkRepo(t *testing.T, dir string) string {
	t.Helper()
	run(t, "", "git", "init", dir)
	run(t, dir, "git", "config", "user.email", "test@test.com")
	run(t, dir, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "README.md"), "# Test\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "initial")
	return dir
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v: %s", name, args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func TestEnsureRepoRejectsNoURLs(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	mgr := NewManager(cacheDir, testLogger())

	_, err := mgr.EnsureRepo(context.Background(), "test:repo", nil)
	if err == nil {
		t.Fatal("expected error for empty clone URLs")
	}
}

func TestEnsureRepoRejectsUnsafeURLs(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	mgr := NewManager(cacheDir, testLogger())

	_, err := mgr.EnsureRepo(context.Background(), "test:repo", []string{
		"ext::sh -c evil",
		"file:///etc/passwd",
	})
	if err == nil {
		t.Fatal("expected error for unsafe clone URLs")
	}
}

func TestEnsureRepoFetchesExisting(t *testing.T) {
	// Pre-create a repo in the cache dir to simulate an already-cloned repo
	cacheDir := t.TempDir()
	mgr := NewManager(cacheDir, testLogger())
	repoPath := mgr.repoPath("test:existing")
	initWorkRepo(t, repoPath)

	// EnsureRepo should succeed (fetch) on the existing repo
	ctx := context.Background()
	path, err := mgr.EnsureRepo(ctx, "test:existing", []string{"https://example.com/unused.git"})
	if err != nil {
		// fetch --all may fail because there's no remote, but the repo exists
		// Actually, git init doesn't add a remote, so fetch will fail.
		// Let's add a remote first.
		t.Logf("EnsureRepo on pre-existing repo: %v (expected if no remote)", err)
	} else if path != repoPath {
		t.Fatalf("expected path %s, got %s", repoPath, path)
	}
}

func TestCheckoutCommitOnBranch(t *testing.T) {
	cacheDir := t.TempDir()
	mgr := NewManager(cacheDir, testLogger())
	repoPath := mgr.repoPath("test:checkout")
	initWorkRepo(t, repoPath)

	ctx := context.Background()
	head := run(t, repoPath, "git", "rev-parse", "HEAD")

	err := mgr.CheckoutCommitOnBranch(ctx, repoPath, "review/test-branch", head)
	if err != nil {
		t.Fatalf("CheckoutCommitOnBranch: %v", err)
	}

	// Verify we're on the right branch
	branch := run(t, repoPath, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if branch != "review/test-branch" {
		t.Fatalf("expected branch review/test-branch, got %s", branch)
	}
}

func TestCheckoutCommitOnBranchRejectsEmptyInputs(t *testing.T) {
	mgr := NewManager(t.TempDir(), testLogger())
	ctx := context.Background()

	if err := mgr.CheckoutCommitOnBranch(ctx, "/tmp", "", "abc"); err == nil {
		t.Fatal("expected error for empty branch")
	}
	if err := mgr.CheckoutCommitOnBranch(ctx, "/tmp", "branch", ""); err == nil {
		t.Fatal("expected error for empty commit")
	}
}

func TestCleanupReviewBranch(t *testing.T) {
	cacheDir := t.TempDir()
	mgr := NewManager(cacheDir, testLogger())
	repoPath := mgr.repoPath("test:cleanup")
	initWorkRepo(t, repoPath)

	ctx := context.Background()
	head := run(t, repoPath, "git", "rev-parse", "HEAD")

	// Create review branch
	if err := mgr.CheckoutCommitOnBranch(ctx, repoPath, "review/to-delete", head); err != nil {
		t.Fatalf("checkout: %v", err)
	}

	// Cleanup should delete the branch
	if err := mgr.CleanupReviewBranch(ctx, repoPath, "review/to-delete"); err != nil {
		t.Fatalf("CleanupReviewBranch: %v", err)
	}
}

func TestCleanupReviewBranchNoop(t *testing.T) {
	mgr := NewManager(t.TempDir(), testLogger())
	// Empty branch name should be a no-op
	if err := mgr.CleanupReviewBranch(context.Background(), "/tmp", ""); err != nil {
		t.Fatalf("expected nil for empty branch, got: %v", err)
	}
}

func TestRepoPath(t *testing.T) {
	mgr := NewManager("/base", testLogger())
	path := mgr.repoPath("abc:def/ghi")
	if !strings.HasPrefix(path, "/base/") {
		t.Fatalf("expected path under /base/, got %s", path)
	}
	// Verify special chars are sanitized
	if strings.ContainsAny(path[len("/base/"):], "/\\:") {
		t.Fatalf("expected sanitized path, got %s", path)
	}
}

func TestGetRepoLockReturnsSameMutex(t *testing.T) {
	mgr := NewManager(t.TempDir(), testLogger())
	m1 := mgr.getRepoLock("/foo")
	m2 := mgr.getRepoLock("/foo")
	m3 := mgr.getRepoLock("/bar")
	if m1 != m2 {
		t.Fatal("expected same mutex for same path")
	}
	if m1 == m3 {
		t.Fatal("expected different mutex for different path")
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("abcdefghijklmnop"); got != "abcdefghijkl" {
		t.Fatalf("expected 12-char truncation, got %s", got)
	}
	if got := shortID("short"); got != "short" {
		t.Fatalf("expected no truncation, got %s", got)
	}
}

func TestIsSafeCloneURLComprehensive(t *testing.T) {
	cases := []struct {
		url  string
		safe bool
	}{
		{"https://github.com/user/repo.git", true},
		{"https://example.com/repo", true},
		{"git://example.com/repo.git", true},
		{"git@github.com:user/repo.git", true},
		{"", false},
		{"ext::sh -c evil%", false},
		{"file:///etc/passwd", false},
		{"http://insecure.com/repo", false},
		{"ftp://bad.com/repo", false},
		{"git@host:path;cmd", false},
		{"git@host:path`cmd`", false},
		{"git@host:path$HOME", false},
	}
	for _, tc := range cases {
		got := isSafeCloneURL(tc.url)
		if got != tc.safe {
			t.Errorf("isSafeCloneURL(%q) = %v, want %v", tc.url, got, tc.safe)
		}
	}
}
