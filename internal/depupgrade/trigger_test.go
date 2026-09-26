package depupgrade

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/deprunner"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
	"git.sharegap.net/cascadia/drydock/internal/publisher"
)

// fakeStore implements the depupgrade.Store interface with in-memory canned data
// so trigger and reconciliation logic is tested without the DB's status-author
// authorization machinery.
type fakeStore struct {
	open       []db.DependencyUpgrade
	statusByID map[string]int
	marks      map[int64]db.DependencyUpgradeStatus
}

func (f *fakeStore) InsertDependencyUpgrade(context.Context, db.DependencyUpgrade) (db.DependencyUpgradeResult, error) {
	return db.DependencyUpgradeResult{}, nil
}

func (f *fakeStore) SupersedeOpenDependencyUpgrades(context.Context, string, string, string, string, string) (int, error) {
	return 0, nil
}

func (f *fakeStore) MarkDependencyUpgradeStatus(_ context.Context, id int64, status db.DependencyUpgradeStatus, _ string) error {
	if f.marks == nil {
		f.marks = map[int64]db.DependencyUpgradeStatus{}
	}
	f.marks[id] = status
	return nil
}

func (f *fakeStore) RecordDependencyUpgradePatchEvent(context.Context, int64, string) error {
	return nil
}

func (f *fakeStore) GetOpenDependencyUpgrades(context.Context, string) ([]db.DependencyUpgrade, error) {
	return f.open, nil
}

func (f *fakeStore) GetRootStatus(_ context.Context, rootID, _ string) (int, string, int64, bool, error) {
	kind, ok := f.statusByID[rootID]
	if !ok {
		return 0, "", 0, false, nil
	}
	return kind, "status-evt", 0, true, nil
}

type stubPublisher struct{}

func (stubPublisher) PublishUpgradePatch(context.Context, publisher.PublishUpgradePatchInput) (publisher.PublishFixPatchResult, error) {
	return publisher.PublishFixPatchResult{Published: true, EventID: "evt"}, nil
}

func newTriggerService(t *testing.T, store Store, monitored bool, repoIDs []string) *Service {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := New(Config{QueueSize: 8}, Dependencies{
		Store:        store,
		Workspaces:   &stubWorkspaces{root: t.TempDir()},
		Scanner:      stubScanner{},
		Updater:      &stubUpdater{},
		Resolver:     stubResolver{},
		Publisher:    stubPublisher{},
		Monitoring:   stubMonitoring{ok: monitored},
		Repositories: stubRepositories{ids: repoIDs},
		Logger:       logger,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

func TestOnDefaultBranchHeadChangedEnqueuesForMonitored(t *testing.T) {
	svc := newTriggerService(t, &fakeStore{}, true, nil)
	if err := svc.OnDefaultBranchHeadChanged(context.Background(), "abc:app", "deadbeef"); err != nil {
		t.Fatalf("OnDefaultBranchHeadChanged: %v", err)
	}
	if len(svc.queue) != 1 {
		t.Fatalf("queue length = %d, want 1", len(svc.queue))
	}
	job := <-svc.queue
	if job.repoID != "abc:app" || job.trigger != TriggerDefaultBranch {
		t.Fatalf("job = %+v, want {abc:app default_branch}", job)
	}
}

func TestOnDefaultBranchHeadChangedSkipsUnmonitored(t *testing.T) {
	svc := newTriggerService(t, &fakeStore{}, false, nil)
	if err := svc.OnDefaultBranchHeadChanged(context.Background(), "abc:app", "deadbeef"); err != nil {
		t.Fatalf("OnDefaultBranchHeadChanged: %v", err)
	}
	if len(svc.queue) != 0 {
		t.Fatalf("queue length = %d, want 0 (unmonitored must not enqueue)", len(svc.queue))
	}
}

func TestReconcileMarksMergedAndRejected(t *testing.T) {
	fs := &fakeStore{
		open: []db.DependencyUpgrade{
			{ID: 1, PatchEventID: "root-merged"},
			{ID: 2, PatchEventID: "root-rejected"},
			{ID: 3, PatchEventID: "root-open"},
			{ID: 4, PatchEventID: ""},
		},
		statusByID: map[string]int{
			"root-merged":   int(eventkind.StatusApplied),
			"root-rejected": int(eventkind.StatusClosed),
			"root-open":     int(eventkind.StatusOpen),
		},
	}
	svc := newTriggerService(t, fs, true, nil)
	svc.reconcile(context.Background(), "abc:app")

	if fs.marks[1] != db.DependencyUpgradeMerged {
		t.Fatalf("applied root -> status = %q, want merged", fs.marks[1])
	}
	if fs.marks[2] != db.DependencyUpgradeRejected {
		t.Fatalf("closed root -> status = %q, want rejected", fs.marks[2])
	}
	if _, ok := fs.marks[3]; ok {
		t.Fatalf("open root status must not transition the upgrade")
	}
	if _, ok := fs.marks[4]; ok {
		t.Fatalf("unpublished upgrade must not be reconciled")
	}
}

func TestScanRepositoryScheduleTriggerGated(t *testing.T) {
	ctx := context.Background()
	updater := &stubUpdater{resp: &deprunner.UpdateResponse{Status: deprunner.StatusOK}}
	relay := &captureRelay{}
	// Default test config enables upgrades but omits triggers, so schedule defaults
	// off: a scheduled scan must be a no-op past config load.
	svc, _, repoID := newTestService(t, ctx, updater, relay)
	if err := svc.ScanRepository(ctx, repoID, TriggerSchedule); err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if updater.calls != 0 {
		t.Fatalf("schedule trigger not gated: updater called %d times", updater.calls)
	}
	if len(relay.events) != 0 {
		t.Fatalf("schedule trigger published %d events, want 0", len(relay.events))
	}
}

func TestScanRepositoryDefaultBranchTriggerRuns(t *testing.T) {
	ctx := context.Background()
	updater := &stubUpdater{resp: &deprunner.UpdateResponse{
		Status: deprunner.StatusOK,
		ChangedFiles: []deprunner.ManifestFile{
			{Path: "go.mod", Content: "module example.com/app\n\ngo 1.22\n\nrequire example.com/foo v1.0.1\n"},
			{Path: "go.sum", Content: "example.com/foo v1.0.1 h1:deadbeef\n"},
		},
	}}
	relay := &captureRelay{}
	svc, _, repoID := newTestService(t, ctx, updater, relay)
	// default_branch defaults enabled, so the reactive trigger proceeds to publish.
	if err := svc.ScanRepository(ctx, repoID, TriggerDefaultBranch); err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if len(relay.events) != 1 {
		t.Fatalf("default-branch trigger published %d events, want 1", len(relay.events))
	}
}

func TestRefuseNonLocalGoReplace(t *testing.T) {
	svc := newTriggerService(t, &fakeStore{}, true, nil)
	dir := t.TempDir()
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
	}

	// A module-path replacement is a code-fetch vector and must be refused.
	write("module example.com/app\n\ngo 1.22\n\nrequire example.com/foo v1.0.0\n\nreplace example.com/foo => evil.example/foo v0.0.1\n")
	if err := svc.refuseNonLocalGoReplace(dir, "go.mod"); !errors.Is(err, errNonLocalReplace) {
		t.Fatalf("non-local replace: err = %v, want errNonLocalReplace", err)
	}

	// A local filesystem replacement is legitimate and allowed.
	write("module example.com/app\n\ngo 1.22\n\nrequire example.com/foo v1.0.0\n\nreplace example.com/foo => ./vendored/foo\n")
	if err := svc.refuseNonLocalGoReplace(dir, "go.mod"); err != nil {
		t.Fatalf("local replace: unexpected err = %v", err)
	}

	// No replace directive at all is allowed.
	write("module example.com/app\n\ngo 1.22\n\nrequire example.com/foo v1.0.0\n")
	if err := svc.refuseNonLocalGoReplace(dir, "go.mod"); err != nil {
		t.Fatalf("no replace: unexpected err = %v", err)
	}

	// A missing go.mod is not an error here (the manifest gather reports it).
	if err := svc.refuseNonLocalGoReplace(t.TempDir(), "go.mod"); err != nil {
		t.Fatalf("missing go.mod: unexpected err = %v", err)
	}
}
