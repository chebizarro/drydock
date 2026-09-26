package ingest_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/ingest"

	"fiatjaf.com/nostr"
)

type recordingUpgrader struct {
	calls []struct{ repoID, newHead string }
}

func (r *recordingUpgrader) OnDefaultBranchHeadChanged(_ context.Context, repoID, newHead string) error {
	r.calls = append(r.calls, struct{ repoID, newHead string }{repoID, newHead})
	return nil
}

func repositoryStateEvent(t *testing.T, sk nostr.SecretKey, id, headCommit string, createdAt int64) nostr.Event {
	t.Helper()
	evt := nostr.Event{
		Kind:      nostr.KindRepositoryState,
		CreatedAt: nostr.Timestamp(createdAt),
		Tags: nostr.Tags{
			{"d", id},
			{"HEAD", "ref: refs/heads/main"},
			{"refs/heads/main", headCommit},
		},
	}
	signEvent(t, sk, &evt)
	return evt
}

// TestDependencyUpgradeTriggerOnHeadChange is the Stage-5 completion bar: a
// RepositoryState event whose default-branch head moved enqueues exactly one
// scan, and a repeated state with an unchanged head enqueues none.
func TestDependencyUpgradeTriggerOnHeadChange(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	up := &recordingUpgrader{}
	processor := ingest.NewProcessor(store, logger, ingest.WithDependencyUpgrades(up))

	sk := nostr.Generate()
	commitA := strings.Repeat("1", 40)
	commitB := strings.Repeat("2", 40)
	now := time.Now().Unix()

	// First observation with a head -> one scan.
	if err := processor.ProcessEvent(ctx, repositoryStateEvent(t, sk, "app", commitA, now-20), "wss://relay.test"); err != nil {
		t.Fatalf("process first state: %v", err)
	}
	// Newer state moving the head -> another scan.
	if err := processor.ProcessEvent(ctx, repositoryStateEvent(t, sk, "app", commitB, now-10), "wss://relay.test"); err != nil {
		t.Fatalf("process second state: %v", err)
	}
	// Newer state with the same head -> no scan.
	if err := processor.ProcessEvent(ctx, repositoryStateEvent(t, sk, "app", commitB, now), "wss://relay.test"); err != nil {
		t.Fatalf("process third state: %v", err)
	}

	if len(up.calls) != 2 {
		t.Fatalf("upgrade trigger fired %d times, want 2 (one per head change)", len(up.calls))
	}
	wantRepo := sk.Public().Hex() + ":app"
	if up.calls[0].repoID != wantRepo || up.calls[0].newHead != commitA {
		t.Fatalf("first call = %+v, want {%s %s}", up.calls[0], wantRepo, commitA)
	}
	if up.calls[1].newHead != commitB {
		t.Fatalf("second call head = %q, want %q", up.calls[1].newHead, commitB)
	}
}

// TestDependencyUpgradeTriggerUnconfigured verifies that an unconfigured
// processor ignores the head-change signal entirely (no panic, no error).
func TestDependencyUpgradeTriggerUnconfigured(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	processor := ingest.NewProcessor(store, logger)

	sk := nostr.Generate()
	if err := processor.ProcessEvent(ctx, repositoryStateEvent(t, sk, "app", strings.Repeat("1", 40), time.Now().Unix()), "wss://relay.test"); err != nil {
		t.Fatalf("process state: %v", err)
	}
}
