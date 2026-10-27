package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MonitoringEnrollment is the durable state of one (repository, author)
// enrollment relationship, received over ContextVM. The row always reflects
// the newest command — an enrollment or a revocation — so a replayed older
// command of either kind can never undo a newer one. RequestEventID and
// RequestCreatedAt identify that winning command. For a revocation the row is
// a tombstone: RevokedAt is set and RevokeEventID equals RequestEventID.
//
// It is provenance: the request event id, the announcement revision whose
// authority was verified, and the payment decision are kept so the enrollment
// can be audited and reconciled.
type MonitoringEnrollment struct {
	RepositoryAddress   string
	AuthorPubkey        string
	RequestEventID      string
	RequestCreatedAt    int64
	AnnouncementEventID string
	AccessKind          string
	ExpiresAt           int64 // 0 = no expiry
	RevokedAt           int64 // 0 = active enrollment; otherwise a tombstone
	RevokeEventID       string
	UpdatedAt           int64
}

// Active reports whether the enrollment grants at now.
func (e MonitoringEnrollment) Active(now int64) bool {
	return e.RevokedAt == 0 && (e.ExpiresAt == 0 || now < e.ExpiresAt)
}

// Supersedes reports whether a command (eventID, createdAt, revoke) is newer
// than the stored winning command. Later created_at wins. On equal timestamps
// a revocation beats an enrollment (fail closed, and deterministic regardless
// of delivery order); between commands of the same kind the lower event id
// wins. The same command never supersedes itself.
func (e MonitoringEnrollment) Supersedes(eventID string, createdAt int64, revoke bool) bool {
	if eventID == e.RequestEventID {
		return false
	}
	if createdAt != e.RequestCreatedAt {
		return createdAt > e.RequestCreatedAt
	}
	storedRevoke := e.RevokedAt != 0
	if revoke != storedRevoke {
		return revoke
	}
	return eventID < e.RequestEventID
}

// ApplyMonitoringEnrollment records an enrollment command for (repository,
// author) if it supersedes the stored command (enrollment or revocation). It
// reports false, without error, when the command is a replay or older than
// what is stored, so callers can answer idempotently from the stored row.
func (s *Store) ApplyMonitoringEnrollment(ctx context.Context, e MonitoringEnrollment) (bool, error) {
	e.RepositoryAddress = strings.TrimSpace(e.RepositoryAddress)
	e.AuthorPubkey = strings.TrimSpace(e.AuthorPubkey)
	e.RequestEventID = strings.TrimSpace(e.RequestEventID)
	if e.RepositoryAddress == "" || e.AuthorPubkey == "" || e.RequestEventID == "" || e.AnnouncementEventID == "" {
		return false, errors.New("monitoring enrollment is incomplete")
	}
	return s.applyEnrollmentCommand(ctx, e.RepositoryAddress, e.AuthorPubkey, e.RequestEventID, e.RequestCreatedAt, false,
		func(tx *sql.Tx, now int64) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO monitoring_enrollments(
					repository_addr, author_pubkey, request_event_id, request_created_at,
					announcement_event_id, access_kind, expires_at, revoked_at, revoke_event_id,
					created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?)
				ON CONFLICT(repository_addr, author_pubkey) DO UPDATE SET
					request_event_id=excluded.request_event_id,
					request_created_at=excluded.request_created_at,
					announcement_event_id=excluded.announcement_event_id,
					access_kind=excluded.access_kind,
					expires_at=excluded.expires_at,
					revoked_at=0,
					revoke_event_id='',
					updated_at=excluded.updated_at`,
				e.RepositoryAddress, e.AuthorPubkey, e.RequestEventID, e.RequestCreatedAt,
				e.AnnouncementEventID, e.AccessKind, e.ExpiresAt, now, now)
			return err
		})
}

// RevokeMonitoringEnrollment records a revocation command for (repository,
// author) if it supersedes the stored command. A revocation with no prior
// enrollment still leaves a tombstone so an older enrollment delivered later
// cannot activate. It reports false when the revocation is a replay or stale.
func (s *Store) RevokeMonitoringEnrollment(ctx context.Context, repositoryAddress, authorPubkey, revokeEventID string, revokedAt int64) (bool, error) {
	repositoryAddress = strings.TrimSpace(repositoryAddress)
	authorPubkey = strings.TrimSpace(authorPubkey)
	revokeEventID = strings.TrimSpace(revokeEventID)
	if repositoryAddress == "" || authorPubkey == "" || revokeEventID == "" {
		return false, errors.New("monitoring revocation is incomplete")
	}
	return s.applyEnrollmentCommand(ctx, repositoryAddress, authorPubkey, revokeEventID, revokedAt, true,
		func(tx *sql.Tx, now int64) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO monitoring_enrollments(
					repository_addr, author_pubkey, request_event_id, request_created_at,
					announcement_event_id, access_kind, expires_at, revoked_at, revoke_event_id,
					created_at, updated_at
				) VALUES (?, ?, ?, ?, '', '', 0, ?, ?, ?, ?)
				ON CONFLICT(repository_addr, author_pubkey) DO UPDATE SET
					request_event_id=excluded.request_event_id,
					request_created_at=excluded.request_created_at,
					revoked_at=excluded.revoked_at,
					revoke_event_id=excluded.revoke_event_id,
					updated_at=excluded.updated_at`,
				repositoryAddress, authorPubkey, revokeEventID, revokedAt, revokedAt, revokeEventID, now, now)
			return err
		})
}

// applyEnrollmentCommand runs the shared compare-then-write for both command
// kinds inside one transaction.
func (s *Store) applyEnrollmentCommand(ctx context.Context, repositoryAddress, authorPubkey, eventID string, createdAt int64, revoke bool, write func(tx *sql.Tx, now int64) error) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin monitoring enrollment transaction: %w", err)
	}
	defer tx.Rollback()

	var current MonitoringEnrollment
	err = tx.QueryRowContext(ctx, `SELECT request_event_id, request_created_at, revoked_at
		FROM monitoring_enrollments WHERE repository_addr=? AND author_pubkey=?`,
		repositoryAddress, authorPubkey).Scan(&current.RequestEventID, &current.RequestCreatedAt, &current.RevokedAt)
	switch {
	case err == nil:
		if !current.Supersedes(eventID, createdAt, revoke) {
			return false, nil // replay, older, or a tie lost on event id
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return false, fmt.Errorf("load monitoring enrollment: %w", err)
	}
	if err := write(tx, time.Now().Unix()); err != nil {
		return false, fmt.Errorf("store monitoring enrollment command: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit monitoring enrollment command: %w", err)
	}
	return true, nil
}

// GetMonitoringEnrollment returns the stored row for (repository, author),
// which may be a revocation tombstone.
func (s *Store) GetMonitoringEnrollment(ctx context.Context, repositoryAddress, authorPubkey string) (MonitoringEnrollment, bool, error) {
	var e MonitoringEnrollment
	err := s.db.QueryRowContext(ctx, `SELECT repository_addr, author_pubkey, request_event_id, request_created_at,
			announcement_event_id, access_kind, expires_at, revoked_at, revoke_event_id, updated_at
		FROM monitoring_enrollments WHERE repository_addr=? AND author_pubkey=?`,
		strings.TrimSpace(repositoryAddress), strings.TrimSpace(authorPubkey)).Scan(
		&e.RepositoryAddress, &e.AuthorPubkey, &e.RequestEventID, &e.RequestCreatedAt,
		&e.AnnouncementEventID, &e.AccessKind, &e.ExpiresAt, &e.RevokedAt, &e.RevokeEventID, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MonitoringEnrollment{}, false, nil
	}
	if err != nil {
		return MonitoringEnrollment{}, false, fmt.Errorf("load monitoring enrollment: %w", err)
	}
	return e, true, nil
}

// LoadMonitoringEnrollments returns every stored row, tombstones included,
// ordered by repository then author.
func (s *Store) LoadMonitoringEnrollments(ctx context.Context) ([]MonitoringEnrollment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository_addr, author_pubkey, request_event_id, request_created_at,
			announcement_event_id, access_kind, expires_at, revoked_at, revoke_event_id, updated_at
		FROM monitoring_enrollments ORDER BY repository_addr, author_pubkey`)
	if err != nil {
		return nil, fmt.Errorf("load monitoring enrollments: %w", err)
	}
	defer rows.Close()
	var out []MonitoringEnrollment
	for rows.Next() {
		var e MonitoringEnrollment
		if err := rows.Scan(&e.RepositoryAddress, &e.AuthorPubkey, &e.RequestEventID, &e.RequestCreatedAt,
			&e.AnnouncementEventID, &e.AccessKind, &e.ExpiresAt, &e.RevokedAt, &e.RevokeEventID, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan monitoring enrollment: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate monitoring enrollments: %w", err)
	}
	return out, nil
}
