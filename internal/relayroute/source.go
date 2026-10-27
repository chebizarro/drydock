package relayroute

import (
	"context"
	"fmt"
	"sync"

	"git.sharegap.net/cascadia/drydock/internal/loom"
	"git.sharegap.net/cascadia/drydock/internal/monitoring"
	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
)

// Routes are the live control-plane inputs the listener turns into
// subscription filters.
type Routes struct {
	// Plan is the bounded customer-relay plan for monitored repositories.
	Plan Plan
	// DeclaredRepositories are all repositories named by active lists; their
	// announcements are recovered without a Since bound because they decide
	// authority.
	DeclaredRepositories []scope.RepositoryRef
	// ListAuthors are known list authors (hex), subscribed directly so
	// withdrawals and deletions arrive.
	ListAuthors []string
	// Operator is the current ceiling author (hex), whose list and deletion
	// are subscribed without a Since bound. Empty when no ceiling is set.
	Operator string
	// LoomFilters are keyed by monitored repository address, including exact result lineage.
	LoomFilters map[string][]nostr.Filter
}

// Registry is the monitoring surface the source reads. *monitoring.Registry
// satisfies it.
type Registry interface {
	Snapshot() monitoring.Snapshot
	DeclaredRepositories() []scope.RepositoryRef
	RoutedAuthors() []string
	OperatorState() (current, previous nostr.PubKey, hasPrevious bool)
	Changes() <-chan struct{}
}

// RelayStore reads declared relays from stored announcements. *db.Store
// satisfies it.
type RelayStore interface {
	ListRepositoryRelays(ctx context.Context, repoIDs []string) (map[string][]string, error)
}

// MonitoringSource derives Routes from the monitoring registry and the stored
// announcements, under a fixed Policy and base relay set.
type MonitoringSource struct {
	registry Registry
	store    RelayStore
	base     []string
	policy   Policy

	mu       sync.Mutex
	lastPlan Plan
	loom     *loom.Service
	changes  chan struct{}
}

func NewMonitoringSource(registry Registry, store RelayStore, base []string, policy Policy) *MonitoringSource {
	return &MonitoringSource{registry: registry, store: store, base: append([]string(nil), base...), policy: policy}
}

// Routes computes the current routing inputs. It reads the registry snapshot
// once so the plan is consistent with a single membership state. When the
// relay lookup fails, the control-plane parts (declared repositories, known
// authors) are still returned together with the last good plan and the error,
// so a transient store failure neither drops working subscriptions nor hides
// a newly known author.
func (m *MonitoringSource) Routes(ctx context.Context) (Routes, error) {
	snapshot := m.registry.Snapshot()
	operator, _, _ := m.registry.OperatorState()
	control := Routes{
		DeclaredRepositories: m.registry.DeclaredRepositories(),
		ListAuthors:          m.registry.RoutedAuthors(),
	}
	if operator != nostr.ZeroPK {
		control.Operator = operator.Hex()
	}

	refs := make([]scope.RepositoryRef, 0, len(snapshot.Repositories))
	ids := make([]string, 0, len(snapshot.Repositories))
	for address := range snapshot.Repositories {
		ref, err := scope.ParseRepositoryRef(address)
		if err != nil {
			continue
		}
		refs = append(refs, ref)
		ids = append(ids, ref.RepositoryID)
	}
	relays, err := m.store.ListRepositoryRelays(ctx, ids)
	if err != nil {
		m.mu.Lock()
		control.Plan = m.lastPlan
		m.mu.Unlock()
		return control, fmt.Errorf("load repository relays: %w", err)
	}
	repos := make([]Repository, 0, len(refs))
	for _, ref := range refs {
		repos = append(repos, Repository{Ref: ref, Relays: relays[ref.RepositoryID]})
	}

	plan := Build(repos, m.base, m.policy)
	m.mu.Lock()
	m.lastPlan = plan
	m.mu.Unlock()
	control.Plan = plan
	if m.loom != nil {
		control.LoomFilters, err = m.loom.Filters(ctx, refs)
		if err != nil {
			return control, fmt.Errorf("load loom routes: %w", err)
		}
	}
	return control, nil
}

// Changes forwards the registry's coalesced change signal.
func (m *MonitoringSource) Changes() <-chan struct{} {
	if m.changes != nil {
		return m.changes
	}
	return m.registry.Changes()
}
