package workspacesnapshot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPinnedSnapshotReadsCommitAndEnforcesScope(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "src/main.go", "package main\nconst Version = 1\n")
	writeFile(t, repo, "docs/readme.md", "secret\n")
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "initial")

	manager := newTestManager(t, nil)
	snapshot, err := manager.CreatePinned(context.Background(), PinnedGitOptions{
		RepoPath: repo, Ref: "HEAD", PatchRef: "event:1", Patch: []byte("diff"),
		Allowlist: []string{"src"},
	})
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, repo, "src/main.go", "package main\nconst Version = 2\n")
	got, err := snapshot.ReadFile(context.Background(), "src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "Version = 1") {
		t.Fatalf("read live workspace instead of pinned commit: %s", got)
	}
	if _, err := snapshot.ReadFile(context.Background(), "docs/readme.md"); !errors.Is(err, ErrOutsideScope) {
		t.Fatalf("outside allowlist error = %v", err)
	}
	for _, path := range []string{"/etc/passwd", "C:\\Windows\\system.ini", "../src/main.go", "src/../../etc/passwd"} {
		if _, err := snapshot.ReadFile(context.Background(), path); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("path %q error = %v, want ErrInvalidPath", path, err)
		}
	}
	if _, err := snapshot.Resolve("src/main.go"); !errors.Is(err, ErrNotMaterialized) {
		t.Fatalf("Resolve pinned error = %v", err)
	}
	if err := snapshot.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	ref := strings.TrimSpace(run(t, repo, "git", "rev-parse", snapshot.refName))
	if ref != snapshot.Commit {
		t.Fatalf("lease ref = %q, commit = %q", ref, snapshot.Commit)
	}
}

func TestPinnedSnapshotDoesNotTrustMutableExportedMetadata(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "src/main.go", "pinned")
	writeFile(t, repo, "outside.txt", "outside")
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "initial")
	manager := newTestManager(t, nil)
	snapshot, err := manager.CreatePinned(context.Background(), PinnedGitOptions{
		RepoPath: repo, Ref: "HEAD", Patch: []byte("patch"), Allowlist: []string{"src"},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "src/main.go", "later")
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "later")
	snapshot.Commit = strings.TrimSpace(run(t, repo, "git", "rev-parse", "HEAD"))
	snapshot.Allowlist = []string{"."}
	snapshot.Kind = KindMutableCopy
	snapshot.PatchHash = "attacker"
	if got, err := snapshot.ReadFile(context.Background(), "src/main.go"); err != nil || string(got) != "pinned" {
		t.Fatalf("read after metadata mutation = %q, %v", got, err)
	}
	if _, err := snapshot.ReadFile(context.Background(), "outside.txt"); !errors.Is(err, ErrOutsideScope) {
		t.Fatalf("allowlist mutation escaped scope: %v", err)
	}
	if err := snapshot.Verify(); err != nil {
		t.Fatalf("public metadata mutation affected verification: %v", err)
	}
}

func TestPinnedSnapshotRejectsSymlinks(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "target.txt", "target")
	if err := os.Symlink("target.txt", filepath.Join(repo, "link.txt")); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "symlink")

	manager := newTestManager(t, nil)
	_, err := manager.CreatePinned(context.Background(), PinnedGitOptions{
		RepoPath: repo, Ref: "HEAD", Allowlist: []string{"."},
	})
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("CreatePinned error = %v, want ErrSymlink", err)
	}
}

func TestMutableSnapshotCopiesAndVerifiesManifest(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, workspace, "src/main.go", "before")
	writeFile(t, workspace, "outside.txt", "outside")
	manager := newTestManager(t, nil)

	snapshot, err := manager.CreateMutable(context.Background(), MutableCopyOptions{
		WorkspacePath: workspace, Patch: []byte("patch"), Allowlist: []string{"src"},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, workspace, "src/main.go", "after")

	got, err := snapshot.ReadFile(context.Background(), "src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "before" {
		t.Fatalf("copied content = %q", got)
	}
	if _, err := snapshot.ReadFile(context.Background(), "outside.txt"); !errors.Is(err, ErrOutsideScope) {
		t.Fatalf("outside allowlist error = %v", err)
	}
	resolved, err := snapshot.Resolve("src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolved, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.ReadFile(context.Background(), "src/main.go"); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("tampered read error = %v, want ErrHashMismatch", err)
	}
	if err := snapshot.Verify(); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("tampered verify error = %v, want ErrHashMismatch", err)
	}
}

func TestMutableSnapshotCollapsesOverlappingAllowlist(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, workspace, "src/main.go", "package p")
	manager := newTestManager(t, nil)
	snapshot, err := manager.CreateMutable(context.Background(), MutableCopyOptions{
		WorkspacePath: workspace, Allowlist: []string{".", "src", "src/main.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := snapshot.List(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "src/main.go" {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestMutableSnapshotRejectsSymlinkTraversalAndCleansFailure(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, workspace, "target.txt", "target")
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../target.txt", filepath.Join(workspace, "src", "link.txt")); err != nil {
		t.Fatal(err)
	}
	storage := t.TempDir()
	manager, err := NewManager(Config{StorageRoot: storage})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.CreateMutable(context.Background(), MutableCopyOptions{
		WorkspacePath: workspace, Allowlist: []string{"src"},
	})
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("CreateMutable error = %v, want ErrSymlink", err)
	}
	entries, err := os.ReadDir(storage)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial snapshot was not cleaned: %v", entries)
	}
}

func TestLeaseLifetimePreventsGC(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	manager := newTestManager(t, func() time.Time { return now })
	workspace := t.TempDir()
	writeFile(t, workspace, "main.go", "package p")
	snapshot, err := manager.CreateMutable(context.Background(), MutableCopyOptions{
		WorkspacePath: workspace, Allowlist: []string{"."}, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(snapshot.ID, "session-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if lease.ExpiresAt.Sub(now) != 4*time.Hour {
		t.Fatalf("lease duration = %s, want session lifetime", lease.ExpiresAt.Sub(now))
	}

	now = now.Add(2 * time.Hour)
	removed, err := manager.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("GC removed leased snapshot: %v", removed)
	}
	manager.Release(lease.ID)
	now = now.Add(3 * time.Hour)
	removed, err = manager.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(removed, snapshot.ID) {
		t.Fatalf("GC removed = %v, want %s", removed, snapshot.ID)
	}
	if _, err := manager.Get(snapshot.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after GC = %v", err)
	}
}

func TestManagerRejectsLeaseShorterThanSessionLifetime(t *testing.T) {
	_, err := NewManager(Config{
		StorageRoot: t.TempDir(), LeaseTTL: time.Hour, SessionLifetime: 2 * time.Hour,
	})
	if err == nil {
		t.Fatal("expected lease/session validation error")
	}
}

func newTestManager(t *testing.T, clock Clock) *Manager {
	t.Helper()
	manager, err := NewManager(Config{
		StorageRoot: t.TempDir(), SnapshotTTL: time.Hour,
		LeaseTTL: 4 * time.Hour, SessionLifetime: 4 * time.Hour, Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	run(t, repo, "git", "config", "user.email", "test@example.com")
	run(t, repo, "git", "config", "user.name", "Test")
	return repo
}

func writeFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}
