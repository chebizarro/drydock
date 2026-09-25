package repository

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/testutil"
)

func TestTreeAndSearchGolden(t *testing.T) {
	rootPath := t.TempDir()
	writeRepoFixture(t, rootPath, "cmd/main.go", "package main\n\nfunc main() {\n\tprintln(\"needle\")\n}\n")
	writeRepoFixture(t, rootPath, "pkg/value.go", "package pkg\n\nconst Value = \"needle\"\n")
	writeRepoFixture(t, rootPath, "pkg/value_test.go", "package pkg\n\nfunc TestValue() { _ = \"needle\" }\n")

	root, err := OpenRoot(context.Background(), RootSpec{
		ID: "fixture", LocalPath: rootPath, SnapshotStore: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := root.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}

	gotTree, err := snapshot.Tree(context.Background(), TreeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gotSearch, err := snapshot.Search(context.Background(), SearchOptions{
		Query: "needle", Path: "pkg", MaxResults: 20,
	})
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := json.MarshalIndent(struct {
		Tree   []TreeEntry
		Search []SearchHit
	}{gotTree, gotSearch}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	testutil.AssertGolden(t, "testdata/tree_search.golden.json", string(encoded)+"\n")
}

func TestManagedGitRootPinsCheckout(t *testing.T) {
	source := t.TempDir()
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "fixture@example.test")
	runGit(t, source, "config", "user.name", "Fixture")
	writeRepoFixture(t, source, "main.go", "package main\n")
	runGit(t, source, "add", "main.go")
	runGit(t, source, "commit", "-m", "fixture")

	parent := t.TempDir()
	bare := filepath.Join(parent, "fixture.git")
	cmd := exec.Command("git", "clone", "--bare", source, bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v: %s", err, out)
	}
	cmd = exec.Command("git", "--git-dir", bare, "update-server-info")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git update-server-info: %v: %s", err, out)
	}

	server := httptest.NewTLSServer(http.FileServer(http.Dir(parent)))
	defer server.Close()
	t.Setenv("GIT_SSL_NO_VERIFY", "true")

	root, err := OpenRoot(context.Background(), RootSpec{
		ID: "managed",
		ManagedGit: &ManagedGitSpec{
			RepositoryID: "fixture", CloneURLs: []string{server.URL + "/fixture.git"},
			Ref: "HEAD", CacheDir: t.TempDir(),
		},
		SnapshotStore: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := root.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := snapshot.Descriptor()
	if descriptor.Kind != SnapshotPinnedGit || descriptor.Commit == "" {
		t.Fatalf("managed snapshot descriptor = %#v", descriptor)
	}
	content, err := snapshot.ReadFile(context.Background(), "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "package main\n" {
		t.Fatalf("managed snapshot content = %q", content)
	}
}

func writeRepoFixture(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

