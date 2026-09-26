package db

import (
	"context"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

func repoStateEvent(t *testing.T, pk nostr.PubKey, id, headCommit string, createdAt int64) nostr.Event {
	t.Helper()
	tags := nostr.Tags{
		{"d", id},
		{"HEAD", "ref: refs/heads/main"},
	}
	if headCommit != "" {
		tags = append(tags, nostr.Tag{"refs/heads/main", headCommit})
	}
	return nostr.Event{
		Kind:      30618,
		PubKey:    pk,
		CreatedAt: nostr.Timestamp(createdAt),
		Tags:      tags,
	}
}

func TestUpsertRepositorySnapshotDetectsHeadChange(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	pk, err := nostr.PubKeyFromHex(strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("pubkey: %v", err)
	}
	commitA := strings.Repeat("1", 40)
	commitB := strings.Repeat("2", 40)

	// First observation of a repository with a head is a change (scan once).
	got, err := store.UpsertRepositorySnapshot(ctx, repoStateEvent(t, pk, "app", commitA, 100))
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if !got.HeadChanged || got.NewHead != commitA {
		t.Fatalf("first upsert = %+v, want HeadChanged=true NewHead=%s", got, commitA)
	}
	if want := pk.Hex() + ":app"; got.RepoID != want {
		t.Fatalf("repo id = %q, want %q", got.RepoID, want)
	}

	// Newer snapshot moving the head is a change.
	got, err = store.UpsertRepositorySnapshot(ctx, repoStateEvent(t, pk, "app", commitB, 200))
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if !got.HeadChanged || got.NewHead != commitB {
		t.Fatalf("second upsert = %+v, want HeadChanged=true NewHead=%s", got, commitB)
	}

	// Newer snapshot with the same head is not a change.
	got, err = store.UpsertRepositorySnapshot(ctx, repoStateEvent(t, pk, "app", commitB, 300))
	if err != nil {
		t.Fatalf("third upsert: %v", err)
	}
	if got.HeadChanged {
		t.Fatalf("unchanged head reported as change: %+v", got)
	}

	// An out-of-order (older) snapshot is ignored by the created_at guard and
	// never reported as a change even though its head differs.
	got, err = store.UpsertRepositorySnapshot(ctx, repoStateEvent(t, pk, "app", commitA, 150))
	if err != nil {
		t.Fatalf("stale upsert: %v", err)
	}
	if got.HeadChanged {
		t.Fatalf("stale (older) snapshot reported as change: %+v", got)
	}
}

func TestUpsertRepositorySnapshotEmptyHeadNeverChanges(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	pk, err := nostr.PubKeyFromHex(strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("pubkey: %v", err)
	}
	// No branch commit tag -> no derivable head -> never a change.
	got, err := store.UpsertRepositorySnapshot(ctx, repoStateEvent(t, pk, "headless", "", 100))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got.HeadChanged || got.NewHead != "" {
		t.Fatalf("empty-head snapshot = %+v, want no change", got)
	}
}
