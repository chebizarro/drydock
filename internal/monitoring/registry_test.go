package monitoring

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"drydock/internal/db"

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
	return store
}

func newTestRegistry(t *testing.T) (*Registry, *db.Store, nostr.SecretKey) {
	t.Helper()
	store := openRegistryStore(t, filepath.Join(t.TempDir(), "monitoring.db"))
	operatorSK := nostr.Generate()
	operator := nostr.GetPublicKey(operatorSK)
	registry, err := NewRegistry(store, operator.Hex())
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return registry, store, operatorSK
}

func listEvent(t *testing.T, sk nostr.SecretKey, createdAt int64, content string, addresses ...string) nostr.Event {
	t.Helper()
	tags := nostr.Tags{{"d", ListIdentifier}}
	for _, address := range addresses {
		tags = append(tags, nostr.Tag{"a", address})
	}
	event := nostr.Event{
		Kind:      MonitoredListKind,
		CreatedAt: nostr.Timestamp(createdAt),
		Tags:      tags,
		Content:   content,
	}
	if err := event.Sign(sk); err != nil {
		t.Fatalf("sign list: %v", err)
	}
	return event
}

func deletionEvent(t *testing.T, sk nostr.SecretKey, createdAt int64, address string) nostr.Event {
	t.Helper()
	event := nostr.Event{
		Kind:      DeletionKind,
		CreatedAt: nostr.Timestamp(createdAt),
		Tags:      nostr.Tags{{"a", address}},
	}
	if err := event.Sign(sk); err != nil {
		t.Fatalf("sign deletion: %v", err)
	}
	return event
}

func repositoryAddress(sk nostr.SecretKey, identifier string) string {
	return "30617:" + nostr.GetPublicKey(sk).Hex() + ":" + identifier
}

func TestRegistryReplacementOrderingAndTieBreak(t *testing.T) {
	registry, _, operatorSK := newTestRegistry(t)
	ctx := context.Background()
	repoA := repositoryAddress(nostr.Generate(), "a")
	repoB := repositoryAddress(nostr.Generate(), "b")

	first := listEvent(t, operatorSK, 100, "first", repoA)
	if applied, err := registry.ApplyList(ctx, first); err != nil || !applied {
		t.Fatalf("apply first: applied=%v err=%v", applied, err)
	}
	older := listEvent(t, operatorSK, 99, "older", repoB)
	if applied, err := registry.ApplyList(ctx, older); err != nil || applied {
		t.Fatalf("apply older: applied=%v err=%v", applied, err)
	}
	if !registry.Contains(repoA) || registry.Contains(repoB) {
		t.Fatalf("older revision changed membership: %#v", registry.Snapshot())
	}

	a := listEvent(t, operatorSK, 101, "tie-a", repoA)
	b := listEvent(t, operatorSK, 101, "tie-b", repoB)
	lower, higher := a, b
	if lower.ID.Hex() > higher.ID.Hex() {
		lower, higher = higher, lower
	}
	if applied, err := registry.ApplyList(ctx, higher); err != nil || !applied {
		t.Fatalf("apply higher-id revision: applied=%v err=%v", applied, err)
	}
	if applied, err := registry.ApplyList(ctx, lower); err != nil || !applied {
		t.Fatalf("apply lower-id tie winner: applied=%v err=%v", applied, err)
	}
	if applied, err := registry.ApplyList(ctx, higher); err != nil || applied {
		t.Fatalf("reapply losing tie: applied=%v err=%v", applied, err)
	}
	if registry.Snapshot().RevisionID != lower.ID.Hex() {
		t.Fatalf("winner = %s, want %s", registry.Snapshot().RevisionID, lower.ID.Hex())
	}
}

func TestRegistryRejectsSpoofedOperator(t *testing.T) {
	registry, _, _ := newTestRegistry(t)
	spoof := listEvent(t, nostr.Generate(), 100, "spoof", repositoryAddress(nostr.Generate(), "repo"))
	applied, err := registry.ApplyList(context.Background(), spoof)
	if applied || !errors.Is(err, ErrUnauthorizedAuthor) {
		t.Fatalf("spoof result: applied=%v err=%v", applied, err)
	}
	if registry.Snapshot().Initialized {
		t.Fatal("spoofed event initialized registry")
	}
}

func TestRegistryMalformedReplacementLeavesWinnerUntouched(t *testing.T) {
	registry, _, operatorSK := newTestRegistry(t)
	ctx := context.Background()
	repo := repositoryAddress(nostr.Generate(), "repo")
	initial := listEvent(t, operatorSK, 100, "initial", repo)
	if applied, err := registry.ApplyList(ctx, initial); err != nil || !applied {
		t.Fatalf("apply initial: applied=%v err=%v", applied, err)
	}

	malformed := listEvent(t, operatorSK, 101, "malformed")
	malformed.Tags = append(malformed.Tags, nostr.Tag{"a", repo + ":extra"})
	if err := malformed.Sign(operatorSK); err != nil {
		t.Fatal(err)
	}
	if applied, err := registry.ApplyList(ctx, malformed); applied || !errors.Is(err, ErrMalformedList) {
		t.Fatalf("malformed result: applied=%v err=%v", applied, err)
	}
	snapshot := registry.Snapshot()
	if snapshot.RevisionID != initial.ID.Hex() || !registry.Contains(repo) {
		t.Fatalf("malformed replacement changed winner: %#v", snapshot)
	}
}

func TestRegistryEmptyAndDeletedListBehavior(t *testing.T) {
	registry, _, operatorSK := newTestRegistry(t)
	ctx := context.Background()
	repo := repositoryAddress(nostr.Generate(), "repo")
	initial := listEvent(t, operatorSK, 100, "initial", repo)
	if applied, err := registry.ApplyList(ctx, initial); err != nil || !applied {
		t.Fatalf("apply initial: applied=%v err=%v", applied, err)
	}

	empty := listEvent(t, operatorSK, 101, "empty")
	if applied, err := registry.ApplyList(ctx, empty); err != nil || !applied {
		t.Fatalf("apply empty: applied=%v err=%v", applied, err)
	}
	snapshot := registry.Snapshot()
	if !snapshot.Initialized || snapshot.Deleted || len(snapshot.Repositories) != 0 {
		t.Fatalf("empty list snapshot: %#v", snapshot)
	}

	recreated := listEvent(t, operatorSK, 102, "recreated", repo)
	if applied, err := registry.ApplyList(ctx, recreated); err != nil || !applied {
		t.Fatalf("recreate: applied=%v err=%v", applied, err)
	}
	deletion := deletionEvent(t, operatorSK, 103, registry.listAddress)
	if applied, err := registry.ApplyDeletion(ctx, deletion); err != nil || !applied {
		t.Fatalf("delete: applied=%v err=%v", applied, err)
	}
	snapshot = registry.Snapshot()
	if !snapshot.Initialized || !snapshot.Deleted || len(snapshot.Repositories) != 0 || registry.Contains(repo) {
		t.Fatalf("deleted snapshot: %#v", snapshot)
	}

	if applied, err := registry.ApplyList(ctx, listEvent(t, operatorSK, 102, "stale-resurrection", repo)); err != nil || applied {
		t.Fatalf("stale resurrection: applied=%v err=%v", applied, err)
	}
	if applied, err := registry.ApplyList(ctx, listEvent(t, operatorSK, 104, "new-recreation", repo)); err != nil || !applied {
		t.Fatalf("new recreation: applied=%v err=%v", applied, err)
	}
	if !registry.Contains(repo) || registry.Snapshot().Deleted {
		t.Fatalf("new list did not recreate membership: %#v", registry.Snapshot())
	}
}

func TestRegistryRejectsMalformedAndSpoofedDeletion(t *testing.T) {
	registry, _, operatorSK := newTestRegistry(t)
	ctx := context.Background()
	wrongTarget := deletionEvent(t, operatorSK, 100, "30001:deadbeef:wrong")
	if applied, err := registry.ApplyDeletion(ctx, wrongTarget); applied || !errors.Is(err, ErrMalformedDeletion) {
		t.Fatalf("wrong-target deletion: applied=%v err=%v", applied, err)
	}
	spoof := deletionEvent(t, nostr.Generate(), 101, registry.listAddress)
	if applied, err := registry.ApplyDeletion(ctx, spoof); applied || !errors.Is(err, ErrUnauthorizedAuthor) {
		t.Fatalf("spoofed deletion: applied=%v err=%v", applied, err)
	}
}

func TestRegistryLoadsPersistedWinnerAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "monitoring.db")
	operatorSK := nostr.Generate()
	operator := nostr.GetPublicKey(operatorSK)
	repo := repositoryAddress(nostr.Generate(), "repo")

	store := openRegistryStore(t, path)
	registry, err := NewRegistry(store, operator.Hex())
	if err != nil {
		t.Fatal(err)
	}
	event := listEvent(t, operatorSK, 100, "persisted", repo)
	if applied, err := registry.ApplyList(ctx, event); err != nil || !applied {
		t.Fatalf("apply: applied=%v err=%v", applied, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openRegistryStore(t, path)
	defer reopened.Close()
	restarted, err := NewRegistry(reopened, operator.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Snapshot().Initialized {
		t.Fatal("new registry should not be initialized before Load")
	}
	if err := restarted.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !restarted.Contains(repo) || restarted.Snapshot().RevisionID != event.ID.Hex() {
		t.Fatalf("restart snapshot: %#v", restarted.Snapshot())
	}

	copy := restarted.Snapshot()
	delete(copy.Repositories, repo)
	if !restarted.Contains(repo) {
		t.Fatal("Snapshot exposed mutable registry membership")
	}
}
