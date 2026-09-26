package depupgrade

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/deprunner"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
	"git.sharegap.net/cascadia/drydock/internal/publisher"
	"git.sharegap.net/cascadia/drydock/internal/repo"
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
	"git.sharegap.net/cascadia/drydock/internal/signing"

	"fiatjaf.com/nostr"
)

// --- stubs -----------------------------------------------------------------

type captureRelay struct{ events []nostr.Event }

func (c *captureRelay) Publish(_ context.Context, _ []string, event nostr.Event) error {
	c.events = append(c.events, event)
	return nil
}

type stubScanner struct{ findings []reviewengine.Finding }

func (s stubScanner) Scan(context.Context, string) ([]reviewengine.Finding, error) {
	return s.findings, nil
}

type stubUpdater struct {
	resp  *deprunner.UpdateResponse
	calls int
}

func (u *stubUpdater) Update(context.Context, deprunner.UpdateRequest) (*deprunner.UpdateResponse, error) {
	u.calls++
	return u.resp, nil
}

type stubResolver struct{ res Resolution }

func (r stubResolver) Resolve(context.Context, reviewengine.PackageIdentity, string) (Resolution, error) {
	return r.res, nil
}

type stubMonitoring struct{ ok bool }

func (m stubMonitoring) Contains(string) bool { return m.ok }

type stubRepositories struct{ ids []string }

func (r stubRepositories) MonitoredRepositoryIDs() []string { return r.ids }

// stubWorkspaces creates a real temp worktree seeded with a go.mod so the manifest
// gathering and change-apply paths run for real, but skips git by returning a
// canned diff after invoking mutate.
type stubWorkspaces struct {
	repoID     string
	baseConfig []byte
	root       string
	mutateRan  bool
}

func (w *stubWorkspaces) PrepareUpgradeWorkspace(_ context.Context, repoID string) (repo.PrepareResult, error) {
	if err := os.WriteFile(filepath.Join(w.root, "go.mod"), []byte("module example.com/app\n\ngo 1.22\n\nrequire example.com/foo v1.0.0\n"), 0o644); err != nil {
		return repo.PrepareResult{}, err
	}
	return repo.PrepareResult{RepoID: repoID, RepoPath: w.root, BaseRepoConfig: w.baseConfig}, nil
}

func (w *stubWorkspaces) GenerateWorktreeDiff(ctx context.Context, _ string, mutate func(context.Context) error) (string, []string, error) {
	if err := mutate(ctx); err != nil {
		return "", nil, err
	}
	w.mutateRan = true
	diff := "diff --git a/go.mod b/go.mod\n--- a/go.mod\n+++ b/go.mod\n@@ -5 +5 @@\n-require example.com/foo v1.0.0\n+require example.com/foo v1.0.1\n"
	return diff, []string{"go.mod", "go.sum"}, nil
}

func (w *stubWorkspaces) CleanupPreparedReview(context.Context, repo.PrepareResult) {}

// --- test ------------------------------------------------------------------

func newTestService(t *testing.T, ctx context.Context, updater *stubUpdater, relay *captureRelay) (*Service, *db.Store, string) {
	t.Helper()
	store, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	signer, err := signing.NewLocalSigner("0000000000000000000000000000000000000000000000000000000000000001")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pub := publisher.New(publisher.Config{DefaultRelays: []string{"wss://relay.test"}}, store, signer, relay, logger)

	ws := &stubWorkspaces{
		root:       t.TempDir(),
		baseConfig: []byte("version: 1\nupgrades:\n  enabled: true\n  ecosystems: [go]\n  policy: next_patch\n"),
	}
	svc, err := New(Config{}, Dependencies{
		Store:      store,
		Workspaces: ws,
		Scanner: stubScanner{findings: []reviewengine.Finding{{
			Severity: "high", Category: "security", File: "go.mod", Line: 5,
			Package: &reviewengine.PackageIdentity{
				Ecosystem: "go", Name: "example.com/foo", InstalledVersion: "1.0.0",
				FixedVersion: "1.0.1", Advisories: []string{"CVE-2024-9999"},
			},
		}}},
		Updater:    updater,
		Resolver:   stubResolver{res: Resolution{TargetVersion: "v1.0.1", Status: StatusResolved, Source: "registry"}},
		Publisher:    pub,
		Monitoring:   stubMonitoring{ok: true},
		Repositories: stubRepositories{ids: []string{"abc123def:app"}},
		Logger:       logger,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, store, "abc123def:app"
}

func TestScanRepositoryPublishesRootUpgradePatch(t *testing.T) {
	ctx := context.Background()
	updater := &stubUpdater{resp: &deprunner.UpdateResponse{
		Status: deprunner.StatusOK,
		ChangedFiles: []deprunner.ManifestFile{
			{Path: "go.mod", Content: "module example.com/app\n\ngo 1.22\n\nrequire example.com/foo v1.0.1\n"},
			{Path: "go.sum", Content: "example.com/foo v1.0.1 h1:deadbeef\n"},
		},
	}}
	relay := &captureRelay{}
	svc, store, repoID := newTestService(t, ctx, updater, relay)

	if err := svc.ScanRepository(ctx, repoID, TriggerManual); err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}

	if len(relay.events) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(relay.events))
	}
	evt := relay.events[0]
	if evt.Kind != nostr.KindPatch {
		t.Fatalf("event kind = %d, want %d", evt.Kind, nostr.KindPatch)
	}
	// A root patch carries no thread ("e") tags.
	for _, tag := range evt.Tags {
		if len(tag) >= 1 && tag[0] == "e" {
			t.Fatalf("root upgrade patch must not carry an e tag: %v", tag)
		}
	}
	assertTag(t, evt, "t", eventkind.DependencyUpgradeTagValue)
	assertTag(t, evt, "a", "30617:"+repoID)
	assertTag(t, evt, "package", "example.com/foo")
	assertTag(t, evt, "from_version", "1.0.0")
	assertTag(t, evt, "to_version", "v1.0.1")

	rows, err := store.GetOpenDependencyUpgrades(ctx, repoID)
	if err != nil {
		t.Fatalf("get open upgrades: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 open upgrade row, got %d", len(rows))
	}
	if rows[0].PatchEventID != evt.ID.Hex() {
		t.Fatalf("row patch event id = %q, want %q", rows[0].PatchEventID, evt.ID.Hex())
	}
	if rows[0].Status != db.DependencyUpgradeOpen {
		t.Fatalf("row status = %q, want open (recording a patch event must not change status)", rows[0].Status)
	}

	// Re-run: idempotent. No second publish, no second row, same event id.
	if err := svc.ScanRepository(ctx, repoID, TriggerManual); err != nil {
		t.Fatalf("second ScanRepository: %v", err)
	}
	if len(relay.events) != 1 {
		t.Fatalf("re-run republished: got %d events", len(relay.events))
	}
	rows, err = store.GetOpenDependencyUpgrades(ctx, repoID)
	if err != nil {
		t.Fatalf("get open upgrades after re-run: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("re-run created extra rows: got %d", len(rows))
	}
}

func TestScanRepositoryUnmonitoredSkips(t *testing.T) {
	ctx := context.Background()
	relay := &captureRelay{}
	svc, _, repoID := newTestService(t, ctx, &stubUpdater{}, relay)
	// Flip the gate closed.
	svc.deps.Monitoring = stubMonitoring{ok: false}
	if err := svc.ScanRepository(ctx, repoID, TriggerManual); err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if len(relay.events) != 0 {
		t.Fatalf("unmonitored repo produced %d events", len(relay.events))
	}
}

func TestScanRepositoryNoFixRecordsRow(t *testing.T) {
	ctx := context.Background()
	relay := &captureRelay{}
	svc, store, repoID := newTestService(t, ctx, &stubUpdater{}, relay)
	svc.deps.Resolver = stubResolver{res: Resolution{Status: StatusNoFix, Source: "none"}}

	if err := svc.ScanRepository(ctx, repoID, TriggerManual); err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if len(relay.events) != 0 {
		t.Fatalf("no-fix candidate should not publish, got %d events", len(relay.events))
	}
	row, ok, err := store.GetDependencyUpgradeByKey(ctx, repoID, "go", "example.com/foo", "1.0.0", "")
	if err != nil || !ok {
		t.Fatalf("expected no_fix row, ok=%v err=%v", ok, err)
	}
	if row.Status != db.DependencyUpgradeNoFixAvailable {
		t.Fatalf("row status = %q, want no_fix_available", row.Status)
	}
}

func assertTag(t *testing.T, evt nostr.Event, key, want string) {
	t.Helper()
	for _, tag := range evt.Tags {
		if len(tag) >= 2 && tag[0] == key {
			if tag[1] == want {
				return
			}
			t.Fatalf("tag %q = %q, want %q", key, tag[1], want)
		}
	}
	t.Fatalf("missing tag %q (want %q)", key, want)
}
