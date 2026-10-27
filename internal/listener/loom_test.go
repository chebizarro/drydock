package listener

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
	"git.sharegap.net/cascadia/drydock/internal/ingest"
	"git.sharegap.net/cascadia/drydock/internal/loom"
	"git.sharegap.net/cascadia/drydock/internal/monitoring"
	"git.sharegap.net/cascadia/drydock/internal/relayroute"
	"git.sharegap.net/cascadia/drydock/internal/scope"
)

type loomRegistry struct {
	ref     scope.RepositoryRef
	changes chan struct{}
}

func (r *loomRegistry) Snapshot() monitoring.Snapshot {
	return monitoring.Snapshot{Initialized: true, Repositories: map[string]struct{}{r.ref.Address: {}}}
}
func (r *loomRegistry) DeclaredRepositories() []scope.RepositoryRef {
	return []scope.RepositoryRef{r.ref}
}
func (r *loomRegistry) RoutedAuthors() []string { return nil }
func (r *loomRegistry) OperatorState() (nostr.PubKey, nostr.PubKey, bool) {
	return nostr.ZeroPK, nostr.ZeroPK, false
}
func (r *loomRegistry) Changes() <-chan struct{} { return r.changes }
func (r *loomRegistry) Contains(a string) bool   { return a == r.ref.Address }
func TestLoomUsesExistingLiveRouteResubscription(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "routes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	author, publisher := nostr.Generate(), nostr.Generate()
	ref, err := scope.ParseRepositoryRef(fmt.Sprintf("%d:%s:repo", eventkind.RepositoryAnnouncement, author.Public().Hex()))
	if err != nil {
		t.Fatal(err)
	}
	registry := &loomRegistry{ref: ref, changes: make(chan struct{}, 1)}
	claims := loom.New(store, registry, loom.Config{MaxPerRepository: 64, MaxTotal: 1024, TTL: time.Hour}, logger)
	routes := relayroute.NewMonitoringSource(registry, store, []string{"wss://base.example"}, relayroute.Policy{}).WithLoom(ctx, claims)
	processor := ingest.NewProcessor(store, logger, ingest.WithLoom(claims))
	svc := New(Config{Relays: []string{"wss://base.example"}, LookbackMinutes: 1}, processor, logger, WithRoutes(routes))
	svc.debounce = time.Millisecond
	calls := make(chan []nostr.DirectedFilter, 4)
	svc.subscribe = func(ctx context.Context, directed []nostr.DirectedFilter) (chan nostr.RelayEvent, chan nostr.RelayClosed) {
		events, closed := make(chan nostr.RelayEvent), make(chan nostr.RelayClosed)
		go func() { <-ctx.Done(); close(events) }()
		calls <- directed
		return events, closed
	}
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("listener did not stop")
		}
	})
	receive := func() []nostr.DirectedFilter {
		t.Helper()
		select {
		case f := <-calls:
			return f
		case <-time.After(5 * time.Second):
			t.Fatal("no subscription callback")
			return nil
		}
	}
	initial := receive()
	for _, f := range initial {
		for _, kind := range f.Filter.Kinds {
			if kind == eventkind.HiveWorkflowRun && (len(f.Filter.Tags["a"]) != 1 || f.Filter.Tags["a"][0] != ref.Address) {
				t.Fatal("primary filter escaped monitored repository")
			}
			if kind == eventkind.HiveWorkflowResult || kind == eventkind.LoomJobResult {
				t.Fatal("secondary subscription before lineage")
			}
		}
	}
	run := nostr.Event{Kind: eventkind.HiveWorkflowRun, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"a", ref.Address}, {"publisher", publisher.Public().Hex()}, {"commit", strings.Repeat("b", 40)}}}
	if err := run.Sign(author); err != nil {
		t.Fatal(err)
	}
	if err := processor.ProcessEvent(ctx, run, "wss://base.example"); err != nil {
		t.Fatal(err)
	}
	next := receive()
	resultFilters := 0
	for _, f := range next {
		if len(f.Filter.Kinds) == 1 && f.Filter.Kinds[0] == eventkind.HiveWorkflowResult {
			resultFilters++
			if len(f.Filter.Authors) != 1 || f.Filter.Authors[0] != publisher.Public() || len(f.Filter.Tags["e"]) != 1 || f.Filter.Tags["e"][0] != run.ID.Hex() {
				t.Fatalf("cross product/unscoped result filter: %#v", f.Filter)
			}
		}
	}
	if resultFilters != 1 {
		t.Fatalf("want exactly one result filter: %d", resultFilters)
	}
	before, _ := routes.Routes(ctx)
	run.Tags[0][1] = strings.Replace(ref.Address, ":repo", ":unmonitored", 1)
	if err := run.Sign(author); err != nil {
		t.Fatal(err)
	}
	if err := processor.ProcessEvent(ctx, run, "wss://base.example"); err != nil {
		t.Fatal(err)
	}
	after, _ := routes.Routes(ctx)
	if directedKey(svc.directedFilters(0, before)) != directedKey(svc.directedFilters(0, after)) {
		t.Fatal("unmonitored run changed routing")
	}
	// Customer relays get only claims belonging to their declaring repositories.
	other := testRef(t, "other")
	before.Plan = relayroute.Plan{Relays: map[string][]scope.RepositoryRef{"wss://repo.example": {ref}, "wss://other.example": {other}}}
	for _, f := range routeDirectedFilters(0, before) {
		if f.Relay == "wss://other.example" {
			for _, kind := range f.Filter.Kinds {
				if kind == eventkind.HiveWorkflowRun || kind == eventkind.HiveWorkflowResult {
					t.Fatal("claim filters leaked across customer relays")
				}
			}
		}
	}
}
