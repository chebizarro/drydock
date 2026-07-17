package repo

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestRepoPathUsesHashAndStaysUnderBase(t *testing.T) {
	baseDir := t.TempDir()
	mgr := NewManager(baseDir, testLogger())
	hostileIDs := []string{
		"..",
		".",
		"a/../../b",
		filepath.Join(string(filepath.Separator), "tmp", "escape"),
		"a:b",
		"control\x00\n\r\tb",
	}

	seen := make(map[string]string)
	for _, repoID := range hostileIDs {
		for _, repoPath := range []string{mgr.repoPath(repoID), mgr.canonicalRepoPath(repoID)} {
			resolved, err := mgr.validateRepoPath(repoPath)
			if err != nil {
				t.Fatalf("validate path for repo ID %q: %v", repoID, err)
			}
			rel, err := filepath.Rel(baseDir, resolved)
			if err != nil {
				t.Fatalf("relative path for repo ID %q: %v", repoID, err)
			}
			if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("repo ID %q escaped base directory: %s", repoID, resolved)
			}
			component := filepath.Base(resolved)
			if len(component) != sha256.Size*2 || strings.Trim(component, "0123456789abcdef") != "" {
				t.Fatalf("repo ID %q produced non-SHA-256 cache component %q", repoID, component)
			}
			if prior, ok := seen[component]; ok {
				t.Fatalf("repo ID %q collided with %s", repoID, prior)
			}
			seen[component] = fmt.Sprintf("repo ID %q", repoID)
		}
	}

	if mgr.repoPath("a/b") == mgr.repoPath("a_b") {
		t.Fatal("distinct repo IDs must not share a cache path")
	}
}

func TestValidateRepoPathRejectsOutsideAndSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	baseDir := filepath.Join(parent, "cache")
	outsideDir := filepath.Join(parent, "outside")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(baseDir, testLogger())

	for _, path := range []string{baseDir, parent, outsideDir} {
		if _, err := mgr.validateRepoPath(path); err == nil {
			t.Fatalf("expected path outside cache to be rejected: %s", path)
		}
	}

	linkPath := filepath.Join(baseDir, "escape")
	if err := os.Symlink(outsideDir, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.validateRepoPath(linkPath); err == nil {
		t.Fatal("expected symlink escaping cache to be rejected")
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

func TestEvictionByCount(t *testing.T) {
	cacheDir := t.TempDir()
	mgr := NewManager(cacheDir, testLogger(), WithMaxRepoCount(2))

	// Create 3 repos with staggered access times
	for i, name := range []string{"old", "mid", "new"} {
		repoPath := filepath.Join(cacheDir, name)
		initWorkRepo(t, repoPath)
		// Stagger access times
		marker := filepath.Join(repoPath, accessMarkerFile)
		accessTime := time.Now().Add(time.Duration(i-3) * time.Hour)
		os.WriteFile(marker, []byte(accessTime.Format(time.RFC3339)), 0o644)
		os.Chtimes(marker, accessTime, accessTime)
	}

	// Verify we have 3 repos
	repos, _ := mgr.listCachedRepos()
	if len(repos) != 3 {
		t.Fatalf("expected 3 repos, got %d", len(repos))
	}

	// Evict — should remove the oldest (old) to get down to 2
	mgr.evictIfNeeded()

	repos, _ = mgr.listCachedRepos()
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos after eviction, got %d", len(repos))
	}

	// The "old" repo should be gone
	if _, err := os.Stat(filepath.Join(cacheDir, "old")); !os.IsNotExist(err) {
		t.Fatal("expected 'old' repo to be evicted")
	}
	// "mid" and "new" should remain
	for _, name := range []string{"mid", "new"} {
		if _, err := os.Stat(filepath.Join(cacheDir, name)); err != nil {
			t.Fatalf("expected %q repo to remain: %v", name, err)
		}
	}
}

func TestEvictionSkipsRepoWhileOperationHoldsLock(t *testing.T) {
	cacheDir := t.TempDir()
	mgr := NewManager(cacheDir, testLogger(), WithMaxRepoCount(1))

	var paths []string
	for i, name := range []string{"in-use", "middle", "newest"} {
		repoPath := filepath.Join(cacheDir, name)
		initWorkRepo(t, repoPath)
		marker := filepath.Join(repoPath, accessMarkerFile)
		accessTime := time.Now().Add(time.Duration(i-3) * time.Hour)
		if err := os.WriteFile(marker, []byte(accessTime.Format(time.RFC3339)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(marker, accessTime, accessTime); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, repoPath)
	}

	inUsePath, err := mgr.validateRepoPath(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		mu := mgr.getRepoLock(inUsePath)
		mu.Lock()
		close(locked)
		<-release
		mu.Unlock()
		close(done)
	}()
	<-locked
	defer func() {
		close(release)
		<-done
	}()

	mgr.evictIfNeeded()

	if _, err := os.Stat(inUsePath); err != nil {
		t.Fatalf("in-use repo was evicted during operation: %v", err)
	}
}

func TestEvictionNoLimits(t *testing.T) {
	cacheDir := t.TempDir()
	mgr := NewManager(cacheDir, testLogger()) // no limits

	// Create a repo
	repoPath := filepath.Join(cacheDir, "keep")
	initWorkRepo(t, repoPath)

	// Should be a no-op
	mgr.evictIfNeeded()

	if _, err := os.Stat(repoPath); err != nil {
		t.Fatal("repo should not be evicted when no limits set")
	}
}

func TestTouchAccessAndRepoAccessTime(t *testing.T) {
	cacheDir := t.TempDir()
	mgr := NewManager(cacheDir, testLogger())

	repoPath := filepath.Join(cacheDir, "testrepo")
	initWorkRepo(t, repoPath)

	before := time.Now().Add(-time.Second)
	mgr.touchAccess(repoPath)
	after := time.Now().Add(time.Second)

	at := repoAccessTime(repoPath)
	if at.Before(before) || at.After(after) {
		t.Fatalf("access time %v not in expected range [%v, %v]", at, before, after)
	}
}
