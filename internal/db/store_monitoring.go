package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MonitoredRepositoryMember is one canonical repository in a monitored list.
type MonitoredRepositoryMember struct {
	RepositoryAddress string
	RepositoryID      string
}

// MonitoredRepositoryListState is the durable winning list revision.
type MonitoredRepositoryListState struct {
	ListAddress    string
	OperatorPubkey string
	DTag           string
	SourceKind     int
	EventID        string
	CreatedAt      int64
	Deleted        bool
	RawEvent       string
	UpdatedAt      int64
	Repositories   []MonitoredRepositoryMember
}

// ApplyMonitoredRepositoryList atomically replaces the winning revision and
// its complete member projection. Newer created_at wins; equal timestamps use
// the lexicographically lower event ID.
func (s *Store) ApplyMonitoredRepositoryList(ctx context.Context, state MonitoredRepositoryListState) (bool, error) {
	state.ListAddress = strings.TrimSpace(state.ListAddress)
	state.OperatorPubkey = strings.TrimSpace(state.OperatorPubkey)
	state.DTag = strings.TrimSpace(state.DTag)
	state.EventID = strings.TrimSpace(state.EventID)
	if state.ListAddress == "" || state.OperatorPubkey == "" || state.DTag == "" || state.EventID == "" {
		return false, errors.New("monitored repository list state is incomplete")
	}
	if state.Deleted && len(state.Repositories) != 0 {
		return false, errors.New("deleted monitored repository list cannot contain members")
	}

	members := make(map[string]string, len(state.Repositories))
	for _, member := range state.Repositories {
		address := strings.TrimSpace(member.RepositoryAddress)
		repositoryID := strings.TrimSpace(member.RepositoryID)
		if address == "" || repositoryID == "" {
			return false, errors.New("monitored repository member is incomplete")
		}
		if existing, ok := members[address]; ok && existing != repositoryID {
			return false, fmt.Errorf("conflicting monitored repository member %q", address)
		}
		members[address] = repositoryID
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin monitored repository transaction: %w", err)
	}
	defer tx.Rollback()

	var currentID string
	var currentCreatedAt int64
	err = tx.QueryRowContext(ctx, `SELECT event_id, created_at
		FROM monitored_repository_list_state WHERE list_address=?`,
		state.ListAddress,
	).Scan(&currentID, &currentCreatedAt)
	switch {
	case err == nil:
		if state.EventID == currentID ||
			state.CreatedAt < currentCreatedAt ||
			(state.CreatedAt == currentCreatedAt && state.EventID > currentID) {
			return false, nil
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return false, fmt.Errorf("load monitored repository winner: %w", err)
	}

	now := time.Now().Unix()
	if state.UpdatedAt > 0 {
		now = state.UpdatedAt
	}
	deleted := 0
	if state.Deleted {
		deleted = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO monitored_repository_list_state(
		list_address, operator_pubkey, d_tag, source_kind, event_id,
		created_at, deleted, raw_event, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(list_address) DO UPDATE SET
		operator_pubkey=excluded.operator_pubkey,
		d_tag=excluded.d_tag,
		source_kind=excluded.source_kind,
		event_id=excluded.event_id,
		created_at=excluded.created_at,
		deleted=excluded.deleted,
		raw_event=excluded.raw_event,
		updated_at=excluded.updated_at`,
		state.ListAddress, state.OperatorPubkey, state.DTag, state.SourceKind,
		state.EventID, state.CreatedAt, deleted, state.RawEvent, now,
	); err != nil {
		return false, fmt.Errorf("store monitored repository winner: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM monitored_repository_members WHERE list_address=?`,
		state.ListAddress,
	); err != nil {
		return false, fmt.Errorf("clear monitored repository members: %w", err)
	}

	addresses := make([]string, 0, len(members))
	for address := range members {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	for _, address := range addresses {
		if _, err := tx.ExecContext(ctx, `INSERT INTO monitored_repository_members(
			list_address, repository_addr, repository_id
		) VALUES (?, ?, ?)`, state.ListAddress, address, members[address]); err != nil {
			return false, fmt.Errorf("store monitored repository member: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit monitored repository transaction: %w", err)
	}
	return true, nil
}

// LoadMonitoredRepositoryList loads the persisted winning revision and members.
func (s *Store) LoadMonitoredRepositoryList(ctx context.Context, listAddress string) (MonitoredRepositoryListState, bool, error) {
	var state MonitoredRepositoryListState
	var deleted int
	err := s.db.QueryRowContext(ctx, `SELECT
		list_address, operator_pubkey, d_tag, source_kind, event_id,
		created_at, deleted, raw_event, updated_at
		FROM monitored_repository_list_state WHERE list_address=?`,
		strings.TrimSpace(listAddress),
	).Scan(
		&state.ListAddress, &state.OperatorPubkey, &state.DTag, &state.SourceKind,
		&state.EventID, &state.CreatedAt, &deleted, &state.RawEvent, &state.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return MonitoredRepositoryListState{}, false, nil
	}
	if err != nil {
		return MonitoredRepositoryListState{}, false, fmt.Errorf("load monitored repository list: %w", err)
	}
	state.Deleted = deleted != 0

	rows, err := s.db.QueryContext(ctx, `SELECT repository_addr, repository_id
		FROM monitored_repository_members WHERE list_address=?
		ORDER BY repository_addr`, state.ListAddress)
	if err != nil {
		return MonitoredRepositoryListState{}, false, fmt.Errorf("load monitored repository members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var member MonitoredRepositoryMember
		if err := rows.Scan(&member.RepositoryAddress, &member.RepositoryID); err != nil {
			return MonitoredRepositoryListState{}, false, fmt.Errorf("scan monitored repository member: %w", err)
		}
		state.Repositories = append(state.Repositories, member)
	}
	if err := rows.Err(); err != nil {
		return MonitoredRepositoryListState{}, false, fmt.Errorf("iterate monitored repository members: %w", err)
	}
	return state, true, nil
}
