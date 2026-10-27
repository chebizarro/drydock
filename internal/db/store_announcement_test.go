package db

import (
	"context"
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
)

func announcementEvent(t *testing.T, owner nostr.SecretKey, identifier string, createdAt int64, maintainers ...nostr.PubKey) nostr.Event {
	t.Helper()
	tags := nostr.Tags{{"d", identifier}}
	if len(maintainers) > 0 {
		tag := nostr.Tag{"maintainers"}
		for _, pk := range maintainers {
			tag = append(tag, pk.Hex())
		}
		tags = append(tags, tag)
	}
	event := nostr.Event{Kind: nostr.KindRepositoryAnnouncement, CreatedAt: nostr.Timestamp(createdAt), Tags: tags}
	if err := event.Sign(owner); err != nil {
		t.Fatalf("sign announcement: %v", err)
	}
	return event
}

func TestApplyRepositoryAnnouncementNewestWinsAndStaleReplayIsIgnored(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "ann.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	owner := nostr.Generate()
	maintainer := nostr.GetPublicKey(nostr.Generate())
	repoID := nostr.GetPublicKey(owner).Hex() + ":svc"

	older := announcementEvent(t, owner, "svc", 100, maintainer)
	newer := announcementEvent(t, owner, "svc", 200)

	if applied, err := store.ApplyRepositoryAnnouncement(ctx, newer); err != nil || !applied {
		t.Fatalf("apply newer: applied=%v err=%v", applied, err)
	}
	if applied, err := store.ApplyRepositoryAnnouncement(ctx, older); err != nil || applied {
		t.Fatalf("stale replay must be ignored: applied=%v err=%v", applied, err)
	}
	if applied, err := store.ApplyRepositoryAnnouncement(ctx, newer); err != nil || applied {
		t.Fatalf("same revision must be a no-op: applied=%v err=%v", applied, err)
	}

	stored, ok, err := store.LoadRepositoryAnnouncement(ctx, repoID)
	if err != nil || !ok || stored.ID != newer.ID {
		t.Fatalf("stored announcement = %v ok=%v err=%v, want %s", stored.ID, ok, err, newer.ID.Hex())
	}
	if can, err := store.CanMaintainRepository(ctx, repoID, maintainer); err != nil || can {
		t.Fatalf("maintainer from the stale revision still has authority: can=%v err=%v", can, err)
	}
	if can, err := store.CanMaintainRepository(ctx, repoID, nostr.GetPublicKey(owner)); err != nil || !can {
		t.Fatalf("owner lost authority: can=%v err=%v", can, err)
	}

	// A fresher revision that re-adds the maintainer wins again.
	newest := announcementEvent(t, owner, "svc", 300, maintainer)
	if applied, err := store.ApplyRepositoryAnnouncement(ctx, newest); err != nil || !applied {
		t.Fatalf("apply newest: applied=%v err=%v", applied, err)
	}
	if can, err := store.CanMaintainRepository(ctx, repoID, maintainer); err != nil || !can {
		t.Fatalf("re-added maintainer lacks authority: can=%v err=%v", can, err)
	}
}

func TestListRepositoryRelaysFollowsLatestAnnouncement(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "relays.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	owner := nostr.Generate()
	repoID := nostr.GetPublicKey(owner).Hex() + ":svc"
	first := nostr.Event{Kind: nostr.KindRepositoryAnnouncement, CreatedAt: 100, Tags: nostr.Tags{{"d", "svc"}, {"relays", "wss://one.example.com", "wss://two.example.com"}}}
	if err := first.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyRepositoryAnnouncement(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListRepositoryRelays(ctx, []string{repoID, "missing:repo"})
	if err != nil || len(got) != 1 || len(got[repoID]) != 2 || got[repoID][0] != "wss://one.example.com" {
		t.Fatalf("relays = %v err=%v", got, err)
	}
	replaced := nostr.Event{Kind: nostr.KindRepositoryAnnouncement, CreatedAt: 200, Tags: nostr.Tags{{"d", "svc"}, {"relays", "wss://three.example.com"}}}
	if err := replaced.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyRepositoryAnnouncement(ctx, replaced); err != nil {
		t.Fatal(err)
	}
	got, err = store.ListRepositoryRelays(ctx, []string{repoID})
	if err != nil || len(got[repoID]) != 1 || got[repoID][0] != "wss://three.example.com" {
		t.Fatalf("relays after replacement = %v err=%v", got, err)
	}
}

func TestApplyRepositoryAnnouncementRejectsInvalid(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "ann.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	owner := nostr.Generate()

	noD := nostr.Event{Kind: nostr.KindRepositoryAnnouncement, CreatedAt: 100}
	if err := noD.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if applied, err := store.ApplyRepositoryAnnouncement(ctx, noD); err != nil || applied {
		t.Fatalf("announcement without d tag: applied=%v err=%v", applied, err)
	}

	twoD := nostr.Event{Kind: nostr.KindRepositoryAnnouncement, CreatedAt: 100, Tags: nostr.Tags{{"d", "a"}, {"d", "b"}}}
	if err := twoD.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if applied, err := store.ApplyRepositoryAnnouncement(ctx, twoD); err != nil || applied {
		t.Fatalf("announcement with two d tags: applied=%v err=%v", applied, err)
	}
}

func TestRuntimeSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetRuntimeSetting(ctx, RuntimeSettingMonitoringOperator); err != nil || found {
		t.Fatalf("fresh setting found=%v err=%v", found, err)
	}
	if err := store.SetRuntimeSettings(ctx, map[string]string{RuntimeSettingMonitoringOperator: "abc", RuntimeSettingMonitoringOperatorPrevious: ""}, "admin", "evt"); err != nil {
		t.Fatal(err)
	}
	v, found, err := store.GetRuntimeSetting(ctx, RuntimeSettingMonitoringOperator)
	if err != nil || !found || v != "abc" {
		t.Fatalf("get = %q %v %v", v, found, err)
	}
	v, found, err = store.GetRuntimeSetting(ctx, RuntimeSettingMonitoringOperatorPrevious)
	if err != nil || !found || v != "" {
		t.Fatalf("empty value must still be found: %q %v %v", v, found, err)
	}
	if err := store.SetRuntimeSettings(ctx, map[string]string{"": "x"}, "", ""); err == nil {
		t.Fatal("empty key accepted")
	}
}
