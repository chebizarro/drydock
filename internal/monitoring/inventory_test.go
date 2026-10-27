package monitoring

import (
	"errors"
	"strings"
	"testing"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/db"

	"fiatjaf.com/nostr"
)

func TestBuildInventoryClassifiesOperatorListRepositories(t *testing.T) {
	operator := nostr.Generate()
	h := newHarness(t, Config{Operator: nostr.GetPublicKey(operator).Hex()})
	ownerA, ownerB, ownerC, ownerD, ownerE, maintainerD, stranger := nostr.Generate(), nostr.Generate(), nostr.Generate(), nostr.Generate(), nostr.Generate(), nostr.Generate(), nostr.Generate()
	repoA := repositoryAddress(ownerA, "a") // covered by owner list naming drydock
	repoB := repositoryAddress(ownerB, "b") // covered by active enrollment
	repoC := repositoryAddress(ownerC, "c") // no announcement
	repoD := repositoryAddress(ownerD, "d") // maintainer list exists but does not name drydock
	repoE := repositoryAddress(ownerE, "e") // announcement, no opt-in; stranger's list must not count
	h.announce(ownerA, "a", 10)
	h.announce(ownerB, "b", 10)
	h.announce(ownerD, "d", 10, maintainerD)
	h.announce(ownerE, "e", 10)

	h.apply(h.list(operator, 100, false, repoA, repoB, repoC, repoD, repoE))
	h.apply(h.list(ownerA, 100, true, repoA))
	h.apply(h.list(maintainerD, 100, false, repoD))
	h.apply(h.list(stranger, 100, true, repoE))
	if _, err := h.store.ApplyMonitoringEnrollment(h.ctx, db.MonitoringEnrollment{
		RepositoryAddress: repoB, AuthorPubkey: nostr.GetPublicKey(ownerB).Hex(), RequestEventID: "e-b",
		RequestCreatedAt: 100, AnnouncementEventID: "ann-b", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	inv, err := BuildInventory(h.ctx, h.store, nostr.GetPublicKey(operator).Hex(), h.drydock.Hex(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !inv.OperatorListSeen || inv.OperatorListDeleted || inv.Monitored != 2 || inv.Dropped != 3 || len(inv.Entries) != 5 {
		t.Fatalf("inventory summary = %+v", inv)
	}
	byRepo := map[string]InventoryEntry{}
	for _, e := range inv.Entries {
		byRepo[e.Repository] = e
	}
	if e := byRepo[repoA]; e.Verdict != "monitored" || len(e.AuthorizedLists) != 1 {
		t.Fatalf("A = %+v", e)
	}
	if e := byRepo[repoB]; e.Verdict != "monitored" || len(e.ActiveEnrollments) != 1 {
		t.Fatalf("B = %+v", e)
	}
	if e := byRepo[repoC]; e.Verdict != "dropped" || e.Reason != "no_announcement" || e.AnnouncementKnown {
		t.Fatalf("C = %+v", e)
	}
	if e := byRepo[repoD]; e.Verdict != "dropped" || e.Reason != "maintainer_list_does_not_name_drydock" || len(e.UnaddressedLists) != 1 {
		t.Fatalf("D = %+v", e)
	}
	if e := byRepo[repoE]; e.Verdict != "dropped" || e.Reason != "no_maintainer_optin" || len(e.AuthorizedLists) != 0 {
		t.Fatalf("E = %+v (stranger's list must not count)", e)
	}
	// Deterministic: entries sorted by address, text stable across runs.
	for i := 1; i < len(inv.Entries); i++ {
		if inv.Entries[i-1].Repository > inv.Entries[i].Repository {
			t.Fatal("entries not sorted")
		}
	}
	again, _ := BuildInventory(h.ctx, h.store, nostr.GetPublicKey(operator).Hex(), h.drydock.Hex(), time.Unix(inv.GeneratedAt, 0))
	if again.Text() != inv.Text() {
		t.Fatal("inventory text not deterministic")
	}
	if !strings.Contains(inv.Text(), "dropped   "+repoC+" reason=no_announcement") {
		t.Fatalf("text = %s", inv.Text())
	}
}

func TestBuildInventoryWithoutOperatorListOrInvalidKeys(t *testing.T) {
	h := newHarness(t, Config{})
	operator := nostr.GetPublicKey(nostr.Generate())
	inv, err := BuildInventory(h.ctx, h.store, operator.Hex(), h.drydock.Hex(), time.Now())
	if err != nil || inv.OperatorListSeen || len(inv.Entries) != 0 {
		t.Fatalf("empty inventory = %+v err=%v", inv, err)
	}
	if _, err := BuildInventory(h.ctx, h.store, "junk", h.drydock.Hex(), time.Now()); err == nil {
		t.Fatal("invalid operator accepted")
	}
}

func TestSetOperatorStateFailsClosedUntilListSeen(t *testing.T) {
	h := newHarness(t, Config{})
	owner, opA := nostr.Generate(), nostr.Generate()
	repo := repositoryAddress(owner, "svc")
	h.announce(owner, "svc", 10)
	h.apply(h.list(owner, 100, true, repo))
	if !h.registry.Contains(repo) || !h.registry.Snapshot().CeilingSeen {
		t.Fatal("precondition")
	}
	h.registry.SetOperatorState(nostr.GetPublicKey(opA), nostr.ZeroPK, true)
	snap := h.registry.Snapshot()
	if snap.Initialized || snap.CeilingSeen || !snap.Loaded || h.registry.Contains(repo) || snap.Operator != nostr.GetPublicKey(opA).Hex() || snap.PreviousOperator != "" || h.registry.MonitoredCount() != 0 {
		t.Fatalf("after set: %+v", snap)
	}
	cur, prev, hasPrev := h.registry.OperatorState()
	if cur != nostr.GetPublicKey(opA) || prev != nostr.ZeroPK || !hasPrev {
		t.Fatalf("operator state = %v %v %v", cur, prev, hasPrev)
	}
	// Operator list arrives: ceiling seen, membership restored.
	h.apply(h.list(opA, 100, false, repo))
	if !h.registry.Contains(repo) || !h.registry.Snapshot().CeilingSeen || h.registry.MonitoredCount() != 1 {
		t.Fatalf("after operator list: %+v", h.registry.Snapshot())
	}
	// Swap back to "no ceiling" (what a rollback publishes).
	h.registry.SetOperatorState(nostr.ZeroPK, nostr.GetPublicKey(opA), true)
	snap = h.registry.Snapshot()
	if snap.Operator != "" || snap.PreviousOperator != nostr.GetPublicKey(opA).Hex() || !h.registry.Contains(repo) {
		t.Fatalf("after clear: %+v", snap)
	}
	// Hydration from Config.
	hydrated, err := NewRegistry(h.store, Config{Drydock: h.drydock.Hex(), Operator: nostr.GetPublicKey(opA).Hex(), PreviousOperator: "", HasPreviousOperator: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, prev, hasPrev := hydrated.OperatorState(); prev != nostr.ZeroPK || !hasPrev {
		t.Fatal("previous-operator history not hydrated")
	}
}

func TestBuildInventoryUsesRegistrySemanticsForLists(t *testing.T) {
	operator := nostr.Generate()
	h := newHarness(t, Config{Operator: nostr.GetPublicKey(operator).Hex()})
	ownerA, ownerB := nostr.Generate(), nostr.Generate()
	repoA, repoB := repositoryAddress(ownerA, "a"), repositoryAddress(ownerB, "b")
	h.announce(ownerA, "a", 10)
	h.announce(ownerB, "b", 10)
	// The operator's own list names drydock and the operator maintains A: a
	// valid opt-in the registry honours, so inventory must too.
	h.announce(ownerA, "a", 20, operator)
	h.apply(h.list(operator, 100, true, repoA, repoB))
	// Owner B's newest list is malformed: the registry suspends B; inventory
	// must not count the valid-looking entry.
	h.apply(h.list(ownerB, 100, true, repoB))
	bad := h.list(ownerB, 200, true, repoB)
	bad.Tags = append(bad.Tags, nostr.Tag{"p", "not-hex"})
	if err := bad.Sign(ownerB); err != nil {
		t.Fatal(err)
	}
	if applied, err := h.registry.ApplyList(h.ctx, bad); !applied || !errors.Is(err, ErrMalformedList) {
		t.Fatalf("malformed apply = %v %v", applied, err)
	}
	inv, err := BuildInventory(h.ctx, h.store, nostr.GetPublicKey(operator).Hex(), h.drydock.Hex(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	byRepo := map[string]InventoryEntry{}
	for _, e := range inv.Entries {
		byRepo[e.Repository] = e
	}
	if e := byRepo[repoA]; e.Verdict != "monitored" || len(e.AuthorizedLists) != 1 || e.AuthorizedLists[0] != nostr.GetPublicKey(operator).Hex() {
		t.Fatalf("operator opt-in not honoured: %+v", e)
	}
	if e := byRepo[repoB]; e.Verdict != "dropped" || e.Reason != "no_maintainer_optin" {
		t.Fatalf("malformed list counted: %+v", e)
	}
	if h.registry.Contains(repoA) != true || h.registry.Contains(repoB) != false {
		t.Fatal("inventory and registry disagree")
	}
}
