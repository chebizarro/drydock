// Package monitoring maintains the operator-authored NIP-51 repository list.
package monitoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"drydock/internal/db"
	"drydock/internal/eventkind"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
)

const (
	MonitoredListKind nostr.Kind = eventkind.MonitoredRepositories
	DeletionKind      nostr.Kind = eventkind.Deletion
	ListIdentifier               = "drydock:monitored-repositories:v1"
)

var (
	ErrUnauthorizedAuthor = errors.New("monitored repository event is not authored by the configured operator")
	ErrMalformedList      = errors.New("malformed monitored repository list")
	ErrMalformedDeletion  = errors.New("malformed monitored repository list deletion")
)

type Store interface {
	ApplyMonitoredRepositoryList(context.Context, db.MonitoredRepositoryListState) (bool, error)
	LoadMonitoredRepositoryList(context.Context, string) (db.MonitoredRepositoryListState, bool, error)
}

// Snapshot is an immutable view of the accepted winning list revision.
type Snapshot struct {
	Initialized  bool
	Deleted      bool
	RevisionID   string
	CreatedAt    int64
	Repositories map[string]struct{}
}

type Registry struct {
	store       Store
	operator    nostr.PubKey
	listAddress string

	mu       sync.Mutex
	snapshot atomic.Pointer[Snapshot]
}

func NewRegistry(store Store, operatorPubkey string) (*Registry, error) {
	if store == nil {
		return nil, errors.New("monitoring store is required")
	}
	operator, err := scope.ParsePubkey(operatorPubkey)
	if err != nil {
		return nil, fmt.Errorf("invalid monitored repositories operator: %w", err)
	}
	r := &Registry{
		store:       store,
		operator:    operator,
		listAddress: ListAddress(operator.Hex()),
	}
	r.snapshot.Store(&Snapshot{Repositories: map[string]struct{}{}})
	return r, nil
}

// ListAddress returns the fixed parameterized-replaceable list address.
func ListAddress(operatorPubkey string) string {
	return fmt.Sprintf("%d:%s:%s", MonitoredListKind, strings.ToLower(strings.TrimSpace(operatorPubkey)), ListIdentifier)
}

func (r *Registry) Load(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok, err := r.store.LoadMonitoredRepositoryList(ctx, r.listAddress)
	if err != nil {
		return err
	}
	if !ok {
		r.snapshot.Store(&Snapshot{Repositories: map[string]struct{}{}})
		return nil
	}
	if state.ListAddress != r.listAddress || state.OperatorPubkey != r.operator.Hex() || state.DTag != ListIdentifier {
		return errors.New("persisted monitored repository list identity does not match configuration")
	}
	r.snapshot.Store(snapshotFromState(state))
	return nil
}

func (r *Registry) ApplyList(ctx context.Context, event nostr.Event) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if event.Kind != MonitoredListKind {
		return false, fmt.Errorf("%w: kind must be %d", ErrMalformedList, MonitoredListKind)
	}
	if event.PubKey != r.operator {
		return false, ErrUnauthorizedAuthor
	}
	if event.ID == nostr.ZeroID {
		return false, fmt.Errorf("%w: event id is required", ErrMalformedList)
	}

	dCount := 0
	dValue := ""
	repositories := make(map[string]scope.RepositoryRef)
	for _, tag := range event.Tags {
		if len(tag) == 0 {
			continue
		}
		switch tag[0] {
		case "d":
			dCount++
			if len(tag) >= 2 {
				dValue = tag[1]
			}
		case "a":
			if len(tag) < 2 {
				return false, fmt.Errorf("%w: repository a tag is missing an address", ErrMalformedList)
			}
			ref, err := scope.ParseRepositoryRef(tag[1])
			if err != nil {
				return false, fmt.Errorf("%w: %v", ErrMalformedList, err)
			}
			repositories[ref.Address] = ref
		}
	}
	if dCount != 1 || dValue != ListIdentifier {
		return false, fmt.Errorf("%w: exactly one d tag must equal %q", ErrMalformedList, ListIdentifier)
	}

	raw, err := json.Marshal(event)
	if err != nil {
		return false, fmt.Errorf("marshal monitored repository list: %w", err)
	}
	members := make([]db.MonitoredRepositoryMember, 0, len(repositories))
	for _, ref := range repositories {
		members = append(members, db.MonitoredRepositoryMember{
			RepositoryAddress: ref.Address,
			RepositoryID:      ref.RepositoryID,
		})
	}
	state := db.MonitoredRepositoryListState{
		ListAddress:    r.listAddress,
		OperatorPubkey: r.operator.Hex(),
		DTag:           ListIdentifier,
		SourceKind:     int(event.Kind),
		EventID:        event.ID.Hex(),
		CreatedAt:      int64(event.CreatedAt),
		RawEvent:       string(raw),
		Repositories:   members,
	}
	applied, err := r.store.ApplyMonitoredRepositoryList(ctx, state)
	if err != nil || !applied {
		return applied, err
	}
	r.snapshot.Store(snapshotFromState(state))
	return true, nil
}

func (r *Registry) ApplyDeletion(ctx context.Context, event nostr.Event) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if event.Kind != DeletionKind {
		return false, fmt.Errorf("%w: kind must be %d", ErrMalformedDeletion, DeletionKind)
	}
	if event.PubKey != r.operator {
		return false, ErrUnauthorizedAuthor
	}
	if event.ID == nostr.ZeroID {
		return false, fmt.Errorf("%w: event id is required", ErrMalformedDeletion)
	}
	targeted := false
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "a" && tag[1] == r.listAddress {
			targeted = true
			break
		}
	}
	if !targeted {
		return false, fmt.Errorf("%w: exact list address is required", ErrMalformedDeletion)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return false, fmt.Errorf("marshal monitored repository deletion: %w", err)
	}
	state := db.MonitoredRepositoryListState{
		ListAddress:    r.listAddress,
		OperatorPubkey: r.operator.Hex(),
		DTag:           ListIdentifier,
		SourceKind:     int(event.Kind),
		EventID:        event.ID.Hex(),
		CreatedAt:      int64(event.CreatedAt),
		Deleted:        true,
		RawEvent:       string(raw),
	}
	applied, err := r.store.ApplyMonitoredRepositoryList(ctx, state)
	if err != nil || !applied {
		return applied, err
	}
	r.snapshot.Store(snapshotFromState(state))
	return true, nil
}

func (r *Registry) Contains(repositoryAddress string) bool {
	ref, err := scope.ParseRepositoryRef(repositoryAddress)
	if err != nil {
		return false
	}
	current := r.snapshot.Load()
	if current == nil || !current.Initialized || current.Deleted {
		return false
	}
	_, ok := current.Repositories[ref.Address]
	return ok
}

func (r *Registry) Snapshot() Snapshot {
	current := r.snapshot.Load()
	if current == nil {
		return Snapshot{Repositories: map[string]struct{}{}}
	}
	return cloneSnapshot(*current)
}

func snapshotFromState(state db.MonitoredRepositoryListState) *Snapshot {
	repositories := make(map[string]struct{}, len(state.Repositories))
	if !state.Deleted {
		for _, member := range state.Repositories {
			repositories[member.RepositoryAddress] = struct{}{}
		}
	}
	return &Snapshot{
		Initialized:  true,
		Deleted:      state.Deleted,
		RevisionID:   state.EventID,
		CreatedAt:    state.CreatedAt,
		Repositories: repositories,
	}
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	clone := Snapshot{
		Initialized:  snapshot.Initialized,
		Deleted:      snapshot.Deleted,
		RevisionID:   snapshot.RevisionID,
		CreatedAt:    snapshot.CreatedAt,
		Repositories: make(map[string]struct{}, len(snapshot.Repositories)),
	}
	for address := range snapshot.Repositories {
		clone.Repositories[address] = struct{}{}
	}
	return clone
}
