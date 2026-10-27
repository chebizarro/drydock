package listener

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/relayroute"
	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
)

func testRef(t *testing.T, identifier string) scope.RepositoryRef {
	t.Helper()
	r, err := scope.ParseRepositoryRef("30617:" + nostr.GetPublicKey(nostr.Generate()).Hex() + ":" + identifier)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRouteDirectedFiltersScopeCustomerRelaysToDeclaringRepositories(t *testing.T) {
	a, b := testRef(t, "a"), testRef(t, "b")
	routes := relayroute.Routes{Plan: relayroute.Plan{Relays: map[string][]scope.RepositoryRef{
		"wss://grasp.example.org": {a, b},
		"wss://solo.example.org":  {b},
	}}}
	directed := routeDirectedFilters(nostr.Timestamp(42), routes)
	// grasp: work + one announcement filter per owner (2); solo: work + 1.
	if len(directed) != 5 {
		t.Fatalf("directed filters = %d, want 5", len(directed))
	}
	byRelay := map[string][]nostr.Filter{}
	for _, d := range directed {
		byRelay[d.Relay] = append(byRelay[d.Relay], d.Filter)
	}
	work := byRelay["wss://grasp.example.org"][0]
	if work.Since != 42 || len(work.Kinds) != 3 || len(work.Tags["a"]) != 2 {
		t.Fatalf("work filter = %#v", work)
	}
	for _, kind := range work.Kinds {
		if kind != 1617 && kind != 1618 && kind != 1619 {
			t.Fatalf("unexpected work kind %d", kind)
		}
	}
	for _, ann := range byRelay["wss://grasp.example.org"][1:] {
		if ann.Since != 0 || len(ann.Authors) != 1 || len(ann.Tags["d"]) != 1 || len(ann.Kinds) != 2 {
			t.Fatalf("announcement filter = %#v (must be no-Since, one owner, that owner's identifiers only, 30617+30618)", ann)
		}
		want := map[string]string{a.OwnerPubkey: "a", b.OwnerPubkey: "b"}[ann.Authors[0].Hex()]
		if ann.Tags["d"][0] != want {
			t.Fatalf("cross-owner identifier leaked: owner %s got d=%s", ann.Authors[0].Hex(), ann.Tags["d"][0])
		}
	}
	solo := byRelay["wss://solo.example.org"][0]
	if len(solo.Tags["a"]) != 1 || solo.Tags["a"][0] != b.Address {
		t.Fatalf("solo relay must only carry repository b: %#v", solo)
	}
}

func TestControlPlaneFiltersCoverKnownAuthorsAndDeclaredAnnouncements(t *testing.T) {
	known := nostr.GetPublicKey(nostr.Generate())
	decl := testRef(t, "svc")
	filters := controlPlaneFilters(relayroute.Routes{
		ListAuthors:          []string{known.Hex(), "junk"},
		DeclaredRepositories: []scope.RepositoryRef{decl},
	})
	if len(filters) != 3 {
		t.Fatalf("filters = %d, want list+deletion+announcement", len(filters))
	}
	for _, f := range filters {
		if f.Since != 0 {
			t.Fatalf("control-plane filter must have no Since: %#v", f)
		}
	}
	if filters[0].Kinds[0] != 30001 || filters[0].Authors[0] != known || len(filters[0].Tags["p"]) != 0 {
		t.Fatalf("known-author list filter = %#v", filters[0])
	}
	if filters[1].Kinds[0] != 5 || filters[1].Tags["a"][0] != "30001:"+known.Hex()+":drydock:monitored-repositories:v1" {
		t.Fatalf("known-author deletion filter = %#v", filters[1])
	}
	if filters[2].Kinds[0] != 30617 || len(filters[2].Kinds) != 1 || filters[2].Authors[0].Hex() != decl.OwnerPubkey || filters[2].Tags["d"][0] != "svc" {
		t.Fatalf("announcement recovery filter = %#v", filters[2])
	}
}

func TestRunDoesNotResubscribeWhenRoutingIsUnchanged(t *testing.T) {
	planner := &fakePlanner{changes: make(chan struct{}, 1)}
	calls := make(chan *subscribeCall, 4)
	svc := New(Config{Relays: []string{"wss://base.example.com"}, LookbackMinutes: 1}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), WithRoutes(planner))
	svc.debounce = 10 * time.Millisecond
	svc.subscribe = func(ctx context.Context, directed []nostr.DirectedFilter) (chan nostr.RelayEvent, chan nostr.RelayClosed) {
		call := &subscribeCall{directed: directed, stream: make(chan nostr.RelayEvent), closed: make(chan nostr.RelayClosed)}
		go func() { <-ctx.Done(); close(call.stream) }()
		calls <- call
		return call.stream, call.closed
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = svc.Run(ctx) }()
	<-calls

	// A change signal whose resulting filters are identical (e.g. an
	// announcement replay) must not tear down the subscription.
	planner.set(relayroute.Routes{})
	select {
	case call := <-calls:
		t.Fatalf("resubscribed although routing was unchanged: %d filters", len(call.directed))
	case <-time.After(300 * time.Millisecond):
	}
}

type fakePlanner struct {
	mu      sync.Mutex
	routes  relayroute.Routes
	changes chan struct{}
}

func (f *fakePlanner) Routes(context.Context) (relayroute.Routes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.routes, nil
}
func (f *fakePlanner) Changes() <-chan struct{} { return f.changes }
func (f *fakePlanner) set(r relayroute.Routes) {
	f.mu.Lock()
	f.routes = r
	f.mu.Unlock()
	f.changes <- struct{}{}
}

type subscribeCall struct {
	directed []nostr.DirectedFilter
	stream   chan nostr.RelayEvent
	closed   chan nostr.RelayClosed
}

func TestRunResubscribesWhenRoutesChangeWithoutRestart(t *testing.T) {
	planner := &fakePlanner{changes: make(chan struct{}, 1)}
	calls := make(chan *subscribeCall, 4)
	svc := New(Config{Relays: []string{"wss://base.example.com"}, LookbackMinutes: 1}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), WithRoutes(planner))
	svc.debounce = 10 * time.Millisecond
	svc.subscribe = func(ctx context.Context, directed []nostr.DirectedFilter) (chan nostr.RelayEvent, chan nostr.RelayClosed) {
		call := &subscribeCall{directed: directed, stream: make(chan nostr.RelayEvent), closed: make(chan nostr.RelayClosed)}
		go func() { <-ctx.Done(); close(call.stream) }()
		calls <- call
		return call.stream, call.closed
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	first := <-calls
	for _, d := range first.directed {
		if d.Relay != "wss://base.example.com" {
			t.Fatalf("initial subscription touched a non-base relay: %s", d.Relay)
		}
	}

	repo := testRef(t, "svc")
	planner.set(relayroute.Routes{Plan: relayroute.Plan{Relays: map[string][]scope.RepositoryRef{"wss://grasp.example.org": {repo}}}})

	var second *subscribeCall
	select {
	case second = <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not resubscribe after a route change")
	}
	sawCustomer := false
	for _, d := range second.directed {
		if d.Relay == "wss://grasp.example.org" && len(d.Filter.Tags["a"]) == 1 && d.Filter.Tags["a"][0] == repo.Address {
			sawCustomer = true
		}
	}
	if !sawCustomer {
		t.Fatalf("resubscription lacks the scoped customer-relay filter: %+v", second.directed)
	}

	// Removing the repository drops the relay again.
	planner.set(relayroute.Routes{})
	var third *subscribeCall
	select {
	case third = <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not resubscribe after route removal")
	}
	for _, d := range third.directed {
		if d.Relay == "wss://grasp.example.org" {
			t.Fatal("removed customer relay still subscribed")
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on context cancellation")
	}
}
