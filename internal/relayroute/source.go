package relayroute

import (
	"context"
	"fmt"
	"sync"

	"git.sharegap.net/cascadia/drydock/internal/monitoring"
	"git.sharegap.net/cascadia/drydock/internal/scope"
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
}

// Registry is the monitoring surface the source reads. *monitoring.Registry
// satisfies it.
type Registry interface {
	Snapshot() monitoring.Snapshot
	DeclaredRepositories() []scope.RepositoryRef
	RoutedAuthors() []string
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
	control := Routes{
		DeclaredRepositories: m.registry.DeclaredRepositories(),
		ListAuthors:          m.registry.RoutedAuthors(),
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
	return control, nil
}

// Changes forwards the registry's coalesced change signal.
func (m *MonitoringSource) Changes() <-chan struct{} {
	return m.registry.Changes()
}
