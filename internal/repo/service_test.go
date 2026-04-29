package repo

import (
	"context"
	"path/filepath"
	"testing"

	"drydock/internal/db"

	"fiatjaf.com/nostr"
)

func TestPreparePRTipReadsConfigFromCanonicalCache(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate store: %v", err)
	}

	repoID := "owner:repo"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO repositories
		(repo_id, pubkey, identifier, announcement_event_id, name, description, clone_urls, relays, raw_event_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		repoID, "owner", "repo", "announcement", "repo", "", "https://canonical.example/repo.git", "", "{}", int64(1), int64(1)); err != nil {
		t.Fatalf("seed repository: %v", err)
	}

	mgr := NewManager(t.TempDir(), testLogger())
	forkPath := mgr.repoPath(repoID)
	initWorkRepo(t, forkPath)
	writeFile(t, filepath.Join(forkPath, ".drydock.yaml"), "policy: fork\n")
	run(t, forkPath, "git", "add", ".drydock.yaml")
	run(t, forkPath, "git", "commit", "-m", "fork policy")
	forkTip := run(t, forkPath, "git", "rev-parse", "HEAD")

	forkOrigin := filepath.Join(t.TempDir(), "fork-origin")
	initWorkRepo(t, forkOrigin)
	run(t, forkPath, "git", "remote", "add", "origin", forkOrigin)

	canonicalPath := mgr.canonicalRepoPath(repoID)
	initWorkRepo(t, canonicalPath)
	writeFile(t, filepath.Join(canonicalPath, ".drydock.yaml"), "policy: canonical\n")
	run(t, canonicalPath, "git", "add", ".drydock.yaml")
	run(t, canonicalPath, "git", "commit", "-m", "canonical policy")

	canonicalOrigin := filepath.Join(t.TempDir(), "canonical-origin")
	initWorkRepo(t, canonicalOrigin)
	run(t, canonicalPath, "git", "remote", "add", "origin", canonicalOrigin)

	svc := NewService(store, mgr, testLogger())
	target := nostr.Event{
		ID:   nostr.MustIDFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		Kind: 1618,
		Tags: nostr.Tags{
			{"clone", "https://fork.example/repo.git"},
			{"c", forkTip},
		},
	}
	rec := db.PatchEventRecord{EventID: target.ID.Hex(), RepoID: repoID, RootID: target.ID.Hex(), Kind: 1618}

	result, err := svc.preparePRTip(ctx, rec, target)
	if err != nil {
		t.Fatalf("prepare PR tip: %v", err)
	}
	if got, want := string(result.BaseRepoConfig), "policy: canonical"; got != want {
		t.Fatalf("expected canonical config %q, got %q", want, got)
	}
	if result.RepoPath != forkPath {
		t.Fatalf("expected PR checkout to use fork cache path %s, got %s", forkPath, result.RepoPath)
	}
}

func TestPreparePatchSeriesReadsConfigFromCanonicalCache(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate store: %v", err)
	}

	repoID := "owner:repo"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO repositories
		(repo_id, pubkey, identifier, announcement_event_id, name, description, clone_urls, relays, raw_event_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		repoID, "owner", "repo", "announcement", "repo", "", "https://canonical.example/repo.git", "", "{}", int64(1), int64(1)); err != nil {
		t.Fatalf("seed repository: %v", err)
	}

	mgr := NewManager(t.TempDir(), testLogger())
	forkPath := mgr.repoPath(repoID)
	initWorkRepo(t, forkPath)
	writeFile(t, filepath.Join(forkPath, ".drydock.yaml"), "policy: fork\n")
	run(t, forkPath, "git", "add", ".drydock.yaml")
	run(t, forkPath, "git", "commit", "-m", "fork policy")

	forkOrigin := filepath.Join(t.TempDir(), "fork-origin")
	initWorkRepo(t, forkOrigin)
	run(t, forkPath, "git", "remote", "add", "origin", forkOrigin)

	canonicalPath := mgr.canonicalRepoPath(repoID)
	initWorkRepo(t, canonicalPath)
	writeFile(t, filepath.Join(canonicalPath, ".drydock.yaml"), "policy: canonical\n")
	run(t, canonicalPath, "git", "add", ".drydock.yaml")
	run(t, canonicalPath, "git", "commit", "-m", "canonical policy")

	canonicalOrigin := filepath.Join(t.TempDir(), "canonical-origin")
	initWorkRepo(t, canonicalOrigin)
	run(t, canonicalPath, "git", "remote", "add", "origin", canonicalOrigin)

	svc := NewService(store, mgr, testLogger())
	target := nostr.Event{
		ID:   nostr.MustIDFromHex("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"),
		Kind: 1617,
		Content: "diff --git a/README.md b/README.md\n" +
			"--- a/README.md\n" +
			"+++ b/README.md\n" +
			"@@ -1 +1,2 @@\n" +
			" # Test\n" +
			"+patched\n",
	}
	rec := db.PatchEventRecord{EventID: target.ID.Hex(), RepoID: repoID, RootID: target.ID.Hex(), Kind: 1617}

	result, err := svc.preparePatchSeries(ctx, rec, target)
	if err != nil {
		t.Fatalf("prepare patch series: %v", err)
	}
	if got, want := string(result.BaseRepoConfig), "policy: canonical"; got != want {
		t.Fatalf("expected canonical config %q, got %q", want, got)
	}
	if result.RepoPath != canonicalPath {
		t.Fatalf("expected patch series to use canonical cache path %s, got %s", canonicalPath, result.RepoPath)
	}
}

func TestCloneURLsFromEvent(t *testing.T) {
	evt := nostr.Event{Tags: nostr.Tags{
		{"clone", "https://a.example/repo.git", "https://b.example/repo.git"},
		{"clone", "https://a.example/repo.git"},
	}}
	urls := cloneURLsFromEvent(evt)
	if len(urls) != 2 {
		t.Fatalf("expected 2 unique clone urls, got %d (%v)", len(urls), urls)
	}
}

func TestPRTipCommit(t *testing.T) {
	evt := nostr.Event{ID: nostr.MustIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), Tags: nostr.Tags{{"c", "1111111111111111111111111111111111111111"}}}
	tip, err := prTipCommit(evt)
	if err != nil {
		t.Fatalf("expected tip commit, got error: %v", err)
	}
	if tip != "1111111111111111111111111111111111111111" {
		t.Fatalf("unexpected tip commit %s", tip)
	}
}

func TestPRTipCommitMissing(t *testing.T) {
	evt := nostr.Event{ID: nostr.MustIDFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")}
	if _, err := prTipCommit(evt); err == nil {
		t.Fatalf("expected error for missing c tag")
	}
}
