// Package monitoring maintains the set of repositories Drydock monitors.
//
// Membership is maintainer-authorized rather than operator-listed. A repository
// is monitored when some author A publishes a NIP-51 list (kind 30001,
// d=ListIdentifier) that names Drydock's pubkey in a `p` tag and the repository
// in an `a` tag, AND A is the owner or a maintainer in the latest valid kind-30617
// announcement of that repository. Membership is the union over all authors, and
// it is re-evaluated whenever a list, a deletion, or an announcement changes, so
// a withdrawn list, a removed maintainer, or a replaced announcement suspends the
// repository without a restart.
//
// The operator (an optional list author plus the repository allowlists) can only
// narrow membership: it is a ceiling and a deny switch, never a grant.
package monitoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip34"
)

const (
	MonitoredListKind nostr.Kind = eventkind.MonitoredRepositories
	DeletionKind      nostr.Kind = eventkind.Deletion
	ListIdentifier               = "drydock:monitored-repositories:v1"

	// maxListRepositories bounds one author's list so a single key cannot make
	// membership evaluation unbounded. Larger lists are treated as malformed.
	maxListRepositories = 2048
)

// Routing budgets. Strangers may publish service-addressed lists naming any
// repository, so the recovery subscriptions derived from declarations must be
// bounded independently of the per-list limit. Variables so tests can shrink
// them.
var (
	maxDeclaredRepositories = 2048
	maxRoutedAuthors        = 256
)

var (
	ErrUnauthorizedAuthor = errors.New("event author may not modify this monitored repository list")
	ErrMalformedList      = errors.New("malformed monitored repository list")
	ErrMalformedDeletion  = errors.New("malformed monitored repository list deletion")
)

// Store is the persistence the registry needs: the per-author list winners and
// the authoritative repository announcements.
type Store interface {
	ApplyMonitoredRepositoryList(context.Context, db.MonitoredRepositoryListState) (bool, error)
	LoadMonitoredRepositoryLists(context.Context) ([]db.MonitoredRepositoryListState, error)
	LoadRepositoryAnnouncement(ctx context.Context, repoID string) (nostr.Event, bool, error)
	LoadMonitoringEnrollments(context.Context) ([]db.MonitoringEnrollment, error)
}

// Enrollment is a maintainer's ContextVM opt-in for one repository. It grants
// membership under the same authority rule as a list entry, and additionally
// stops granting at ExpiresAt (0 = never).
type Enrollment struct {
	Author         nostr.PubKey
	Ref            scope.RepositoryRef
	RequestEventID string
	CreatedAt      int64
	ExpiresAt      int64
}

// AuthorStatus is the effect an author's current list revision has on membership.
type AuthorStatus string

const (
	// StatusActive: the list names Drydock; its repositories are monitored where
	// the author holds owner/maintainer authority.
	StatusActive AuthorStatus = "active"
	// StatusWithdrawn: a valid list that does not name Drydock (opt-in removed).
	StatusWithdrawn AuthorStatus = "withdrawn"
	// StatusDeleted: the list was removed by a NIP-09 deletion.
	StatusDeleted AuthorStatus = "deleted"
	// StatusMalformed: the author's newest revision is unusable. The author is
	// suspended rather than falling back to an older, possibly stale, opt-in.
	StatusMalformed AuthorStatus = "malformed"
)

// AuthorSnapshot describes one author's winning list revision.
type AuthorSnapshot struct {
	Status     AuthorStatus
	RevisionID string
	CreatedAt  int64
}

// Snapshot is an immutable view of the effective membership.
type Snapshot struct {
	// Initialized is true once persisted state has been recovered and, when an
	// operator ceiling is configured, the operator's list has been seen. It is
	// the fail-closed gate for membership.
	Initialized bool
	// Loaded reports that persisted state has been recovered: the service is
	// healthy regardless of whether any customer or operator list has arrived.
	// Readiness keys on this, not on Initialized.
	Loaded bool
	// Operator is the current ceiling author (hex), empty when none.
	Operator string
	// PreviousOperator is the ceiling author a rollback would restore.
	PreviousOperator string
	// CeilingSeen reports that the current operator's list (or its deletion)
	// is known. False with an operator configured means membership is empty.
	CeilingSeen bool
	// Repositories is the effective membership after authorization and ceilings.
	Repositories map[string]struct{}
	// Authors is every known list author's winning revision, keyed by pubkey hex.
	Authors map[string]AuthorSnapshot
	// Expiries holds, for repositories whose membership rests only on
	// enrollments, the latest expiry among the granting enrollments. Contains
	// consults it so an expired enrollment stops granting without a rebuild.
	Expiries map[string]int64
}

// Config configures a Registry.
type Config struct {
	// Drydock is Drydock's own service pubkey. Only lists naming it opt in.
	Drydock string
	// Operator optionally names the author of the operator ceiling list. When
	// set, effective membership is limited to the repositories that list names;
	// a missing, deleted, or malformed operator list monitors nothing.
	Operator string
	// PreviousOperator and HasPreviousOperator hydrate rollback history that
	// the control plane persisted before a restart.
	PreviousOperator    string
	HasPreviousOperator bool
	// Scope is the operator repository/owner allowlist ceiling. Its zero value
	// allows everything.
	Scope scope.Matcher
}

type authorList struct {
	author       nostr.PubKey
	status       AuthorStatus
	revisionID   string
	createdAt    int64
	namesDrydock bool
	repos        map[string]scope.RepositoryRef
}

type authority struct {
	known       bool
	owner       nostr.PubKey
	maintainers map[nostr.PubKey]struct{}
}

type Registry struct {
	store   Store
	drydock nostr.PubKey
	scope   scope.Matcher
	// operator is the ceiling author (ZeroPK = none). It is mutable through
	// SetOperatorState so the ceiling can be rotated without a restart;
	// previous/hasPrevious are rollback history owned by the control plane.
	// All three are guarded by mu.
	operator    nostr.PubKey
	previous    nostr.PubKey
	hasPrevious bool

	now func() time.Time

	mu          sync.Mutex
	loaded      bool
	lists       map[nostr.PubKey]*authorList
	enrollments map[string]map[nostr.PubKey]Enrollment // address -> author -> enrollment
	authorities map[string]authority
	snapshot    atomic.Pointer[Snapshot]
	// changes receives one coalesced signal per published rebuild so the
	// listener can resubscribe when membership, authors, or announcements move.
	changes chan struct{}
}

func NewRegistry(store Store, cfg Config) (*Registry, error) {
	if store == nil {
		return nil, errors.New("monitoring store is required")
	}
	drydock, err := scope.ParsePubkey(cfg.Drydock)
	if err != nil {
		return nil, fmt.Errorf("invalid drydock service pubkey: %w", err)
	}
	r := &Registry{
		store:       store,
		drydock:     drydock,
		scope:       cfg.Scope,
		now:         time.Now,
		lists:       make(map[nostr.PubKey]*authorList),
		enrollments: make(map[string]map[nostr.PubKey]Enrollment),
		authorities: make(map[string]authority),
		changes:     make(chan struct{}, 1),
	}
	if strings.TrimSpace(cfg.Operator) != "" {
		if r.operator, err = scope.ParsePubkey(cfg.Operator); err != nil {
			return nil, fmt.Errorf("invalid monitored repositories operator: %w", err)
		}
	}
	if cfg.HasPreviousOperator {
		r.hasPrevious = true
		if strings.TrimSpace(cfg.PreviousOperator) != "" {
			if r.previous, err = scope.ParsePubkey(cfg.PreviousOperator); err != nil {
				return nil, fmt.Errorf("invalid previous monitored repositories operator: %w", err)
			}
		}
	}
	r.snapshot.Store(&Snapshot{Repositories: map[string]struct{}{}, Authors: map[string]AuthorSnapshot{}, Expiries: map[string]int64{}})
	return r, nil
}

// ListAddress returns the parameterized-replaceable list address of an author.
func ListAddress(authorPubkey string) string {
	return fmt.Sprintf("%d:%s:%s", MonitoredListKind, strings.ToLower(strings.TrimSpace(authorPubkey)), ListIdentifier)
}

// Load recovers every persisted list and the announcement authority behind it.
// It fails closed on persisted state that does not match its own identity.
func (r *Registry) Load(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	states, err := r.store.LoadMonitoredRepositoryLists(ctx)
	if err != nil {
		return err
	}
	lists := make(map[nostr.PubKey]*authorList, len(states))
	for _, state := range states {
		list, err := r.listFromState(state)
		if err != nil {
			return err
		}
		lists[list.author] = list
	}
	rows, err := r.store.LoadMonitoringEnrollments(ctx)
	if err != nil {
		return err
	}
	enrollments := make(map[string]map[nostr.PubKey]Enrollment)
	loadNow := r.now().Unix()
	for _, row := range rows {
		if !row.Active(loadNow) {
			continue
		}
		author, err := nostr.PubKeyFromHex(row.AuthorPubkey)
		if err != nil {
			return fmt.Errorf("persisted monitoring enrollment has an invalid author: %w", err)
		}
		ref, err := scope.ParseRepositoryRef(row.RepositoryAddress)
		if err != nil {
			return fmt.Errorf("persisted monitoring enrollment %q: %w", row.RepositoryAddress, err)
		}
		if enrollments[ref.Address] == nil {
			enrollments[ref.Address] = map[nostr.PubKey]Enrollment{}
		}
		enrollments[ref.Address][author] = Enrollment{Author: author, Ref: ref, RequestEventID: row.RequestEventID, CreatedAt: row.RequestCreatedAt, ExpiresAt: row.ExpiresAt}
	}
	authorities, err := r.loadAuthorities(ctx, lists, enrollments, nil)
	if err != nil {
		return err
	}
	r.lists = lists
	r.enrollments = enrollments
	r.authorities = authorities
	r.loaded = true
	r.rebuildLocked()
	return nil
}

// ApplyEnrollment admits a persisted enrollment into the live membership
// derivation. Authority is resolved before the state changes so a lookup
// failure leaves nothing half-applied.
func (r *Registry) ApplyEnrollment(ctx context.Context, e Enrollment) error {
	if e.Author == nostr.ZeroPK || e.Ref.Address == "" {
		return errors.New("enrollment requires an author and a repository")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := map[string]map[nostr.PubKey]Enrollment{e.Ref.Address: {e.Author: e}}
	authorities, err := r.loadAuthorities(ctx, nil, pending, r.authorities)
	if err != nil {
		return err
	}
	if r.enrollments[e.Ref.Address] == nil {
		r.enrollments[e.Ref.Address] = map[nostr.PubKey]Enrollment{}
	}
	r.enrollments[e.Ref.Address][e.Author] = e
	r.authorities = authorities
	r.rebuildLocked()
	return nil
}

// RevokeEnrollment removes a (repository, author) enrollment from the live
// derivation. Unknown enrollments are a no-op.
func (r *Registry) RevokeEnrollment(repositoryAddress string, author nostr.PubKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	byAuthor, ok := r.enrollments[repositoryAddress]
	if !ok {
		return
	}
	if _, ok := byAuthor[author]; !ok {
		return
	}
	delete(byAuthor, author)
	if len(byAuthor) == 0 {
		delete(r.enrollments, repositoryAddress)
	}
	r.rebuildLocked()
}

// ApplyList applies an author's list revision. Any author may publish a list;
// whether it grants anything is decided by announcement authority, so an
// unauthorized author's list is retained (and simply grants nothing) because a
// later announcement may confer authority and a later list must be orderable
// against it.
//
// A revision of our list address whose tags are unusable is attributable to its
// signer, so it is persisted as StatusMalformed and suspends that author; in
// that case ApplyList returns applied=true together with ErrMalformedList.
func (r *Registry) ApplyList(ctx context.Context, event nostr.Event) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if event.Kind != MonitoredListKind {
		return false, fmt.Errorf("%w: kind must be %d", ErrMalformedList, MonitoredListKind)
	}
	if event.PubKey == nostr.ZeroPK {
		return false, ErrUnauthorizedAuthor
	}
	if event.ID == nostr.ZeroID {
		return false, fmt.Errorf("%w: event id is required", ErrMalformedList)
	}
	if !hasListIdentifier(event) {
		return false, fmt.Errorf("%w: exactly one d tag must equal %q", ErrMalformedList, ListIdentifier)
	}

	names, repos, parseErr := parseListTags(event, r.drydock)
	raw, err := json.Marshal(event)
	if err != nil {
		return false, fmt.Errorf("marshal monitored repository list: %w", err)
	}
	state := db.MonitoredRepositoryListState{
		ListAddress:    ListAddress(event.PubKey.Hex()),
		OperatorPubkey: event.PubKey.Hex(),
		DTag:           ListIdentifier,
		SourceKind:     int(event.Kind),
		EventID:        event.ID.Hex(),
		CreatedAt:      int64(event.CreatedAt),
		RawEvent:       string(raw),
	}
	list := &authorList{
		author:       event.PubKey,
		revisionID:   state.EventID,
		createdAt:    state.CreatedAt,
		namesDrydock: names,
		repos:        repos,
	}
	switch {
	case parseErr != nil:
		list.status = StatusMalformed
		list.repos = nil
	case names:
		list.status = StatusActive
	default:
		list.status = StatusWithdrawn
	}
	for _, ref := range list.repos {
		state.Repositories = append(state.Repositories, db.MonitoredRepositoryMember{
			RepositoryAddress: ref.Address,
			RepositoryID:      ref.RepositoryID,
		})
	}

	// Resolve announcement authority before persisting so a lookup failure
	// leaves no half-applied state and the event is safe to redeliver.
	authorities, err := r.loadAuthorities(ctx, map[nostr.PubKey]*authorList{event.PubKey: list}, nil, r.authorities)
	if err != nil {
		return false, err
	}
	applied, err := r.store.ApplyMonitoredRepositoryList(ctx, state)
	if err != nil || !applied {
		if err == nil {
			err = parseErr
		}
		return false, err
	}
	r.lists[event.PubKey] = list
	r.authorities = authorities
	r.rebuildLocked()
	return true, parseErr
}

// ApplyDeletion applies a NIP-09 deletion of the event author's own list.
func (r *Registry) ApplyDeletion(ctx context.Context, event nostr.Event) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if event.Kind != DeletionKind {
		return false, fmt.Errorf("%w: kind must be %d", ErrMalformedDeletion, DeletionKind)
	}
	if event.PubKey == nostr.ZeroPK {
		return false, ErrUnauthorizedAuthor
	}
	if event.ID == nostr.ZeroID {
		return false, fmt.Errorf("%w: event id is required", ErrMalformedDeletion)
	}
	own := ListAddress(event.PubKey.Hex())
	targeted, foreign := false, false
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "a" {
			continue
		}
		switch {
		case tag[1] == own:
			targeted = true
		case isListAddress(tag[1]):
			foreign = true
		}
	}
	if !targeted {
		if foreign {
			return false, ErrUnauthorizedAuthor
		}
		return false, fmt.Errorf("%w: exact list address is required", ErrMalformedDeletion)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return false, fmt.Errorf("marshal monitored repository deletion: %w", err)
	}
	state := db.MonitoredRepositoryListState{
		ListAddress:    own,
		OperatorPubkey: event.PubKey.Hex(),
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
	r.lists[event.PubKey] = &authorList{
		author:     event.PubKey,
		status:     StatusDeleted,
		revisionID: state.EventID,
		createdAt:  state.CreatedAt,
	}
	r.rebuildLocked()
	return true, nil
}

// ApplyAnnouncement re-evaluates membership after a kind-30617 announcement was
// stored. The store owns replacement ordering, so the registry re-reads the
// authoritative winner for the announced repository instead of trusting event:
// a stale replay therefore changes nothing, and the call is idempotent, which
// makes redelivery after a failure safe.
func (r *Registry) ApplyAnnouncement(ctx context.Context, event nostr.Event) error {
	if !db.ValidRepositoryAnnouncement(event) {
		return nil
	}
	ref, err := scope.ParseRepositoryRef(fmt.Sprintf("%d:%s:%s",
		nostr.KindRepositoryAnnouncement, event.PubKey.Hex(), nip34.ParseRepository(event).ID))
	if err != nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.declaredLocked(ref.Address) {
		// Nothing declares it now, but a cached authority would be reused
		// verbatim if a list declares it again later; evict so the next
		// declaration re-reads the stored winner.
		delete(r.authorities, ref.Address)
		return nil
	}
	current, err := r.lookupAuthority(ctx, ref)
	if err != nil {
		// The store already holds the new announcement; keeping the old
		// authority would let a revoked maintainer stay enrolled until the
		// next successful refresh. Fail closed for this repository instead.
		delete(r.authorities, ref.Address)
		r.rebuildLocked()
		return err
	}
	r.authorities[ref.Address] = current
	r.rebuildLocked()
	return nil
}

// OperatorState returns the ceiling author, the rollback target, and whether a
// rollback target exists at all (ZeroPK previous with hasPrevious means "the
// ceiling was absent before the last change").
func (r *Registry) OperatorState() (current, previous nostr.PubKey, hasPrevious bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.operator, r.previous, r.hasPrevious
}

// SetOperatorState atomically installs ceiling state decided and persisted by
// the control plane, and re-derives membership in the same critical section:
// with a new operator whose list is not yet known, effective membership is
// empty (fail closed) until the list arrives through the resubscription this
// change signals. It carries no policy; scope-expansion acknowledgement and
// authorization live in the control plane, which owns the durable record.
func (r *Registry) SetOperatorState(current, previous nostr.PubKey, hasPrevious bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.operator, r.previous, r.hasPrevious = current, previous, hasPrevious
	r.rebuildLocked()
}

// MonitoredCount is the expiry-aware number of monitored repositories.
func (r *Registry) MonitoredCount() int {
	return len(r.MonitoredRepositoryIDs())
}

// Membership is the live monitored-repository membership projection consumed by
// the pipeline and the review-order service. *Registry is the production
// implementation; both consumers share this interface rather than each
// re-declaring a byte-identical one.
type Membership interface {
	Contains(repositoryAddress string) bool
}

func (r *Registry) Contains(repositoryAddress string) bool {
	ref, err := scope.ParseRepositoryRef(repositoryAddress)
	if err != nil {
		return false
	}
	current := r.snapshot.Load()
	if current == nil || !current.Initialized {
		return false
	}
	if _, ok := current.Repositories[ref.Address]; !ok {
		return false
	}
	if expiry, ok := current.Expiries[ref.Address]; ok && r.now().Unix() >= expiry {
		return false
	}
	return true
}

// MonitoredRepositoryIDs returns the "<pubkey>:<identifier>" repository ids of
// every currently monitored repository. It is used by the scheduled
// dependency-upgrade sweep to enumerate candidates; membership for the
// fail-closed gate still goes through Contains.
func (r *Registry) MonitoredRepositoryIDs() []string {
	current := r.snapshot.Load()
	if current == nil || !current.Initialized {
		return nil
	}
	now := r.now().Unix()
	ids := make([]string, 0, len(current.Repositories))
	for address := range current.Repositories {
		if expiry, ok := current.Expiries[address]; ok && now >= expiry {
			continue
		}
		ref, err := scope.ParseRepositoryRef(address)
		if err != nil {
			continue
		}
		ids = append(ids, ref.RepositoryID)
	}
	// Sort so the scheduled sweep processes repositories in a stable order
	// across runs (deterministic logging; map iteration is randomized).
	sort.Strings(ids)
	return ids
}

func (r *Registry) Snapshot() Snapshot {
	current := r.snapshot.Load()
	if current == nil {
		return Snapshot{Repositories: map[string]struct{}{}, Authors: map[string]AuthorSnapshot{}, Expiries: map[string]int64{}}
	}
	return cloneSnapshot(*current)
}

// Changes delivers a coalesced signal after every published rebuild: a list,
// deletion, or announcement was applied, so routing inputs may have changed.
// Receivers must re-read Snapshot/DeclaredRepositories rather than count
// signals.
func (r *Registry) Changes() <-chan struct{} {
	return r.changes
}

// DeclaredRepositories returns the repositories named by active lists,
// authorized or not, that fall inside the operator and static ceilings, sorted
// by address and bounded by a global budget. Routing uses it to recover the
// announcements that decide authority; ceilings apply first so a stranger
// cannot spend the budget on repositories this deployment could never monitor.
func (r *Registry) DeclaredRepositories() []scope.RepositoryRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ceiling map[string]struct{}
	if r.operator != nostr.ZeroPK {
		var seen bool
		if ceiling, seen = r.operatorCeilingLocked(); !seen {
			return nil
		}
	}
	declared := map[string]scope.RepositoryRef{}
	for address, ref := range r.declaredRefsLocked() {
		if ceiling != nil {
			if _, ok := ceiling[address]; !ok {
				continue
			}
		}
		if !r.scope.Allows(ref.RepositoryID, ref.OwnerPubkey) {
			continue
		}
		declared[address] = ref
	}
	refs := make([]scope.RepositoryRef, 0, len(declared))
	for _, ref := range declared {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Address < refs[j].Address })
	if len(refs) > maxDeclaredRepositories {
		refs = refs[:maxDeclaredRepositories]
	}
	return refs
}

// RoutedAuthors returns the list authors worth subscribing by author so their
// withdrawals and deletions arrive: authors that currently grant membership
// first, then other active authors, each group sorted, bounded by a budget.
func (r *Registry) RoutedAuthors() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var granting, other []string
	for author, list := range r.lists {
		if list.status != StatusActive {
			continue
		}
		grants := false
		for address := range list.repos {
			if r.authorizedLocked(author, address) {
				grants = true
				break
			}
		}
		if grants {
			granting = append(granting, author.Hex())
		} else {
			other = append(other, author.Hex())
		}
	}
	sort.Strings(granting)
	sort.Strings(other)
	authors := append(granting, other...)
	if len(authors) > maxRoutedAuthors {
		authors = authors[:maxRoutedAuthors]
	}
	return authors
}

// rebuildLocked recomputes effective membership from the list and authority
// state and publishes it atomically. Callers hold r.mu.
func (r *Registry) rebuildLocked() {
	// Authority is only meaningful for repositories some list still declares;
	// drop the rest so rotating lists cannot grow the cache without bound.
	for address := range r.authorities {
		if !r.declaredLocked(address) {
			delete(r.authorities, address)
		}
	}
	members := make(map[string]struct{})
	for author, list := range r.lists {
		if list.status != StatusActive {
			continue
		}
		for address := range list.repos {
			if r.authorizedLocked(author, address) {
				members[address] = struct{}{}
			}
		}
	}
	// Enrollments grant under the same authority rule; repositories granted
	// only by enrollments carry the latest granting expiry.
	now := r.now().Unix()
	expiries := map[string]int64{}
	for address, byAuthor := range r.enrollments {
		if _, permanent := members[address]; permanent {
			continue
		}
		var latest int64 = -1
		for author, e := range byAuthor {
			if !r.authorizedLocked(author, address) {
				continue
			}
			if e.ExpiresAt != 0 && now >= e.ExpiresAt {
				continue
			}
			if e.ExpiresAt == 0 {
				latest = 0
				break
			}
			if latest != 0 && e.ExpiresAt > latest {
				latest = e.ExpiresAt
			}
		}
		if latest < 0 {
			continue
		}
		members[address] = struct{}{}
		if latest > 0 {
			expiries[address] = latest
		}
	}

	initialized := r.loaded
	ceilingSeen := true
	if r.operator != nostr.ZeroPK {
		ceiling, seen := r.operatorCeilingLocked()
		ceilingSeen = seen
		initialized = initialized && seen
		for address := range members {
			if _, ok := ceiling[address]; !ok {
				delete(members, address)
			}
		}
	}
	for address := range members {
		ref, err := scope.ParseRepositoryRef(address)
		if err != nil || !r.scope.Allows(ref.RepositoryID, ref.OwnerPubkey) {
			delete(members, address)
		}
	}

	authors := make(map[string]AuthorSnapshot, len(r.lists))
	for author, list := range r.lists {
		authors[author.Hex()] = AuthorSnapshot{Status: list.status, RevisionID: list.revisionID, CreatedAt: list.createdAt}
	}
	for address := range expiries {
		if _, ok := members[address]; !ok {
			delete(expiries, address) // cut by a ceiling
		}
	}
	snapshot := &Snapshot{Initialized: initialized, Loaded: r.loaded, CeilingSeen: ceilingSeen, Repositories: members, Authors: authors, Expiries: expiries}
	if r.operator != nostr.ZeroPK {
		snapshot.Operator = r.operator.Hex()
	}
	if r.hasPrevious && r.previous != nostr.ZeroPK {
		snapshot.PreviousOperator = r.previous.Hex()
	}
	r.snapshot.Store(snapshot)
	select {
	case r.changes <- struct{}{}:
	default: // a signal is already pending; the receiver re-reads state anyway
	}
}

// operatorCeilingLocked returns the repositories the operator list names and
// whether an operator revision has been seen at all. A deleted or malformed
// operator list yields an empty ceiling: the operator switched monitoring off.
func (r *Registry) operatorCeilingLocked() (map[string]struct{}, bool) {
	list, ok := r.lists[r.operator]
	if !ok {
		return nil, false
	}
	ceiling := make(map[string]struct{}, len(list.repos))
	if list.status == StatusActive || list.status == StatusWithdrawn {
		for address := range list.repos {
			ceiling[address] = struct{}{}
		}
	}
	return ceiling, true
}

func (r *Registry) authorizedLocked(author nostr.PubKey, address string) bool {
	a, ok := r.authorities[address]
	if !ok || !a.known {
		return false
	}
	if author == a.owner {
		return true
	}
	_, ok = a.maintainers[author]
	return ok
}

func (r *Registry) declaredLocked(address string) bool {
	for _, list := range r.lists {
		if _, ok := list.repos[address]; ok {
			return true
		}
	}
	for _, e := range r.enrollments[address] {
		if e.ExpiresAt == 0 || r.now().Unix() < e.ExpiresAt {
			return true
		}
	}
	return false
}

// declaredRefsLocked is every repository named by an active list or an
// enrollment, keyed by address. Callers hold r.mu.
func (r *Registry) declaredRefsLocked() map[string]scope.RepositoryRef {
	out := map[string]scope.RepositoryRef{}
	for _, list := range r.lists {
		if list.status != StatusActive {
			continue
		}
		for address, ref := range list.repos {
			out[address] = ref
		}
	}
	now := r.now().Unix()
	for address, byAuthor := range r.enrollments {
		for _, e := range byAuthor {
			if e.ExpiresAt == 0 || now < e.ExpiresAt {
				out[address] = e.Ref
				break
			}
		}
	}
	return out
}

// loadAuthorities returns the authority map covering every repository declared
// by lists, reusing entries already present in cached.
func (r *Registry) loadAuthorities(ctx context.Context, lists map[nostr.PubKey]*authorList, enrollments map[string]map[nostr.PubKey]Enrollment, cached map[string]authority) (map[string]authority, error) {
	out := make(map[string]authority, len(cached))
	for address, a := range cached {
		out[address] = a
	}
	need := map[string]scope.RepositoryRef{}
	for _, list := range lists {
		for address, ref := range list.repos {
			need[address] = ref
		}
	}
	for address, byAuthor := range enrollments {
		for _, e := range byAuthor {
			need[address] = e.Ref
			break
		}
	}
	for address, ref := range need {
		if _, ok := out[address]; ok {
			continue
		}
		a, err := r.lookupAuthority(ctx, ref)
		if err != nil {
			return nil, err
		}
		out[address] = a
	}
	return out, nil
}

// lookupAuthority derives owner/maintainer authority from the stored winning
// announcement. No announcement, or one that does not belong to ref, grants no
// authority.
func (r *Registry) lookupAuthority(ctx context.Context, ref scope.RepositoryRef) (authority, error) {
	event, ok, err := r.store.LoadRepositoryAnnouncement(ctx, ref.RepositoryID)
	if err != nil {
		return authority{}, err
	}
	if !ok || !db.ValidRepositoryAnnouncement(event) || event.PubKey.Hex() != ref.OwnerPubkey ||
		strings.TrimSpace(nip34.ParseRepository(event).ID) != ref.Identifier {
		return authority{}, nil
	}
	maintainers := make(map[nostr.PubKey]struct{})
	for _, maintainer := range nip34.ParseRepository(event).Maintainers {
		maintainers[maintainer] = struct{}{}
	}
	return authority{known: true, owner: event.PubKey, maintainers: maintainers}, nil
}

// listFromState rebuilds an author's in-memory list from its persisted winner,
// re-parsing the stored event so recovery and live application share one parser.
func (r *Registry) listFromState(state db.MonitoredRepositoryListState) (*authorList, error) {
	return listFromPersisted(state, r.drydock)
}

// listFromPersisted is the single interpretation of a persisted list revision
// (status, whether it names drydock, declared repositories). The inventory
// uses it too, so a report can never disagree with the registry about what a
// stored list means.
func listFromPersisted(state db.MonitoredRepositoryListState, drydock nostr.PubKey) (*authorList, error) {
	author, err := scope.ParsePubkey(state.OperatorPubkey)
	if err != nil || state.ListAddress != ListAddress(author.Hex()) || state.DTag != ListIdentifier {
		return nil, errors.New("persisted monitored repository list identity does not match configuration")
	}
	list := &authorList{author: author, revisionID: state.EventID, createdAt: state.CreatedAt}
	if state.Deleted {
		list.status = StatusDeleted
		return list, nil
	}
	var event nostr.Event
	if err := json.Unmarshal([]byte(state.RawEvent), &event); err != nil ||
		event.Kind != MonitoredListKind || event.PubKey != author || event.ID.Hex() != state.EventID {
		return nil, errors.New("persisted monitored repository list event does not match its record")
	}
	names, repos, parseErr := parseListTags(event, drydock)
	list.namesDrydock = names
	switch {
	case parseErr != nil:
		list.status = StatusMalformed
	case names:
		list.status = StatusActive
		list.repos = repos
	default:
		list.status = StatusWithdrawn
		list.repos = repos
	}
	return list, nil
}

// parseListTags extracts whether the list names Drydock and which repositories
// it declares. Any unusable `a` or `p` tag, or an oversized list, makes the
// whole revision malformed.
func parseListTags(event nostr.Event, drydock nostr.PubKey) (bool, map[string]scope.RepositoryRef, error) {
	names := false
	repos := make(map[string]scope.RepositoryRef)
	for _, tag := range event.Tags {
		if len(tag) == 0 {
			continue
		}
		switch tag[0] {
		case "a":
			if len(tag) < 2 {
				return false, nil, fmt.Errorf("%w: repository a tag is missing an address", ErrMalformedList)
			}
			ref, err := scope.ParseRepositoryRef(tag[1])
			if err != nil {
				return false, nil, fmt.Errorf("%w: %v", ErrMalformedList, err)
			}
			repos[ref.Address] = ref
		case "p":
			if len(tag) < 2 {
				return false, nil, fmt.Errorf("%w: p tag is missing a pubkey", ErrMalformedList)
			}
			pubkey, err := nostr.PubKeyFromHex(tag[1])
			if err != nil {
				return false, nil, fmt.Errorf("%w: p tag is not a hex pubkey", ErrMalformedList)
			}
			if pubkey == drydock {
				names = true
			}
		}
	}
	if len(repos) > maxListRepositories {
		return false, nil, fmt.Errorf("%w: more than %d repositories", ErrMalformedList, maxListRepositories)
	}
	return names, repos, nil
}

func hasListIdentifier(event nostr.Event) bool {
	count := 0
	value := ""
	for _, tag := range event.Tags {
		if len(tag) >= 1 && tag[0] == "d" {
			count++
			if len(tag) >= 2 {
				value = tag[1]
			}
		}
	}
	return count == 1 && value == ListIdentifier
}

// isListAddress reports whether address has the shape of any author's list.
// ListIdentifier itself contains colons, so match by prefix and suffix rather
// than by splitting.
func isListAddress(address string) bool {
	prefix := fmt.Sprintf("%d:", MonitoredListKind)
	suffix := ":" + ListIdentifier
	if !strings.HasPrefix(address, prefix) || !strings.HasSuffix(address, suffix) {
		return false
	}
	_, err := nostr.PubKeyFromHex(strings.TrimSuffix(strings.TrimPrefix(address, prefix), suffix))
	return err == nil
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	clone := Snapshot{
		Initialized:      snapshot.Initialized,
		Loaded:           snapshot.Loaded,
		Operator:         snapshot.Operator,
		PreviousOperator: snapshot.PreviousOperator,
		CeilingSeen:      snapshot.CeilingSeen,
		Repositories:     make(map[string]struct{}, len(snapshot.Repositories)),
		Authors:          make(map[string]AuthorSnapshot, len(snapshot.Authors)),
		Expiries:         make(map[string]int64, len(snapshot.Expiries)),
	}
	for address, expiry := range snapshot.Expiries {
		clone.Expiries[address] = expiry
	}
	for address := range snapshot.Repositories {
		clone.Repositories[address] = struct{}{}
	}
	for author, state := range snapshot.Authors {
		clone.Authors[author] = state
	}
	return clone
}
