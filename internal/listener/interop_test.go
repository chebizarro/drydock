package listener

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/ingest"
	"git.sharegap.net/cascadia/drydock/internal/monitoring"
	"git.sharegap.net/cascadia/drydock/internal/relayroute"
	"git.sharegap.net/cascadia/drydock/internal/revieworder"
	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
)

// fakeRelayNet stands in for the pool: every subscription it opens records the
// directed filters and exposes a stream the test feeds. It lets the test play
// a GRASP-style forge whose patches exist only on the repository's own relay.
type fakeRelayNet struct {
	mu   sync.Mutex
	subs []*fakeSub
	new  chan *fakeSub
}

type fakeSub struct {
	directed []nostr.DirectedFilter
	stream   chan nostr.RelayEvent
	closed   chan nostr.RelayClosed
}

func (n *fakeRelayNet) subscribe(ctx context.Context, directed []nostr.DirectedFilter) (chan nostr.RelayEvent, chan nostr.RelayClosed) {
	sub := &fakeSub{directed: directed, stream: make(chan nostr.RelayEvent, 16), closed: make(chan nostr.RelayClosed)}
	go func() { <-ctx.Done(); close(sub.stream) }()
	n.mu.Lock()
	n.subs = append(n.subs, sub)
	n.mu.Unlock()
	n.new <- sub
	return sub.stream, sub.closed
}

// deliver routes an event the way a relay would: only through a subscription
// filter opened on that relay that actually matches the event (kind, authors,
// tags, since). It fails the test when nothing would deliver it, so the test
// cannot pass by injecting events the listener never asked for.
func (s *fakeSub) deliver(t *testing.T, relay string, event nostr.Event) {
	t.Helper()
	for _, f := range s.targets(relay) {
		if f.Matches(event) {
			s.stream <- nostr.RelayEvent{Event: event}
			return
		}
	}
	t.Fatalf("no filter on %s would deliver kind %d event %s: %+v", relay, event.Kind, event.ID.Hex(), s.targets(relay))
}

func (s *fakeSub) targets(relay string) []nostr.Filter {
	var out []nostr.Filter
	for _, d := range s.directed {
		if d.Relay == relay {
			out = append(out, d.Filter)
		}
	}
	return out
}

func (s *fakeSub) deliversPatchesFor(relay, address string) bool {
	for _, f := range s.targets(relay) {
		for _, a := range f.Tags["a"] {
			if a == address {
				return true
			}
		}
	}
	return false
}

func awaitSub(t *testing.T, net *fakeRelayNet) *fakeSub {
	t.Helper()
	select {
	case sub := <-net.new:
		return sub
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not (re)subscribe in time")
		return nil
	}
}

// eventClock hands out strictly increasing created_at values so replaceable
// events in a test never tie on timestamp (ties resolve by event id). It only
// ever moves forward, including across -count runs.
type eventClock struct{ at nostr.Timestamp }

func (c *eventClock) next() nostr.Timestamp { c.at++; return c.at }

// catchUp moves the clock to at least now-5 so subsequent events fall inside
// Since-bounded filters, without ever going backwards.
func (c *eventClock) catchUp() {
	if recent := nostr.Now() - 5; recent > c.at {
		c.at = recent
	}
}

func signed(t *testing.T, clock *eventClock, sk nostr.SecretKey, kind nostr.Kind, tags nostr.Tags, content string) nostr.Event {
	t.Helper()
	ev := nostr.Event{Kind: kind, CreatedAt: clock.next(), Tags: tags, Content: content}
	if err := ev.Sign(sk); err != nil {
		t.Fatal(err)
	}
	return ev
}

// TestGRASPStyleRepositoryIsReviewedFromItsOwnRelayWithoutRestart is the
// repository-native interoperability check for a NIP-34 forge such as GRASP:
// the repository's announcement is the authority for maintainers and relays,
// its patches live only on the relay it declares, and the maintainer's opt-in,
// a relay replacement, and a withdrawal all take effect live.
func TestGRASPStyleRepositoryIsReviewedFromItsOwnRelayWithoutRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "interop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	drydock := nostr.GetPublicKey(nostr.Generate())
	registry, err := monitoring.NewRegistry(store, monitoring.Config{Drydock: drydock.Hex()})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Load(ctx); err != nil {
		t.Fatal(err)
	}
	orders := revieworder.New(revieworder.Config{QueueSize: 8}, store, scope.Matcher{}, registry, nil, nil, logger)
	processor := ingest.NewProcessor(store, logger, ingest.WithMonitoring(registry), ingest.WithReviewOrders(orders))

	const base = "wss://base.example.com"
	const grasp = "wss://grasp.example.org"
	const grasp2 = "wss://grasp2.example.org"
	routes := relayroute.NewMonitoringSource(registry, store, []string{base}, relayroute.Policy{MaxPerRepository: -1, MaxTotal: -1})
	net := &fakeRelayNet{new: make(chan *fakeSub, 8)}
	svc := New(Config{Relays: []string{base}, LookbackMinutes: 5, ContextVMPubkey: drydock.Hex()}, processor, logger, WithRoutes(routes))
	svc.subscribe = net.subscribe
	svc.debounce = 20 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	sub := awaitSub(t, net)
	if len(sub.targets(grasp)) != 0 {
		t.Fatal("customer relay subscribed before any repository was monitored")
	}

	owner, maintainer, contributor := nostr.Generate(), nostr.Generate(), nostr.Generate()
	repoAddr := "30617:" + nostr.GetPublicKey(owner).Hex() + ":svc"
	clock := &eventClock{at: nostr.Now() - 1000} // control-plane events are old on purpose

	// The maintainer opts in first, naming Drydock. It reaches us through the
	// #p discovery filter (no Since), and declares the repository.
	optIn := signed(t, clock, maintainer, monitoring.MonitoredListKind, nostr.Tags{{"d", monitoring.ListIdentifier}, {"p", drydock.Hex()}, {"a", repoAddr}}, "")
	sub.deliver(t, base, optIn)
	sub = awaitSub(t, net)
	if registry.Contains(repoAddr) {
		t.Fatal("monitored before the announcement is known")
	}

	// The forge's announcement is old (well outside the lookback) and arrives
	// only because the declared repository is now recovered without a Since
	// bound on the base relay. It names the maintainer and the repo's relay.
	announcement := signed(t, clock, owner, nostr.KindRepositoryAnnouncement, nostr.Tags{
		{"d", "svc"}, {"maintainers", nostr.GetPublicKey(maintainer).Hex()}, {"relays", grasp}, {"clone", "https://grasp.example.org/svc.git"},
	}, "")
	sub.deliver(t, base, announcement)

	// Live resubscription: the GRASP relay is now subscribed, scoped to the repo.
	sub = awaitSub(t, net)
	if !sub.deliversPatchesFor(grasp, repoAddr) {
		t.Fatalf("GRASP relay not subscribed for %s: %+v", repoAddr, sub.directed)
	}
	if !registry.Contains(repoAddr) {
		t.Fatal("repository not monitored after maintainer opt-in + announcement")
	}

	// From here on events are current (patch filters carry a Since bound).
	clock.catchUp()

	// A patch that exists only on the GRASP relay is reviewed. The base relay
	// must not be able to deliver it under the repo-scoped filter.
	patch := signed(t, clock, contributor, nostr.KindPatch, nostr.Tags{{"a", repoAddr}}, "diff --git a/x b/x")
	sub.deliver(t, grasp, patch)
	select {
	case task := <-orders.Queue():
		if task.PatchEventID != patch.ID.Hex() {
			t.Fatalf("queued %+v, want %s", task, patch.ID.Hex())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("patch delivered on the repository's own relay was not queued")
	}

	// Relay replacement: the owner moves the repository to another relay.
	moved := signed(t, clock, owner, nostr.KindRepositoryAnnouncement, nostr.Tags{
		{"d", "svc"}, {"maintainers", nostr.GetPublicKey(maintainer).Hex()}, {"relays", grasp2},
	}, "")
	sub.deliver(t, base, moved)
	sub = awaitSub(t, net)
	if sub.deliversPatchesFor(grasp, repoAddr) || !sub.deliversPatchesFor(grasp2, repoAddr) {
		t.Fatalf("relay replacement not applied live: %+v", sub.directed)
	}

	// Withdrawal: the maintainer republishes without naming Drydock.
	withdraw := signed(t, clock, maintainer, monitoring.MonitoredListKind, nostr.Tags{{"d", monitoring.ListIdentifier}, {"a", repoAddr}}, "")
	sub.deliver(t, base, withdraw)
	sub = awaitSub(t, net)
	if len(sub.targets(grasp2)) != 0 || registry.Contains(repoAddr) {
		t.Fatalf("withdrawal did not drop the customer relay/membership: %+v", sub.directed)
	}
	later := signed(t, clock, contributor, nostr.KindPatch, nostr.Tags{{"a", repoAddr}}, "diff --git a/y b/y")
	if len(sub.targets(grasp2)) != 0 {
		t.Fatal("withdrawn repository's relay still subscribed")
	}
	// The general base filter still carries patches (other repos may be
	// monitored); this one must be skipped as not monitored, not queued.
	sub.deliver(t, base, later)
	// Processing is synchronous in the listener loop but SQLite under a
	// parallel test run can be slow; poll for the durable skip, then make
	// sure nothing was queued.
	ref, _ := scope.ParseRepositoryRef(repoAddr)
	deadline := time.Now().Add(5 * time.Second)
	for {
		reason, ok, err := store.GetReviewSkip(ctx, later.ID.Hex(), ref.RepositoryID)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if reason != "not_monitored" {
				t.Fatalf("withdrawn repository skip reason = %q", reason)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("withdrawn repository patch was never recorded as skipped")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case task := <-orders.Queue():
		t.Fatalf("patch for a withdrawn repository was queued: %+v", task)
	default:
	}

	// Authority withdrawal through the forge: the owner drops the maintainer
	// while an opt-in is active. Re-opt-in first, then replace the announcement.
	reopt := signed(t, clock, maintainer, monitoring.MonitoredListKind, nostr.Tags{{"d", monitoring.ListIdentifier}, {"p", drydock.Hex()}, {"a", repoAddr}}, "")
	sub.deliver(t, base, reopt)
	sub = awaitSub(t, net)
	if !registry.Contains(repoAddr) {
		t.Fatal("re-opt-in did not restore membership")
	}
	demoted := signed(t, clock, owner, nostr.KindRepositoryAnnouncement, nostr.Tags{{"d", "svc"}, {"relays", grasp2}}, "")
	sub.deliver(t, base, demoted)
	sub = awaitSub(t, net)
	if registry.Contains(repoAddr) || len(sub.targets(grasp2)) != 0 {
		t.Fatal("removed maintainer's opt-in still grants after announcement replacement")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not stop")
	}
}
