package monitoring

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
)

func openRegistryStore(t *testing.T, path string) *db.Store {
	t.Helper()
	store, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		_ = store.Close()
		t.Fatalf("migrate store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

type harness struct {
	t        *testing.T
	ctx      context.Context
	store    *db.Store
	registry *Registry
	drydock  nostr.PubKey
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	store := openRegistryStore(t, filepath.Join(t.TempDir(), "monitoring.db"))
	drydockSK := nostr.Generate()
	drydock := nostr.GetPublicKey(drydockSK)
	if cfg.Drydock == "" {
		cfg.Drydock = drydock.Hex()
	}
	registry, err := NewRegistry(store, cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return &harness{t: t, ctx: context.Background(), store: store, registry: registry, drydock: drydock}
}

// announce stores a kind-30617 announcement and lets the registry re-evaluate.
func (h *harness) announce(ownerSK nostr.SecretKey, identifier string, createdAt int64, maintainers ...nostr.SecretKey) nostr.Event {
	h.t.Helper()
	tags := nostr.Tags{{"d", identifier}}
	if len(maintainers) > 0 {
		tag := nostr.Tag{"maintainers"}
		for _, sk := range maintainers {
			tag = append(tag, nostr.GetPublicKey(sk).Hex())
		}
		tags = append(tags, tag)
	}
	event := nostr.Event{Kind: nostr.KindRepositoryAnnouncement, CreatedAt: nostr.Timestamp(createdAt), Tags: tags}
	if err := event.Sign(ownerSK); err != nil {
		h.t.Fatalf("sign announcement: %v", err)
	}
	if _, err := h.store.ApplyRepositoryAnnouncement(h.ctx, event); err != nil {
		h.t.Fatalf("store announcement: %v", err)
	}
	if err := h.registry.ApplyAnnouncement(h.ctx, event); err != nil {
		h.t.Fatalf("apply announcement: %v", err)
	}
	return event
}

func (h *harness) list(sk nostr.SecretKey, createdAt int64, nameDrydock bool, addresses ...string) nostr.Event {
	h.t.Helper()
	tags := nostr.Tags{{"d", ListIdentifier}}
	if nameDrydock {
		tags = append(tags, nostr.Tag{"p", h.drydock.Hex()})
	}
	for _, address := range addresses {
		tags = append(tags, nostr.Tag{"a", address})
	}
	event := nostr.Event{Kind: MonitoredListKind, CreatedAt: nostr.Timestamp(createdAt), Tags: tags}
	if err := event.Sign(sk); err != nil {
		h.t.Fatalf("sign list: %v", err)
	}
	return event
}

func (h *harness) apply(event nostr.Event) {
	h.t.Helper()
	applied, err := h.registry.ApplyList(h.ctx, event)
	if err != nil || !applied {
		h.t.Fatalf("apply list: applied=%v err=%v", applied, err)
	}
}

func deletionEvent(t *testing.T, sk nostr.SecretKey, createdAt int64, address string) nostr.Event {
	t.Helper()
	event := nostr.Event{Kind: DeletionKind, CreatedAt: nostr.Timestamp(createdAt), Tags: nostr.Tags{{"a", address}}}
	if err := event.Sign(sk); err != nil {
		t.Fatalf("sign deletion: %v", err)
	}
	return event
}

func repositoryAddress(sk nostr.SecretKey, identifier string) string {
	return "30617:" + nostr.GetPublicKey(sk).Hex() + ":" + identifier
}

func repositoryID(sk nostr.SecretKey, identifier string) string {
	return nostr.GetPublicKey(sk).Hex() + ":" + identifier
}

func TestNewRegistryRequiresServicePubkey(t *testing.T) {
	store := openRegistryStore(t, filepath.Join(t.TempDir(), "monitoring.db"))
	if _, err := NewRegistry(store, Config{}); err == nil {
		t.Fatal("expected error without a Drydock pubkey")
	}
	if _, err := NewRegistry(store, Config{Drydock: nostr.GetPublicKey(nostr.Generate()).Hex(), Operator: "nope"}); err == nil {
		t.Fatal("expected error for invalid operator")
	}
}

func TestOwnerEnrollmentFailsClosedUntilAnnouncementIsKnown(t *testing.T) {
	h := newHarness(t, Config{})
	owner := nostr.Generate()
	repo := repositoryAddress(owner, "svc")

	h.apply(h.list(owner, 100, true, repo))
	if h.registry.Contains(repo) {
		t.Fatal("repository monitored before any announcement was stored")
	}
	if !h.registry.Snapshot().Initialized {
		t.Fatal("registry without operator must be initialized after Load")
	}

	h.announce(owner, "svc", 50)
	if !h.registry.Contains(repo) {
		t.Fatal("owner's own list did not grant membership once the announcement was known")
	}
	if got := h.registry.MonitoredRepositoryIDs(); len(got) != 1 || got[0] != repositoryID(owner, "svc") {
		t.Fatalf("MonitoredRepositoryIDs = %v", got)
	}
}

func TestMaintainerMayEnrollStrangerMayNot(t *testing.T) {
	h := newHarness(t, Config{})
	owner, maintainer, stranger := nostr.Generate(), nostr.Generate(), nostr.Generate()
	repo := repositoryAddress(owner, "svc")
	h.announce(owner, "svc", 50, maintainer)

	h.apply(h.list(stranger, 100, true, repo))
	if h.registry.Contains(repo) {
		t.Fatal("stranger's list granted membership")
	}
	if status := h.registry.Snapshot().Authors[nostr.GetPublicKey(stranger).Hex()].Status; status != StatusActive {
		t.Fatalf("stranger status = %q; list is valid, it just grants nothing", status)
	}

	h.apply(h.list(maintainer, 100, true, repo))
	if !h.registry.Contains(repo) {
		t.Fatal("maintainer's list did not grant membership")
	}
}

func TestListNotNamingDrydockIsWithdrawn(t *testing.T) {
	h := newHarness(t, Config{})
	owner := nostr.Generate()
	repo := repositoryAddress(owner, "svc")
	h.announce(owner, "svc", 50)

	h.apply(h.list(owner, 100, true, repo))
	if !h.registry.Contains(repo) {
		t.Fatal("precondition: enrolled")
	}
	h.apply(h.list(owner, 200, false, repo))
	snapshot := h.registry.Snapshot()
	if h.registry.Contains(repo) || snapshot.Authors[nostr.GetPublicKey(owner).Hex()].Status != StatusWithdrawn {
		t.Fatalf("list without p tag should withdraw: %#v", snapshot)
	}
}

func TestMultipleAuthorsUnionAndIndependentWithdrawal(t *testing.T) {
	h := newHarness(t, Config{})
	ownerA, ownerB, maintainer := nostr.Generate(), nostr.Generate(), nostr.Generate()
	repoA, repoB := repositoryAddress(ownerA, "a"), repositoryAddress(ownerB, "b")
	h.announce(ownerA, "a", 50)
	h.announce(ownerB, "b", 50, maintainer)

	h.apply(h.list(ownerA, 100, true, repoA))
	h.apply(h.list(maintainer, 100, true, repoB, repoA)) // repoA: maintainer has no authority there
	if !h.registry.Contains(repoA) || !h.registry.Contains(repoB) {
		t.Fatalf("union missing members: %#v", h.registry.Snapshot())
	}
	if got := h.registry.MonitoredRepositoryIDs(); len(got) != 2 {
		t.Fatalf("ids = %v", got)
	}

	h.apply(h.list(ownerA, 200, false))
	if h.registry.Contains(repoA) || !h.registry.Contains(repoB) {
		t.Fatalf("ownerA withdrawal affected the wrong repositories: %#v", h.registry.Snapshot())
	}
}

func TestAnnouncementReplacementWithdrawsMaintainerAuthority(t *testing.T) {
	h := newHarness(t, Config{})
	owner, maintainer := nostr.Generate(), nostr.Generate()
	repo := repositoryAddress(owner, "svc")
	h.announce(owner, "svc", 50, maintainer)
	h.apply(h.list(maintainer, 100, true, repo))
	if !h.registry.Contains(repo) {
		t.Fatal("precondition: maintainer enrolled")
	}

	// Owner drops the maintainer.
	h.announce(owner, "svc", 60)
	if h.registry.Contains(repo) {
		t.Fatal("removed maintainer still grants membership")
	}

	// A stale replay of the older announcement must not restore authority.
	h.announce(owner, "svc", 55, maintainer)
	if h.registry.Contains(repo) {
		t.Fatal("stale announcement replay restored maintainer authority")
	}

	// Owner re-adds the maintainer in a newer revision.
	h.announce(owner, "svc", 70, maintainer)
	if !h.registry.Contains(repo) {
		t.Fatal("re-added maintainer did not regain membership")
	}
}

func TestAnnouncementFromNonOwnerGrantsNothing(t *testing.T) {
	h := newHarness(t, Config{})
	owner, impostor := nostr.Generate(), nostr.Generate()
	repo := repositoryAddress(owner, "svc")
	// The impostor announces a repo with the same identifier under its own key;
	// that is a different repository and must not authorize owner's address.
	h.announce(impostor, "svc", 50, owner)
	h.apply(h.list(owner, 100, true, repo))
	if h.registry.Contains(repo) {
		t.Fatal("announcement by a different owner authorized the repository")
	}
}

func TestAuthorityCacheEvictedWhenUndeclared(t *testing.T) {
	h := newHarness(t, Config{})
	owner, maintainer := nostr.Generate(), nostr.Generate()
	repo := repositoryAddress(owner, "svc")
	h.announce(owner, "svc", 50, maintainer)
	h.apply(h.list(maintainer, 100, true, repo))
	// Drop the declaration, change authority while undeclared, redeclare.
	h.apply(h.list(maintainer, 200, true))
	h.announce(owner, "svc", 60)
	h.apply(h.list(maintainer, 300, true, repo))
	if h.registry.Contains(repo) {
		t.Fatal("stale cached authority was reused after the repository was redeclared")
	}
}

func TestReplacementOrderingAndTieBreakPerAuthor(t *testing.T) {
	h := newHarness(t, Config{})
	owner := nostr.Generate()
	repoA, repoB := repositoryAddress(owner, "a"), repositoryAddress(owner, "b")
	h.announce(owner, "a", 10)
	h.announce(owner, "b", 10)

	h.apply(h.list(owner, 200, true, repoA))
	if applied, err := h.registry.ApplyList(h.ctx, h.list(owner, 100, true, repoB)); err != nil || applied {
		t.Fatalf("older revision: applied=%v err=%v", applied, err)
	}
	if !h.registry.Contains(repoA) || h.registry.Contains(repoB) {
		t.Fatalf("older revision changed membership: %#v", h.registry.Snapshot())
	}

	first := h.list(owner, 300, true, repoA)
	second := h.list(owner, 300, true, repoB)
	lower, higher := first, second
	if higher.ID.Hex() < lower.ID.Hex() {
		lower, higher = second, first
	}
	h.apply(higher)
	h.apply(lower)
	if applied, _ := h.registry.ApplyList(h.ctx, higher); applied {
		t.Fatal("losing tie was re-applied")
	}
	if got := h.registry.Snapshot().Authors[nostr.GetPublicKey(owner).Hex()].RevisionID; got != lower.ID.Hex() {
		t.Fatalf("winner = %s, want %s", got, lower.ID.Hex())
	}
}

func TestMalformedNewerRevisionSuspendsAuthor(t *testing.T) {
	h := newHarness(t, Config{})
	owner := nostr.Generate()
	repo := repositoryAddress(owner, "svc")
	h.announce(owner, "svc", 10)
	h.apply(h.list(owner, 100, true, repo))

	bad := h.list(owner, 200, true, repo)
	bad.Tags = append(bad.Tags, nostr.Tag{"a", "not-an-address"})
	if err := bad.Sign(owner); err != nil {
		t.Fatal(err)
	}
	applied, err := h.registry.ApplyList(h.ctx, bad)
	if !applied || !errors.Is(err, ErrMalformedList) {
		t.Fatalf("malformed newer revision: applied=%v err=%v", applied, err)
	}
	snapshot := h.registry.Snapshot()
	if h.registry.Contains(repo) || snapshot.Authors[nostr.GetPublicKey(owner).Hex()].Status != StatusMalformed {
		t.Fatalf("malformed revision did not suspend the author: %#v", snapshot)
	}

	wrongD := h.list(owner, 300, true, repo)
	wrongD.Tags[0] = nostr.Tag{"d", "other"}
	if err := wrongD.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if applied, err := h.registry.ApplyList(h.ctx, wrongD); applied || !errors.Is(err, ErrMalformedList) {
		t.Fatalf("wrong d tag must be rejected without state: applied=%v err=%v", applied, err)
	}
}

func TestDeletionOnlyOfOwnList(t *testing.T) {
	h := newHarness(t, Config{})
	owner, other := nostr.Generate(), nostr.Generate()
	repo := repositoryAddress(owner, "svc")
	h.announce(owner, "svc", 10)
	h.apply(h.list(owner, 100, true, repo))

	ownerAddress := ListAddress(nostr.GetPublicKey(owner).Hex())
	if applied, err := h.registry.ApplyDeletion(h.ctx, deletionEvent(t, other, 200, ownerAddress)); applied || !errors.Is(err, ErrUnauthorizedAuthor) {
		t.Fatalf("foreign deletion: applied=%v err=%v", applied, err)
	}
	if applied, err := h.registry.ApplyDeletion(h.ctx, deletionEvent(t, owner, 200, "30001:x:y")); applied || !errors.Is(err, ErrMalformedDeletion) {
		t.Fatalf("untargeted deletion: applied=%v err=%v", applied, err)
	}
	if !h.registry.Contains(repo) {
		t.Fatal("rejected deletions changed membership")
	}

	if applied, err := h.registry.ApplyDeletion(h.ctx, deletionEvent(t, owner, 200, ownerAddress)); err != nil || !applied {
		t.Fatalf("own deletion: applied=%v err=%v", applied, err)
	}
	if h.registry.Contains(repo) || h.registry.Snapshot().Authors[nostr.GetPublicKey(owner).Hex()].Status != StatusDeleted {
		t.Fatalf("deletion did not remove membership: %#v", h.registry.Snapshot())
	}
	if applied, err := h.registry.ApplyList(h.ctx, h.list(owner, 150, true, repo)); applied || err != nil {
		t.Fatalf("stale resurrection: applied=%v err=%v", applied, err)
	}
	h.apply(h.list(owner, 300, true, repo))
	if !h.registry.Contains(repo) {
		t.Fatal("newer list did not recreate membership after deletion")
	}
}

func TestOperatorListIsCeilingNotGrant(t *testing.T) {
	operator := nostr.Generate()
	h := newHarness(t, Config{Operator: nostr.GetPublicKey(operator).Hex()})
	owner := nostr.Generate()
	repoA, repoB, repoC := repositoryAddress(owner, "a"), repositoryAddress(owner, "b"), repositoryAddress(owner, "c")
	h.announce(owner, "a", 10)
	h.announce(owner, "b", 10)
	h.announce(owner, "c", 10)
	h.apply(h.list(owner, 100, true, repoA, repoB))

	if h.registry.Snapshot().Initialized || h.registry.Contains(repoA) {
		t.Fatal("must fail closed until the operator ceiling has been seen")
	}

	// Operator names A and C: A is monitored (maintainer enrolled + ceiling),
	// B is cut by the ceiling, C is not enrolled by any maintainer.
	h.apply(h.list(operator, 100, false, repoA, repoC))
	snapshot := h.registry.Snapshot()
	if !snapshot.Initialized || !h.registry.Contains(repoA) || h.registry.Contains(repoB) || h.registry.Contains(repoC) {
		t.Fatalf("operator ceiling misapplied: %#v", snapshot)
	}

	// Operator switches monitoring off by deleting the list; still initialized.
	if applied, err := h.registry.ApplyDeletion(h.ctx, deletionEvent(t, operator, 200, ListAddress(nostr.GetPublicKey(operator).Hex()))); err != nil || !applied {
		t.Fatalf("operator deletion: applied=%v err=%v", applied, err)
	}
	snapshot = h.registry.Snapshot()
	if !snapshot.Initialized || len(snapshot.Repositories) != 0 {
		t.Fatalf("deleted operator list should monitor nothing: %#v", snapshot)
	}
}

func TestStaticScopeIsCeiling(t *testing.T) {
	owner := nostr.Generate()
	h := newHarness(t, Config{Scope: scope.NewMatcher([]string{repositoryID(owner, "a")}, nil)})
	repoA, repoB := repositoryAddress(owner, "a"), repositoryAddress(owner, "b")
	h.announce(owner, "a", 10)
	h.announce(owner, "b", 10)
	h.apply(h.list(owner, 100, true, repoA, repoB))
	if !h.registry.Contains(repoA) || h.registry.Contains(repoB) {
		t.Fatalf("static allowlist ceiling misapplied: %#v", h.registry.Snapshot())
	}
}

func TestRestartRecoveryAcrossAuthorsAndServiceIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitoring.db")
	store := openRegistryStore(t, path)
	drydock := nostr.GetPublicKey(nostr.Generate())
	registry, err := NewRegistry(store, Config{Drydock: drydock.Hex()})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ctx: context.Background(), store: store, registry: registry, drydock: drydock}
	ownerA, ownerB := nostr.Generate(), nostr.Generate()
	repoA, repoB := repositoryAddress(ownerA, "a"), repositoryAddress(ownerB, "b")
	h.announce(ownerA, "a", 10)
	h.announce(ownerB, "b", 10)
	h.apply(h.list(ownerA, 100, true, repoA))
	h.apply(h.list(ownerB, 100, true, repoB))

	restarted, err := NewRegistry(store, Config{Drydock: drydock.Hex()})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := restarted.Snapshot()
	if !snapshot.Initialized || !restarted.Contains(repoA) || !restarted.Contains(repoB) || len(snapshot.Authors) != 2 {
		t.Fatalf("restart lost state: %#v", snapshot)
	}

	// Same database, different service identity: persisted lists no longer
	// name us, so they are withdrawn rather than trusted.
	rekeyed, err := NewRegistry(store, Config{Drydock: nostr.GetPublicKey(nostr.Generate()).Hex()})
	if err != nil {
		t.Fatal(err)
	}
	if err := rekeyed.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot = rekeyed.Snapshot()
	if rekeyed.Contains(repoA) || snapshot.Authors[nostr.GetPublicKey(ownerA).Hex()].Status != StatusWithdrawn {
		t.Fatalf("lists addressed to a previous service identity survived: %#v", snapshot)
	}
}

func TestChangesSignalAndDeclaredRepositories(t *testing.T) {
	h := newHarness(t, Config{})
	drain := func() {
		for {
			select {
			case <-h.registry.Changes():
			default:
				return
			}
		}
	}
	drain()
	owner, stranger := nostr.Generate(), nostr.Generate()
	repoA, repoB := repositoryAddress(owner, "a"), repositoryAddress(stranger, "b")
	h.announce(owner, "a", 10)

	// A stranger's list declares b: not monitored, but declared, so routing can
	// recover its announcement.
	h.apply(h.list(stranger, 100, true, repoB))
	select {
	case <-h.registry.Changes():
	default:
		t.Fatal("ApplyList did not signal a change")
	}
	declared := h.registry.DeclaredRepositories()
	if len(declared) != 1 || declared[0].Address != repoB || h.registry.Contains(repoB) {
		t.Fatalf("declared = %+v contains=%v", declared, h.registry.Contains(repoB))
	}

	h.apply(h.list(owner, 100, true, repoA))
	drain()
	if got := h.registry.DeclaredRepositories(); len(got) != 2 {
		t.Fatalf("declared after second list = %d", len(got))
	}
	// Announcement replacement for a declared repo also signals (relays may change).
	h.announce(owner, "a", 20)
	select {
	case <-h.registry.Changes():
	default:
		t.Fatal("ApplyAnnouncement did not signal a change")
	}
	// Withdrawn lists no longer declare.
	h.apply(h.list(stranger, 200, false, repoB))
	if got := h.registry.DeclaredRepositories(); len(got) != 1 || got[0].Address != repoA {
		t.Fatalf("declared after withdrawal = %+v", got)
	}
}

func TestDeclaredRepositoriesApplyCeilingsAndBudget(t *testing.T) {
	operator := nostr.Generate()
	h := newHarness(t, Config{Operator: nostr.GetPublicKey(operator).Hex()})
	stranger, owner := nostr.Generate(), nostr.Generate()
	inCeiling := repositoryAddress(owner, "in")
	outside := []string{repositoryAddress(owner, "x"), repositoryAddress(owner, "y"), repositoryAddress(owner, "z")}
	h.apply(h.list(stranger, 100, true, append([]string{inCeiling}, outside...)...))
	if got := h.registry.DeclaredRepositories(); len(got) != 0 {
		t.Fatalf("declared before the operator ceiling was seen = %d, want 0", len(got))
	}
	h.apply(h.list(operator, 100, false, inCeiling))
	if got := h.registry.DeclaredRepositories(); len(got) != 1 || got[0].Address != inCeiling {
		t.Fatalf("declared must be cut by the operator ceiling: %+v", got)
	}

	// Budget: shrink it and check deterministic truncation.
	old := maxDeclaredRepositories
	maxDeclaredRepositories = 2
	t.Cleanup(func() { maxDeclaredRepositories = old })
	h.apply(h.list(operator, 200, false, append([]string{inCeiling}, outside...)...))
	got := h.registry.DeclaredRepositories()
	if len(got) != 2 {
		t.Fatalf("declared = %d, want budget of 2", len(got))
	}
	if got[0].Address > got[1].Address {
		t.Fatalf("declared not sorted: %+v", got)
	}
}

func TestRoutedAuthorsPreferGrantingAuthorsWithinBudget(t *testing.T) {
	h := newHarness(t, Config{})
	owner, strangerA, strangerB := nostr.Generate(), nostr.Generate(), nostr.Generate()
	repo := repositoryAddress(owner, "svc")
	h.announce(owner, "svc", 10)
	h.apply(h.list(strangerA, 100, true, repo))
	h.apply(h.list(strangerB, 100, true, repo))
	h.apply(h.list(owner, 100, true, repo))
	authors := h.registry.RoutedAuthors()
	if len(authors) != 3 || authors[0] != nostr.GetPublicKey(owner).Hex() {
		t.Fatalf("granting author must rank first: %v", authors)
	}
	old := maxRoutedAuthors
	maxRoutedAuthors = 1
	t.Cleanup(func() { maxRoutedAuthors = old })
	if got := h.registry.RoutedAuthors(); len(got) != 1 || got[0] != nostr.GetPublicKey(owner).Hex() {
		t.Fatalf("budget must keep the granting author: %v", got)
	}
	// Withdrawn authors drop out entirely.
	h.apply(h.list(owner, 200, false))
	maxRoutedAuthors = old
	if got := h.registry.RoutedAuthors(); len(got) != 2 {
		t.Fatalf("withdrawn author still routed: %v", got)
	}
}

// failingStore lets one authority lookup fail to simulate a store error after
// the announcement itself was already committed by ingest.
type failingStore struct {
	Store
	fail bool
}

func (f *failingStore) LoadRepositoryAnnouncement(ctx context.Context, repoID string) (nostr.Event, bool, error) {
	if f.fail {
		return nostr.Event{}, false, errors.New("boom")
	}
	return f.Store.LoadRepositoryAnnouncement(ctx, repoID)
}

func TestAnnouncementRefreshFailureFailsClosed(t *testing.T) {
	store := openRegistryStore(t, filepath.Join(t.TempDir(), "monitoring.db"))
	wrapped := &failingStore{Store: store}
	drydock := nostr.GetPublicKey(nostr.Generate())
	registry, err := NewRegistry(wrapped, Config{Drydock: drydock.Hex()})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ctx: context.Background(), store: store, registry: registry, drydock: drydock}
	owner, maintainer := nostr.Generate(), nostr.Generate()
	repo := repositoryAddress(owner, "svc")
	h.announce(owner, "svc", 50, maintainer)
	h.apply(h.list(maintainer, 100, true, repo))
	if !registry.Contains(repo) {
		t.Fatal("precondition: enrolled")
	}

	// Owner removes the maintainer; the store commits it, then the registry's
	// re-read fails. Membership must not survive on the stale authority.
	revoked := nostr.Event{Kind: nostr.KindRepositoryAnnouncement, CreatedAt: 60, Tags: nostr.Tags{{"d", "svc"}}}
	if err := revoked.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyRepositoryAnnouncement(h.ctx, revoked); err != nil {
		t.Fatal(err)
	}
	wrapped.fail = true
	if err := registry.ApplyAnnouncement(h.ctx, revoked); err == nil {
		t.Fatal("expected lookup error to surface")
	}
	if registry.Contains(repo) {
		t.Fatal("revoked maintainer stayed enrolled after a failed authority refresh")
	}
	// Recovery: a successful redelivery re-derives from the stored winner.
	wrapped.fail = false
	if err := registry.ApplyAnnouncement(h.ctx, revoked); err != nil {
		t.Fatal(err)
	}
	if registry.Contains(repo) {
		t.Fatal("revoked maintainer regained membership")
	}
}

func TestAuthorityCacheIsBoundedByDeclaredRepositories(t *testing.T) {
	h := newHarness(t, Config{})
	author := nostr.Generate()
	for round := int64(0); round < 5; round++ {
		addresses := make([]string, 0, 50)
		for i := 0; i < 50; i++ {
			addresses = append(addresses, repositoryAddress(nostr.Generate(), "r"))
		}
		h.apply(h.list(author, 100+round, true, addresses...))
		h.registry.mu.Lock()
		size := len(h.registry.authorities)
		h.registry.mu.Unlock()
		if size != 50 {
			t.Fatalf("round %d: authority cache = %d entries, want 50 (only declared repositories)", round, size)
		}
	}
	h.apply(h.list(author, 200, true))
	h.registry.mu.Lock()
	size := len(h.registry.authorities)
	h.registry.mu.Unlock()
	if size != 0 {
		t.Fatalf("empty list left %d cached authorities", size)
	}
}
