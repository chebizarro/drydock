package repo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"drydock/internal/db"

	"fiatjaf.com/nostr"
)

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

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

	// The fork's origin shares history with the fork: the initial commit is
	// the default branch head, and the PR tip adds one commit on top of it.
	forkOrigin := filepath.Join(t.TempDir(), "fork-origin")
	run(t, "", "git", "init", "--bare", forkOrigin)
	run(t, forkPath, "git", "remote", "add", "origin", forkOrigin)
	run(t, forkPath, "git", "push", "origin", "HEAD:refs/heads/main")
	forkBase := run(t, forkPath, "git", "rev-parse", "HEAD")

	writeFile(t, filepath.Join(forkPath, ".drydock.yaml"), "policy: fork\n")
	run(t, forkPath, "git", "add", ".drydock.yaml")
	run(t, forkPath, "git", "commit", "-m", "fork policy")
	forkTip := run(t, forkPath, "git", "rev-parse", "HEAD")

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
			// Local path is rejected by isSafeCloneURL, so no fetch from the
			// fork URL is ever attempted in this test (no network access).
			{"clone", filepath.Join(t.TempDir(), "nonexistent-fork.git")},
			{"c", forkTip},
			{"merge-base", forkBase},
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
	defer svc.CleanupPreparedReview(ctx, result)
	if result.RepoPath == forkPath || result.RepoPath == canonicalPath || !strings.Contains(result.RepoPath, ".worktrees") {
		t.Fatalf("expected isolated PR worktree, got %s", result.RepoPath)
	}
	if result.ExpectedCommit != forkTip || run(t, result.RepoPath, "git", "rev-parse", "HEAD") != forkTip {
		t.Fatalf("PR worktree not pinned to tip %s: %+v", forkTip, result)
	}
	if !strings.Contains(result.Diff, "diff --git") || !strings.Contains(result.Diff, ".drydock.yaml") {
		t.Fatalf("expected PR prepare to produce a unified diff of the tip, got %q", result.Diff)
	}
	if result.BaseCommit != forkBase || result.TipCommit != forkTip || len(result.DiffSHA256) != 64 {
		t.Fatalf("unexpected PR diff provenance: %+v", result)
	}
}

func TestPreparePRTipUsesAssertedBaseWithStaleCanonicalDefault(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate store: %v", err)
	}

	repoID := "owner:stale"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO repositories
		(repo_id, pubkey, identifier, announcement_event_id, name, description, clone_urls, relays, raw_event_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		repoID, "owner", "stale", "announcement", "stale", "", "https://canonical.example/stale.git", "", "{}", int64(1), int64(1)); err != nil {
		t.Fatalf("seed repository: %v", err)
	}

	mgr := NewManager(t.TempDir(), testLogger())
	forkPath := mgr.repoPath(repoID)
	initWorkRepo(t, forkPath)
	staleCanonicalTip := run(t, forkPath, "git", "rev-parse", "HEAD")

	forkOrigin := filepath.Join(t.TempDir(), "fork-origin")
	run(t, "", "git", "init", "--bare", forkOrigin)
	run(t, forkPath, "git", "remote", "add", "origin", forkOrigin)
	run(t, forkPath, "git", "push", "origin", "HEAD:refs/heads/main")

	writeFile(t, filepath.Join(forkPath, "historical.go"), "package historical\n")
	run(t, forkPath, "git", "add", "historical.go")
	run(t, forkPath, "git", "commit", "-m", "history after stale canonical")
	assertedBase := run(t, forkPath, "git", "rev-parse", "HEAD")

	writeFile(t, filepath.Join(forkPath, "feature.go"), "package feature\n")
	run(t, forkPath, "git", "add", "feature.go")
	run(t, forkPath, "git", "commit", "-m", "PR feature")
	tip := run(t, forkPath, "git", "rev-parse", "HEAD")

	// Seed the canonical cache with all objects while leaving origin/main at
	// the old commit. The asserted base must win over this stale default.
	canonicalPath := mgr.canonicalRepoPath(repoID)
	run(t, "", "git", "clone", forkPath, canonicalPath)
	canonicalOrigin := filepath.Join(t.TempDir(), "canonical-origin")
	run(t, "", "git", "init", "--bare", canonicalOrigin)
	run(t, forkPath, "git", "push", canonicalOrigin, staleCanonicalTip+":refs/heads/main")
	run(t, canonicalPath, "git", "remote", "set-url", "origin", canonicalOrigin)
	run(t, canonicalPath, "git", "fetch", "origin")

	target := nostr.Event{
		ID:   nostr.MustIDFromHex("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"),
		Kind: 1618,
		Tags: nostr.Tags{
			{"clone", filepath.Join(t.TempDir(), "nonexistent-fork.git")},
			{"c", tip},
			{"merge-base", assertedBase},
		},
	}
	rec := db.PatchEventRecord{EventID: target.ID.Hex(), RepoID: repoID, RootID: target.ID.Hex(), Kind: 1618}

	svc := NewService(store, mgr, testLogger())
	result, err := svc.preparePRTip(ctx, rec, target)
	if err != nil {
		t.Fatalf("prepare PR tip against stale canonical clone: %v", err)
	}
	defer svc.CleanupPreparedReview(ctx, result)
	if result.BaseCommit != assertedBase || result.TipCommit != tip || result.ExpectedCommit != tip {
		t.Fatalf("unexpected selected commits: base=%s tip=%s expected=%s", result.BaseCommit, result.TipCommit, result.ExpectedCommit)
	}
	if !strings.Contains(result.Diff, "feature.go") || strings.Contains(result.Diff, "historical.go") {
		t.Fatalf("expected only asserted-base delta, got %q", result.Diff)
	}
	if result.DiffFiles != 1 || result.DiffBytes != int64(len(result.Diff)) || len(result.DiffSHA256) != 64 {
		t.Fatalf("unexpected diff metadata: %+v", result)
	}
}

func TestPreparePRTipRefusesImplicitBaseInForkClone(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repoID := "owner:no-fork-default"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO repositories
		(repo_id, pubkey, identifier, announcement_event_id, name, description, clone_urls, relays, raw_event_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		repoID, "owner", "repo", "announcement", "repo", "", "https://canonical.example/repo.git", "", "{}", int64(1), int64(1)); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(t.TempDir(), testLogger())
	forkPath := mgr.repoPath(repoID)
	initWorkRepo(t, forkPath)
	forkOrigin := filepath.Join(t.TempDir(), "fork-origin")
	run(t, "", "git", "init", "--bare", forkOrigin)
	run(t, forkPath, "git", "remote", "add", "origin", forkOrigin)
	run(t, forkPath, "git", "push", "origin", "HEAD:refs/heads/main")
	writeFile(t, filepath.Join(forkPath, "feature.go"), "package feature\n")
	run(t, forkPath, "git", "add", "feature.go")
	run(t, forkPath, "git", "commit", "-m", "feature")
	tip := run(t, forkPath, "git", "rev-parse", "HEAD")

	canonicalPath := mgr.canonicalRepoPath(repoID)
	initWorkRepo(t, canonicalPath) // unrelated history; does not contain tip
	canonicalOrigin := filepath.Join(t.TempDir(), "canonical-origin")
	run(t, "", "git", "init", "--bare", canonicalOrigin)
	run(t, canonicalPath, "git", "remote", "add", "origin", canonicalOrigin)
	run(t, canonicalPath, "git", "push", "origin", "HEAD:refs/heads/main")

	target := nostr.Event{
		ID:   nostr.MustIDFromHex("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
		Kind: 1618,
		Tags: nostr.Tags{{"clone", filepath.Join(t.TempDir(), "unsafe-local.git")}, {"c", tip}},
	}
	rec := db.PatchEventRecord{EventID: target.ID.Hex(), RepoID: repoID, RootID: target.ID.Hex(), Kind: 1618}
	if _, err := NewService(store, mgr, testLogger()).preparePRTip(ctx, rec, target); err == nil || !strings.Contains(err.Error(), "canonical clone for implicit-base diff") {
		t.Fatalf("expected implicit fork-base rejection, got %v", err)
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
	canonicalHead := run(t, canonicalPath, "git", "rev-parse", "HEAD")

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
	defer svc.CleanupPreparedReview(ctx, result)
	if result.RepoPath == canonicalPath || !strings.Contains(result.RepoPath, ".worktrees") {
		t.Fatalf("expected isolated patch-series worktree, got %s", result.RepoPath)
	}
	if result.ExpectedCommit != canonicalHead || run(t, result.RepoPath, "git", "rev-parse", "HEAD") != canonicalHead {
		t.Fatalf("patch-series worktree not pinned to canonical HEAD %s: %+v", canonicalHead, result)
	}
	if got := string(mustReadFile(t, filepath.Join(result.RepoPath, "README.md"))); !strings.Contains(got, "patched") {
		t.Fatalf("patch-series worktree missing applied patch: %q", got)
	}
}

func TestConcurrentSameRepoReviewsUseIsolatedWorktrees(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	repoID := "owner:concurrent"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO repositories
		(repo_id, pubkey, identifier, announcement_event_id, name, description, clone_urls, relays, raw_event_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		repoID, "owner", "concurrent", "announcement", "concurrent", "", "https://canonical.example/concurrent.git", "", "{}", int64(1), int64(1)); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(t.TempDir(), testLogger())
	sourcePath := mgr.repoPath(repoID)
	initWorkRepo(t, sourcePath)
	origin := filepath.Join(t.TempDir(), "origin")
	run(t, "", "git", "init", "--bare", origin)
	run(t, origin, "git", "symbolic-ref", "HEAD", "refs/heads/main")
	run(t, sourcePath, "git", "remote", "add", "origin", origin)
	run(t, sourcePath, "git", "push", "origin", "HEAD:refs/heads/main")
	base := run(t, sourcePath, "git", "rev-parse", "HEAD")

	writeFile(t, filepath.Join(sourcePath, "isolated.txt"), "review A\n")
	run(t, sourcePath, "git", "add", "isolated.txt")
	run(t, sourcePath, "git", "commit", "-m", "review A")
	tipA := run(t, sourcePath, "git", "rev-parse", "HEAD")
	run(t, sourcePath, "git", "branch", "review-a", tipA)

	run(t, sourcePath, "git", "checkout", "--detach", base)
	writeFile(t, filepath.Join(sourcePath, "isolated.txt"), "review B\n")
	run(t, sourcePath, "git", "add", "isolated.txt")
	run(t, sourcePath, "git", "commit", "-m", "review B")
	tipB := run(t, sourcePath, "git", "rev-parse", "HEAD")
	run(t, sourcePath, "git", "branch", "review-b", tipB)

	canonicalPath := mgr.canonicalRepoPath(repoID)
	run(t, "", "git", "clone", origin, canonicalPath)
	run(t, canonicalPath, "git", "fetch", sourcePath, "review-a", "review-b")

	targets := []nostr.Event{
		{
			ID:   nostr.MustIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
			Kind: 1618,
			Tags: nostr.Tags{{"clone", filepath.Join(t.TempDir(), "unsafe-local.git")}, {"c", tipA}, {"merge-base", base}},
		},
		{
			ID:   nostr.MustIDFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
			Kind: 1618,
			Tags: nostr.Tags{{"clone", filepath.Join(t.TempDir(), "unsafe-local.git")}, {"c", tipB}, {"merge-base", base}},
		},
	}

	svc := NewService(store, mgr, testLogger())
	results := make([]PrepareResult, len(targets))
	errs := make([]error, len(targets))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := db.PatchEventRecord{
				EventID: targets[i].ID.Hex(), RepoID: repoID,
				RootID: targets[i].ID.Hex(), Kind: int(targets[i].Kind),
			}
			results[i], errs[i] = svc.preparePRTip(ctx, rec, targets[i])
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("prepare concurrent review %d: %v", i, err)
		}
	}
	defer svc.CleanupPreparedReview(ctx, results[0])
	defer svc.CleanupPreparedReview(ctx, results[1])

	if results[0].RepoPath == results[1].RepoPath {
		t.Fatalf("concurrent reviews shared worktree %s", results[0].RepoPath)
	}
	for i, want := range []struct {
		tip     string
		content string
	}{{tipA, "review A\n"}, {tipB, "review B\n"}} {
		if err := svc.AssertPreparedReview(ctx, results[i]); err != nil {
			t.Fatalf("review %d checkout assertion: %v", i, err)
		}
		if results[i].ExpectedCommit != want.tip {
			t.Fatalf("review %d expected commit = %s, want %s", i, results[i].ExpectedCommit, want.tip)
		}
		if got := string(mustReadFile(t, filepath.Join(results[i].RepoPath, "isolated.txt"))); got != want.content {
			t.Fatalf("review %d read cross-review content %q, want %q", i, got, want.content)
		}
	}
	if _, err := os.Stat(filepath.Join(canonicalPath, "isolated.txt")); !os.IsNotExist(err) {
		t.Fatalf("canonical checkout was mutated by concurrent reviews: %v", err)
	}

	firstPath, secondPath := results[0].RepoPath, results[1].RepoPath
	svc.CleanupPreparedReview(ctx, results[0])
	svc.CleanupPreparedReview(ctx, results[1])
	listed := run(t, canonicalPath, "git", "worktree", "list", "--porcelain")
	if strings.Contains(listed, firstPath) || strings.Contains(listed, secondPath) {
		t.Fatalf("concurrent review worktrees were not pruned:\n%s", listed)
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

func TestPRMergeBaseCommit(t *testing.T) {
	evt := nostr.Event{ID: nostr.MustIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), Tags: nostr.Tags{{"merge-base", "ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD"}}}
	base, err := prMergeBaseCommit(evt)
	if err != nil || base != "abcdefabcdefabcdefabcdefabcdefabcdefabcd" {
		t.Fatalf("merge base = %q, err=%v", base, err)
	}
	if _, err := prMergeBaseCommit(nostr.Event{ID: evt.ID, Tags: nostr.Tags{{"merge-base", "bad"}}}); err == nil {
		t.Fatal("expected invalid merge-base error")
	}
}
