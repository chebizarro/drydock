package db

import (
	"context"
	"errors"
	"testing"
)

func sampleUpgrade() DependencyUpgrade {
	return DependencyUpgrade{
		RepoID:       "repo1",
		Ecosystem:    "go",
		PackageName:  "github.com/foo/bar",
		PURL:         "pkg:golang/github.com/foo/bar@1.0.0",
		FromVersion:  "1.0.0",
		ToVersion:    "1.0.1",
		ManifestPath: "go.mod",
		LockfilePath: "go.sum",
		Advisories:   []string{"GHSA-xxxx-yyyy-zzzz", "CVE-2026-0001"},
		Policy:       "next_patch",
	}
}

func mustInsertUpgrade(t *testing.T, ctx context.Context, store *Store, up DependencyUpgrade) DependencyUpgrade {
	t.Helper()
	res, err := store.InsertDependencyUpgrade(ctx, up)
	if err != nil {
		t.Fatalf("insert dependency upgrade: %v", err)
	}
	if res.Disposition != DependencyUpgradeCreated {
		t.Fatalf("insert disposition = %q, want created", res.Disposition)
	}
	return res.Upgrade
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDependencyUpgradeRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	created := mustInsertUpgrade(t, ctx, store, sampleUpgrade())
	if created.ID == 0 {
		t.Fatal("created upgrade has zero id")
	}
	if created.Status != DependencyUpgradeOpen {
		t.Fatalf("default status = %q, want open", created.Status)
	}

	got, found, err := store.GetDependencyUpgrade(ctx, created.ID)
	if err != nil || !found {
		t.Fatalf("get by id: found=%v err=%v", found, err)
	}
	if got.PackageName != "github.com/foo/bar" || got.FromVersion != "1.0.0" || got.ToVersion != "1.0.1" {
		t.Fatalf("identity round-trip mismatch: %+v", got)
	}
	if got.PURL == "" || got.ManifestPath != "go.mod" || got.LockfilePath != "go.sum" || got.Policy != "next_patch" {
		t.Fatalf("metadata round-trip mismatch: %+v", got)
	}
	if !equalStrings(got.Advisories, []string{"GHSA-xxxx-yyyy-zzzz", "CVE-2026-0001"}) {
		t.Fatalf("advisories round-trip mismatch: %v", got.Advisories)
	}

	byKey, found, err := store.GetDependencyUpgradeByKey(ctx, "repo1", "go", "github.com/foo/bar", "1.0.0", "1.0.1")
	if err != nil || !found {
		t.Fatalf("get by key: found=%v err=%v", found, err)
	}
	if byKey.ID != created.ID {
		t.Fatalf("get by key id = %d, want %d", byKey.ID, created.ID)
	}
}

// TestInsertDependencyUpgradeIdempotentPreservesStatus pins the core reason the
// table exists: a re-proposed identical upgrade short-circuits on the identity
// key and returns the existing row unchanged — a rejected proposal is NOT
// resurrected to 'open', so the trigger cannot republish the same patch.
func TestInsertDependencyUpgradeIdempotentPreservesStatus(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	created := mustInsertUpgrade(t, ctx, store, sampleUpgrade())
	if err := store.MarkDependencyUpgradeStatus(ctx, created.ID, DependencyUpgradeRejected, "operator closed"); err != nil {
		t.Fatalf("mark rejected: %v", err)
	}

	// Re-proposing the byte-identical upgrade must return the existing rejected
	// row, not a fresh open one.
	res, err := store.InsertDependencyUpgrade(ctx, sampleUpgrade())
	if err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	if res.Disposition != DependencyUpgradeExisting {
		t.Fatalf("re-insert disposition = %q, want existing", res.Disposition)
	}
	if res.Upgrade.ID != created.ID {
		t.Fatalf("re-insert id = %d, want existing %d", res.Upgrade.ID, created.ID)
	}
	if res.Upgrade.Status != DependencyUpgradeRejected {
		t.Fatalf("re-insert status = %q, want rejected (must not resurrect)", res.Upgrade.Status)
	}

	// A genuinely different target (dependency moved on to a newer fix) is a new
	// record with its own lifecycle — rejection of 1.0.1 does not block 1.0.2.
	moved := sampleUpgrade()
	moved.ToVersion = "1.0.2"
	movedRes, err := store.InsertDependencyUpgrade(ctx, moved)
	if err != nil {
		t.Fatalf("insert moved-on upgrade: %v", err)
	}
	if movedRes.Disposition != DependencyUpgradeCreated {
		t.Fatalf("moved-on disposition = %q, want created", movedRes.Disposition)
	}
	if movedRes.Upgrade.ID == created.ID {
		t.Fatal("moved-on upgrade reused the rejected row's id")
	}
}

func TestSupersedeOpenDependencyUpgrades(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	older := mustInsertUpgrade(t, ctx, store, sampleUpgrade()) // 1.0.0 -> 1.0.1
	newer := sampleUpgrade()
	newer.ToVersion = "1.0.2"
	kept := mustInsertUpgrade(t, ctx, store, newer) // 1.0.0 -> 1.0.2

	// A newer fix arrived: keep the 1.0.2 proposal, supersede the rest.
	n, err := store.SupersedeOpenDependencyUpgrades(ctx, "repo1", "go", "github.com/foo/bar", "1.0.0", "1.0.2")
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if n != 1 {
		t.Fatalf("superseded %d rows, want 1", n)
	}

	gotOlder, _, err := store.GetDependencyUpgrade(ctx, older.ID)
	if err != nil {
		t.Fatalf("get older: %v", err)
	}
	if gotOlder.Status != DependencyUpgradeSuperseded {
		t.Fatalf("older status = %q, want superseded", gotOlder.Status)
	}
	gotKept, _, err := store.GetDependencyUpgrade(ctx, kept.ID)
	if err != nil {
		t.Fatalf("get kept: %v", err)
	}
	if gotKept.Status != DependencyUpgradeOpen {
		t.Fatalf("kept status = %q, want open", gotKept.Status)
	}

	open, err := store.GetOpenDependencyUpgrades(ctx, "repo1")
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 1 || open[0].ID != kept.ID {
		t.Fatalf("open upgrades = %+v, want only kept id %d", open, kept.ID)
	}

	// Empty keep versions supersede every remaining open row for the package
	// (e.g. it is no longer vulnerable).
	n, err = store.SupersedeOpenDependencyUpgrades(ctx, "repo1", "go", "github.com/foo/bar", "", "")
	if err != nil {
		t.Fatalf("supersede all: %v", err)
	}
	if n != 1 {
		t.Fatalf("superseded %d rows on clear, want 1", n)
	}
	open, err = store.GetOpenDependencyUpgrades(ctx, "repo1")
	if err != nil {
		t.Fatalf("list open after clear: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("open upgrades after clear = %d, want 0", len(open))
	}
}

func TestGetOpenDependencyUpgradesFiltersStatusAndRepo(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	openUp := mustInsertUpgrade(t, ctx, store, sampleUpgrade())

	merged := sampleUpgrade()
	merged.PackageName = "github.com/foo/baz"
	mergedUp := mustInsertUpgrade(t, ctx, store, merged)
	if err := store.MarkDependencyUpgradeStatus(ctx, mergedUp.ID, DependencyUpgradeMerged, ""); err != nil {
		t.Fatalf("mark merged: %v", err)
	}

	otherRepo := sampleUpgrade()
	otherRepo.RepoID = "repo2"
	mustInsertUpgrade(t, ctx, store, otherRepo)

	open, err := store.GetOpenDependencyUpgrades(ctx, "repo1")
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 1 || open[0].ID != openUp.ID {
		t.Fatalf("open for repo1 = %+v, want only %d", open, openUp.ID)
	}
}

func TestDependencyUpgradeNoFixAvailable(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	noFix := sampleUpgrade()
	noFix.ToVersion = ""
	noFix.Status = DependencyUpgradeNoFixAvailable
	res, err := store.InsertDependencyUpgrade(ctx, noFix)
	if err != nil {
		t.Fatalf("insert no-fix: %v", err)
	}
	if res.Disposition != DependencyUpgradeCreated || res.Upgrade.Status != DependencyUpgradeNoFixAvailable {
		t.Fatalf("no-fix insert = %+v", res)
	}

	// It is recorded (empty to_version key) so a re-scan short-circuits.
	again, err := store.InsertDependencyUpgrade(ctx, noFix)
	if err != nil {
		t.Fatalf("re-insert no-fix: %v", err)
	}
	if again.Disposition != DependencyUpgradeExisting {
		t.Fatalf("re-insert no-fix disposition = %q, want existing", again.Disposition)
	}

	// Once a real fix appears, that is a distinct record.
	fixed := sampleUpgrade() // ToVersion 1.0.1
	fixedRes, err := store.InsertDependencyUpgrade(ctx, fixed)
	if err != nil {
		t.Fatalf("insert fixed: %v", err)
	}
	if fixedRes.Disposition != DependencyUpgradeCreated {
		t.Fatalf("fixed disposition = %q, want created", fixedRes.Disposition)
	}
}

func TestDependencyUpgradeStatusNotFound(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	if err := store.MarkDependencyUpgradeStatus(ctx, 9999, DependencyUpgradeMerged, ""); !errors.Is(err, ErrDependencyUpgradeNotFound) {
		t.Fatalf("mark missing status err = %v, want ErrDependencyUpgradeNotFound", err)
	}
	if err := store.RecordDependencyUpgradePatchEvent(ctx, 9999, "event1"); !errors.Is(err, ErrDependencyUpgradeNotFound) {
		t.Fatalf("record missing patch err = %v, want ErrDependencyUpgradeNotFound", err)
	}
	if err := store.MarkDependencyUpgradeStatus(ctx, 1, "bogus", ""); err == nil {
		t.Fatal("expected error for invalid status")
	}
}

func TestRecordDependencyUpgradePatchEvent(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	created := mustInsertUpgrade(t, ctx, store, sampleUpgrade())
	if err := store.RecordDependencyUpgradePatchEvent(ctx, created.ID, "root-event-id"); err != nil {
		t.Fatalf("record patch event: %v", err)
	}
	got, _, err := store.GetDependencyUpgrade(ctx, created.ID)
	if err != nil {
		t.Fatalf("get after record: %v", err)
	}
	if got.PatchEventID != "root-event-id" {
		t.Fatalf("patch_event_id = %q, want root-event-id", got.PatchEventID)
	}
	// Recording publication must not change the lifecycle state.
	if got.Status != DependencyUpgradeOpen {
		t.Fatalf("status after record = %q, want open (publication does not merge)", got.Status)
	}
}

// TestDependencyUpgradeOutboxReusesReservedEvent exercises the reserve-then-mark
// idempotency contract through the dependency-upgrade outbox: once an event is
// reserved for an upgrade, every later reserve returns the exact first event —
// same Nostr event ID — so a retried relay publish stays idempotent.
func TestDependencyUpgradeOutboxReusesReservedEvent(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// The outbox FKs to a real upgrade row.
	up := mustInsertUpgrade(t, ctx, store, sampleUpgrade())

	first := signedEvent(t, `{"patch":1}`)
	second := signedEvent(t, `{"patch":2}`)
	if first.ID == second.ID {
		t.Fatal("test fixtures must have distinct event IDs")
	}

	reserved, delivered, err := store.ReserveDependencyUpgradePublication(ctx, up.ID, first)
	if err != nil {
		t.Fatalf("reserve first: %v", err)
	}
	if delivered || reserved.ID != first.ID {
		t.Fatalf("reserve returned id=%s delivered=%v, want id=%s delivered=false", reserved.ID.Hex(), delivered, first.ID.Hex())
	}

	reReserved, _, err := store.ReserveDependencyUpgradePublication(ctx, up.ID, second)
	if err != nil {
		t.Fatalf("reserve second: %v", err)
	}
	if reReserved.ID != first.ID {
		t.Fatalf("reserve reused %s, want original %s (idempotency broken)", reReserved.ID.Hex(), first.ID.Hex())
	}

	got, delivered, found, err := store.GetDependencyUpgradePublication(ctx, up.ID)
	if err != nil || !found {
		t.Fatalf("get publication: found=%v err=%v", found, err)
	}
	if got.ID != first.ID || delivered {
		t.Fatalf("get returned id=%s delivered=%v, want id=%s delivered=false", got.ID.Hex(), delivered, first.ID.Hex())
	}

	if err := store.MarkDependencyUpgradePublicationDelivered(ctx, up.ID); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	got, delivered, found, err = store.GetDependencyUpgradePublication(ctx, up.ID)
	if err != nil || !found {
		t.Fatalf("get after deliver: found=%v err=%v", found, err)
	}
	if !delivered || got.ID != first.ID {
		t.Fatalf("after deliver id=%s delivered=%v, want id=%s delivered=true", got.ID.Hex(), delivered, first.ID.Hex())
	}
}

func TestDependencyUpgradeOutboxMarkDeliveredMissing(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	if err := store.MarkDependencyUpgradePublicationDelivered(ctx, 4242); err == nil {
		t.Fatal("expected error marking a nonexistent reservation delivered")
	}
}
