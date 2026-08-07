package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
)

// SecurityFindingFingerprint returns a stable fingerprint for a security
// finding. It intentionally excludes the line number so findings survive line
// drift, and normalizes path separators and code whitespace before hashing.
func SecurityFindingFingerprint(file, cwe, nearbyCode string) string {
	normalizedPath := strings.ReplaceAll(strings.TrimSpace(file), "\\", "/")
	normalizedPath = path.Clean(normalizedPath)
	if normalizedPath == "." {
		normalizedPath = ""
	}
	normalizedPath = strings.TrimPrefix(normalizedPath, "./")

	normalizedCWE := strings.ToUpper(strings.TrimSpace(cwe))
	codeShape := strings.Join(strings.Fields(nearbyCode), " ")

	sum := sha256.Sum256([]byte(normalizedPath + "\x00" + normalizedCWE + "\x00" + codeShape))
	return hex.EncodeToString(sum[:])
}

// ResetStuckAudits transitions audits stuck in "running" (e.g. from a crash)
// back to "pending" so they can be retried.
func (s *Store) ResetStuckAudits(ctx context.Context) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE security_audits SET state='pending', updated_at=?
		  WHERE state='running'`, now)
	if err != nil {
		return 0, fmt.Errorf("reset stuck audits: %w", err)
	}
	return res.RowsAffected()
}

// SecurityAuditFinding is the normalized database representation of one
// verified audit finding.
type SecurityAuditFinding struct {
	File        string
	Line        int
	CWE         string
	Severity    string
	Confidence  float64
	Verified    bool
	RefuteVotes int
	Fingerprint string
}

// CreateSecurityAudit inserts a pending audit and returns its database ID.
func (s *Store) CreateSecurityAudit(ctx context.Context, repoID, ref, depth, requestedBy string) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO security_audits(repo_id, ref, depth, requested_by, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'pending', ?, ?)`, repoID, ref, depth, requestedBy, now, now)
	if err != nil {
		return 0, fmt.Errorf("create security audit: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("security audit id: %w", err)
	}
	return id, nil
}

// StartSecurityAudit performs the pending -> running transition.
func (s *Store) StartSecurityAudit(ctx context.Context, id int64) error {
	return s.transitionSecurityAudit(ctx, id, "pending", "running", "", "")
}

// PublishSecurityAudit performs the running -> published transition and stores
// the public report and SARIF identifiers.
func (s *Store) PublishSecurityAudit(ctx context.Context, id int64, reportEventID, sarifHash string) error {
	return s.transitionSecurityAudit(ctx, id, "running", "published", reportEventID, sarifHash)
}

// FailSecurityAudit performs the running -> failed transition.
func (s *Store) FailSecurityAudit(ctx context.Context, id int64) error {
	return s.transitionSecurityAudit(ctx, id, "running", "failed", "", "")
}

func (s *Store) transitionSecurityAudit(ctx context.Context, id int64, from, to, reportEventID, sarifHash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE security_audits
		SET state=?, report_event_id=CASE WHEN ?='' THEN report_event_id ELSE ? END,
		sarif_hash=CASE WHEN ?='' THEN sarif_hash ELSE ? END, updated_at=?
		WHERE id=? AND state=?`,
		to, reportEventID, reportEventID, sarifHash, sarifHash, time.Now().Unix(), id, from)
	if err != nil {
		return fmt.Errorf("transition security audit %d %s->%s: %w", id, from, to, err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("transition security audit %d: %w", id, err)
	}
	if changed != 1 {
		return fmt.Errorf("transition security audit %d %s->%s: invalid current state", id, from, to)
	}
	return nil
}

// ReplaceSecurityAuditFindings atomically replaces the findings for an audit.
func (s *Store) ReplaceSecurityAuditFindings(ctx context.Context, auditID int64, findings []SecurityAuditFinding) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin security findings transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM security_findings WHERE audit_id=?`, auditID); err != nil {
		return fmt.Errorf("clear security findings: %w", err)
	}
	for _, finding := range findings {
		verified := 0
		if finding.Verified {
			verified = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO security_findings(
			audit_id, file, line, cwe, severity, confidence, verified, refute_votes, fingerprint
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			auditID, finding.File, finding.Line, finding.CWE, finding.Severity,
			finding.Confidence, verified, finding.RefuteVotes, finding.Fingerprint); err != nil {
			return fmt.Errorf("insert security finding: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit security findings: %w", err)
	}
	return nil
}

// SecurityBaselineFingerprints returns accepted/known fingerprints for repoID.
func (s *Store) SecurityBaselineFingerprints(ctx context.Context, repoID string) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT fingerprint FROM security_baseline WHERE repo_id=?`, repoID)
	if err != nil {
		return nil, fmt.Errorf("query security baseline: %w", err)
	}
	defer rows.Close()
	fingerprints := make(map[string]struct{})
	for rows.Next() {
		var fingerprint string
		if err := rows.Scan(&fingerprint); err != nil {
			return nil, fmt.Errorf("scan security baseline: %w", err)
		}
		fingerprints[fingerprint] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read security baseline: %w", err)
	}
	return fingerprints, nil
}
