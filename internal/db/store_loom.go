package db

import (
	"context"
	"database/sql"
	"fmt"
)

func migrateLoomCI(ctx context.Context, tx *sql.Tx) error {
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS loom_runs (
   event_id TEXT PRIMARY KEY, repo_id TEXT NOT NULL, repository_addr TEXT NOT NULL,
   commit_ref TEXT NOT NULL, author TEXT NOT NULL, publisher TEXT NOT NULL,
   job_id TEXT NOT NULL DEFAULT '', worker TEXT NOT NULL DEFAULT '', job_created_at INTEGER NOT NULL DEFAULT 0,
   created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_loom_runs_repo ON loom_runs(repo_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS ci_attestations (
   event_id TEXT PRIMARY KEY, repo_id TEXT NOT NULL, commit_ref TEXT NOT NULL,
   patch_ref TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL, worker TEXT NOT NULL,
   signer TEXT NOT NULL, claim_type TEXT NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_ci_attestations_target ON ci_attestations(repo_id, commit_ref, patch_ref)`,
	} {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("apply loom ci ddl: %w", err)
		}
	}
	return nil
}

// LoomRun is bounded subscription lineage, not an authorization record.
// Only public routing metadata is retained; job arguments/payment/secrets are not copied here.
type LoomRun struct {
	EventID, RepoID, RepositoryAddress, CommitRef, Author, Publisher string
	JobID, Worker                                                    string
	JobCreatedAt, CreatedAt, ExpiresAt                               int64
}

type CIAttestation struct {
	EventID, RepoID, CommitRef, PatchRef, RunID, Worker, Signer, ClaimType, Status string
	CreatedAt                                                                      int64
}

func (s *Store) TrackLoomRun(ctx context.Context, r LoomRun, perRepo, total int, now int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin loom run: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO loom_runs
 (event_id, repo_id, repository_addr, commit_ref, author, publisher, created_at, expires_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(event_id) DO NOTHING`,
		r.EventID, r.RepoID, r.RepositoryAddress, r.CommitRef, r.Author, r.Publisher, r.CreatedAt, r.ExpiresAt); err != nil {
		return 0, fmt.Errorf("insert loom run: %w", err)
	}
	removed, err := boundLoomRuns(ctx, tx, perRepo, total, now)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit loom run: %w", err)
	}
	return removed, nil
}

func boundLoomRuns(ctx context.Context, tx *sql.Tx, perRepo, total int, now int64) (int64, error) {
	var removed int64
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM loom_runs WHERE expires_at <= ?`, []any{now}},
		{`DELETE FROM loom_runs WHERE event_id IN (SELECT event_id FROM (
   SELECT event_id, ROW_NUMBER() OVER (PARTITION BY repo_id ORDER BY created_at DESC, event_id ASC) AS n FROM loom_runs) WHERE n > ?)`, []any{perRepo}},
		{`DELETE FROM loom_runs WHERE event_id IN (SELECT event_id FROM loom_runs ORDER BY created_at DESC, event_id ASC LIMIT -1 OFFSET ?)`, []any{total}},
	} {
		result, err := tx.ExecContext(ctx, q.sql, q.args...)
		if err != nil {
			return 0, fmt.Errorf("bound loom runs: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count bounded loom runs: %w", err)
		}
		removed += n
	}
	return removed, nil
}

func (s *Store) PruneLoomRuns(ctx context.Context, perRepo, total int, now int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin loom pruning: %w", err)
	}
	defer tx.Rollback()
	removed, err := boundLoomRuns(ctx, tx, perRepo, total, now)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit loom pruning: %w", err)
	}
	return removed, nil
}

func (s *Store) ListLoomRuns(ctx context.Context, now int64) ([]LoomRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, repo_id, repository_addr, commit_ref, author, publisher,
 job_id, worker, job_created_at, created_at, expires_at FROM loom_runs WHERE expires_at > ? ORDER BY created_at DESC, event_id ASC`, now)
	if err != nil {
		return nil, fmt.Errorf("query loom runs: %w", err)
	}
	defer rows.Close()
	var out []LoomRun
	for rows.Next() {
		var r LoomRun
		if err := rows.Scan(&r.EventID, &r.RepoID, &r.RepositoryAddress, &r.CommitRef, &r.Author, &r.Publisher,
			&r.JobID, &r.Worker, &r.JobCreatedAt, &r.CreatedAt, &r.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan loom run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate loom runs: %w", err)
	}
	return out, nil
}

func (s *Store) AttachLoomJob(ctx context.Context, runID, jobID, worker string, createdAt int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE loom_runs SET job_id=?, worker=?, job_created_at=? WHERE event_id=?
 AND (job_id='' OR job_created_at < ? OR (job_created_at = ? AND job_id > ?))`,
		jobID, worker, createdAt, runID, createdAt, createdAt, jobID); err != nil {
		return fmt.Errorf("attach loom job: %w", err)
	}
	return nil
}

// CompleteLoomRun stores the claim and evicts its live lineage atomically.
func (s *Store) CompleteLoomRun(ctx context.Context, a CIAttestation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ci attestation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO ci_attestations
 (event_id,repo_id,commit_ref,patch_ref,run_id,worker,signer,claim_type,status,created_at)
 VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`,
		a.EventID, a.RepoID, a.CommitRef, a.PatchRef, a.RunID, a.Worker, a.Signer, a.ClaimType, a.Status, a.CreatedAt); err != nil {
		return fmt.Errorf("insert ci attestation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM loom_runs WHERE event_id=?`, a.RunID); err != nil {
		return fmt.Errorf("complete loom run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ci attestation: %w", err)
	}
	return nil
}

func (s *Store) CIAttestationsForPatch(ctx context.Context, repoID, commit, patchID string) ([]CIAttestation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event_id,repo_id,commit_ref,patch_ref,run_id,worker,signer,claim_type,status,created_at
 FROM ci_attestations WHERE repo_id=? AND ((commit_ref<>'' AND commit_ref=?) OR (patch_ref<>'' AND patch_ref=?))
 ORDER BY created_at DESC,event_id ASC LIMIT 16`, repoID, commit, patchID)
	if err != nil {
		return nil, fmt.Errorf("query ci attestations: %w", err)
	}
	defer rows.Close()
	var out []CIAttestation
	for rows.Next() {
		var a CIAttestation
		if err := rows.Scan(&a.EventID, &a.RepoID, &a.CommitRef, &a.PatchRef, &a.RunID, &a.Worker, &a.Signer, &a.ClaimType, &a.Status, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan ci attestation: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ci attestations: %w", err)
	}
	return out, nil
}
