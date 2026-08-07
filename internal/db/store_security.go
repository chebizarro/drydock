package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"fiatjaf.com/nostr"
)

const (
	SecurityAuditPublicationReport   = "report"
	SecurityAuditPublicationDetail   = "detail"
	SecurityAuditPublicationFallback = "fallback"
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

// SecurityAuditCoverage records scan operations and model-review units omitted
// by a depth budget. Scan counts aggregate deterministic and surface passes.
type SecurityAuditCoverage struct {
	ScanOperationsScanned int
	ScanOperationsSkipped int
	ScanOperationsErrored int
	UnitsDropped          int
}

// SecurityAuditPublication is one signed event in the durable audit outbox.
type SecurityAuditPublication struct {
	AuditID   int64
	EventType string
	Event     nostr.Event
	Relays    []string
	Delivered bool
}

// SecurityAuditPublicationSet is the atomically persisted publication payload
// for an audit, including the durable SARIF artifact.
type SecurityAuditPublicationSet struct {
	AuditID      int64
	SARIFHash    string
	SARIF        []byte
	Publications []SecurityAuditPublication
}

// ResetStuckAudits recovers audits interrupted by a crash. Audits with a
// durable publication outbox become pending for startup delivery recovery;
// earlier crashes have no durable request to replay and are marked failed
// instead of being stranded in an unconsumed pending state.
func (s *Store) ResetStuckAudits(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin stuck audit recovery: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	recoverable, err := tx.ExecContext(ctx, `UPDATE security_audits
		SET state='pending', updated_at=?
		WHERE state='running'
		AND EXISTS (SELECT 1 FROM security_audit_publication_outbox o WHERE o.audit_id=security_audits.id)`, now)
	if err != nil {
		return 0, fmt.Errorf("reset publishable stuck audits: %w", err)
	}
	failed, err := tx.ExecContext(ctx, `UPDATE security_audits
		SET state='failed', updated_at=?
		WHERE state='running'`, now)
	if err != nil {
		return 0, fmt.Errorf("fail non-recoverable stuck audits: %w", err)
	}
	recoverableCount, err := recoverable.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count publishable stuck audits: %w", err)
	}
	failedCount, err := failed.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count non-recoverable stuck audits: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit stuck audit recovery: %w", err)
	}
	return recoverableCount + failedCount, nil
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

// UpdateSecurityAuditCoverage persists the latest known audit coverage.
func (s *Store) UpdateSecurityAuditCoverage(ctx context.Context, id int64, coverage SecurityAuditCoverage) error {
	if coverage.ScanOperationsScanned < 0 || coverage.ScanOperationsSkipped < 0 || coverage.ScanOperationsErrored < 0 || coverage.UnitsDropped < 0 {
		return fmt.Errorf("update security audit %d coverage: negative count", id)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE security_audits
		SET scan_operations_scanned=?, scan_operations_skipped=?, scan_operations_errored=?, units_dropped=?, updated_at=?
		WHERE id=?`,
		coverage.ScanOperationsScanned, coverage.ScanOperationsSkipped, coverage.ScanOperationsErrored, coverage.UnitsDropped,
		time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("update security audit %d coverage: %w", id, err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update security audit %d coverage: %w", id, err)
	}
	if changed != 1 {
		return fmt.Errorf("update security audit %d coverage: audit not found", id)
	}
	return nil
}

// PublishSecurityAudit performs the running -> published transition and stores
// the public report and SARIF identifiers. Repeating the same completed
// transition is safe so startup outbox recovery can race-free hand off to the
// engine.
func (s *Store) PublishSecurityAudit(ctx context.Context, id int64, reportEventID, sarifHash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE security_audits
		SET state='published', report_event_id=?, sarif_hash=?, updated_at=?
		WHERE id=? AND state='running'`,
		reportEventID, sarifHash, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("transition security audit %d running->published: %w", id, err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("transition security audit %d: %w", id, err)
	}
	if changed == 1 {
		return nil
	}

	var state string
	var storedReport, storedHash sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT state, report_event_id, sarif_hash FROM security_audits WHERE id=?`, id,
	).Scan(&state, &storedReport, &storedHash); err != nil {
		return fmt.Errorf("transition security audit %d running->published: invalid current state", id)
	}
	if state == "published" && storedReport.String == reportEventID && storedHash.String == sarifHash {
		return nil
	}
	return fmt.Errorf("transition security audit %d running->published: invalid current state", id)
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

// ReserveSecurityAuditPublicationSet atomically stores the SARIF artifact and
// all signed events before any network delivery. If a set already exists, it
// is returned unchanged so retries reuse identical event IDs and ciphertext.
func (s *Store) ReserveSecurityAuditPublicationSet(ctx context.Context, auditID int64, sarifHash string, sarif []byte, publications []SecurityAuditPublication) (SecurityAuditPublicationSet, error) {
	var empty SecurityAuditPublicationSet
	if auditID <= 0 {
		return empty, fmt.Errorf("reserve security audit publication: invalid audit id %d", auditID)
	}
	if strings.TrimSpace(sarifHash) == "" || len(sarif) == 0 {
		return empty, fmt.Errorf("reserve security audit publication %d: SARIF artifact is required", auditID)
	}
	if actual := securityAuditArtifactHash(sarif); actual != strings.ToLower(strings.TrimSpace(sarifHash)) {
		return empty, fmt.Errorf("reserve security audit publication %d: SARIF hash mismatch", auditID)
	}
	if err := validateSecurityAuditPublications(publications); err != nil {
		return empty, fmt.Errorf("reserve security audit publication %d: %w", auditID, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, fmt.Errorf("begin security audit publication transaction: %w", err)
	}
	defer tx.Rollback()

	var state string
	var storedHash sql.NullString
	var storedSARIF []byte
	if err := tx.QueryRowContext(ctx,
		`SELECT state, sarif_hash, sarif_artifact FROM security_audits WHERE id=?`, auditID,
	).Scan(&state, &storedHash, &storedSARIF); err != nil {
		return empty, fmt.Errorf("load security audit %d: %w", auditID, err)
	}
	existing, err := loadSecurityAuditPublications(ctx, tx, auditID)
	if err != nil {
		return empty, err
	}
	if len(existing) > 0 {
		if len(existing) != 3 || !completePublicationTypes(existing) {
			return empty, fmt.Errorf("security audit %d has incomplete durable publication set", auditID)
		}
		if !storedHash.Valid || storedHash.String == "" || len(storedSARIF) == 0 {
			return empty, fmt.Errorf("security audit %d durable publication set has no SARIF artifact", auditID)
		}
		if err := tx.Commit(); err != nil {
			return empty, fmt.Errorf("commit existing security audit publication: %w", err)
		}
		return SecurityAuditPublicationSet{
			AuditID: auditID, SARIFHash: storedHash.String,
			SARIF: append([]byte(nil), storedSARIF...), Publications: existing,
		}, nil
	}
	if state != "running" {
		return empty, fmt.Errorf("security audit %d publication reservation requires running state, got %s", auditID, state)
	}

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE security_audits
		SET sarif_hash=?, sarif_artifact=?, updated_at=? WHERE id=?`,
		sarifHash, sarif, now, auditID); err != nil {
		return empty, fmt.Errorf("store security audit %d SARIF: %w", auditID, err)
	}
	for _, publication := range publications {
		rawEvent, err := json.Marshal(publication.Event)
		if err != nil {
			return empty, fmt.Errorf("marshal security audit %s event: %w", publication.EventType, err)
		}
		relaysJSON, err := json.Marshal(publication.Relays)
		if err != nil {
			return empty, fmt.Errorf("marshal security audit %s relays: %w", publication.EventType, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO security_audit_publication_outbox(
			audit_id, event_type, event_id, raw_event_json, relays_json, delivered_at, created_at
		) VALUES (?, ?, ?, ?, ?, 0, ?)`,
			auditID, publication.EventType, publication.Event.ID.Hex(), string(rawEvent), string(relaysJSON), now); err != nil {
			return empty, fmt.Errorf("store security audit %s publication: %w", publication.EventType, err)
		}
	}
	persisted, err := loadSecurityAuditPublications(ctx, tx, auditID)
	if err != nil {
		return empty, err
	}
	if err := tx.Commit(); err != nil {
		return empty, fmt.Errorf("commit security audit publication: %w", err)
	}
	return SecurityAuditPublicationSet{
		AuditID: auditID, SARIFHash: sarifHash,
		SARIF: append([]byte(nil), sarif...), Publications: persisted,
	}, nil
}

func validateSecurityAuditPublications(publications []SecurityAuditPublication) error {
	if len(publications) != 3 || !completePublicationTypes(publications) {
		return fmt.Errorf("publication set must contain report, detail, and fallback")
	}
	for _, publication := range publications {
		if publication.Event.ID == nostr.ZeroID {
			return fmt.Errorf("%s event is unsigned", publication.EventType)
		}
		if len(publication.Relays) == 0 {
			return fmt.Errorf("%s event has no relays", publication.EventType)
		}
	}
	return nil
}

func completePublicationTypes(publications []SecurityAuditPublication) bool {
	found := make(map[string]bool, len(publications))
	for _, publication := range publications {
		switch publication.EventType {
		case SecurityAuditPublicationReport, SecurityAuditPublicationDetail, SecurityAuditPublicationFallback:
			found[publication.EventType] = true
		default:
			return false
		}
	}
	return len(found) == 3
}

type securityAuditPublicationQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadSecurityAuditPublications(ctx context.Context, q securityAuditPublicationQuerier, auditID int64) ([]SecurityAuditPublication, error) {
	rows, err := q.QueryContext(ctx, `SELECT event_type, raw_event_json, relays_json, delivered_at
		FROM security_audit_publication_outbox WHERE audit_id=?
		ORDER BY CASE event_type WHEN 'report' THEN 1 WHEN 'detail' THEN 2 ELSE 3 END`, auditID)
	if err != nil {
		return nil, fmt.Errorf("load security audit %d publication set: %w", auditID, err)
	}
	defer rows.Close()

	var publications []SecurityAuditPublication
	for rows.Next() {
		var eventType, rawEvent, relaysJSON string
		var deliveredAt int64
		if err := rows.Scan(&eventType, &rawEvent, &relaysJSON, &deliveredAt); err != nil {
			return nil, fmt.Errorf("scan security audit %d publication: %w", auditID, err)
		}
		var event nostr.Event
		if err := json.Unmarshal([]byte(rawEvent), &event); err != nil {
			return nil, fmt.Errorf("decode security audit %d %s event: %w", auditID, eventType, err)
		}
		var relays []string
		if err := json.Unmarshal([]byte(relaysJSON), &relays); err != nil {
			return nil, fmt.Errorf("decode security audit %d %s relays: %w", auditID, eventType, err)
		}
		publications = append(publications, SecurityAuditPublication{
			AuditID: auditID, EventType: eventType, Event: event,
			Relays: relays, Delivered: deliveredAt != 0,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read security audit %d publication set: %w", auditID, err)
	}
	return publications, nil
}

// MarkSecurityAuditPublicationDelivered records durable delivery after relay OK.
func (s *Store) MarkSecurityAuditPublicationDelivered(ctx context.Context, auditID int64, eventType string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE security_audit_publication_outbox
		SET delivered_at=CASE WHEN delivered_at=0 THEN ? ELSE delivered_at END
		WHERE audit_id=? AND event_type=?`, time.Now().Unix(), auditID, eventType)
	if err != nil {
		return fmt.Errorf("mark security audit %d %s delivered: %w", auditID, eventType, err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark security audit %d %s delivered: %w", auditID, eventType, err)
	}
	if changed != 1 {
		return fmt.Errorf("mark security audit %d %s delivered: publication not found", auditID, eventType)
	}
	return nil
}

// ListRecoverableSecurityAuditPublicationSets returns durable outboxes whose
// audit did not reach published state, including failed publication attempts.
func (s *Store) ListRecoverableSecurityAuditPublicationSets(ctx context.Context) ([]SecurityAuditPublicationSet, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, sarif_hash, sarif_artifact
		FROM security_audits
		WHERE state IN ('pending', 'running', 'failed')
		AND EXISTS (SELECT 1 FROM security_audit_publication_outbox o WHERE o.audit_id=security_audits.id)
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list recoverable security audit publications: %w", err)
	}
	var sets []SecurityAuditPublicationSet
	for rows.Next() {
		var set SecurityAuditPublicationSet
		var hash sql.NullString
		if err := rows.Scan(&set.AuditID, &hash, &set.SARIF); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan recoverable security audit publication: %w", err)
		}
		set.SARIFHash = hash.String
		set.SARIF = append([]byte(nil), set.SARIF...)
		sets = append(sets, set)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read recoverable security audit publications: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close recoverable security audit publications: %w", err)
	}
	for i := range sets {
		sets[i].Publications, err = loadSecurityAuditPublications(ctx, s.db, sets[i].AuditID)
		if err != nil {
			return nil, err
		}
	}
	return sets, nil
}

// CompleteSecurityAuditPublication marks a pending/running/failed audit
// published only after all three durable outbox events are delivered.
func (s *Store) CompleteSecurityAuditPublication(ctx context.Context, auditID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin security audit completion: %w", err)
	}
	defer tx.Rollback()

	var total, undelivered int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN delivered_at=0 THEN 1 ELSE 0 END), 0)
		FROM security_audit_publication_outbox WHERE audit_id=?`, auditID,
	).Scan(&total, &undelivered); err != nil {
		return fmt.Errorf("check security audit %d outbox completion: %w", auditID, err)
	}
	if total != 3 || undelivered != 0 {
		return fmt.Errorf("security audit %d publication incomplete: total=%d undelivered=%d", auditID, total, undelivered)
	}

	var reportEventID string
	if err := tx.QueryRowContext(ctx, `SELECT event_id FROM security_audit_publication_outbox
		WHERE audit_id=? AND event_type='report'`, auditID).Scan(&reportEventID); err != nil {
		return fmt.Errorf("load security audit %d report event: %w", auditID, err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE security_audits
		SET state='published', report_event_id=?, updated_at=?
		WHERE id=? AND state IN ('pending', 'running', 'failed')`,
		reportEventID, time.Now().Unix(), auditID)
	if err != nil {
		return fmt.Errorf("complete security audit %d publication: %w", auditID, err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete security audit %d publication: %w", auditID, err)
	}
	if changed != 1 {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM security_audits WHERE id=?`, auditID).Scan(&state); err != nil || state != "published" {
			return fmt.Errorf("complete security audit %d publication: invalid current state", auditID)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit security audit %d publication completion: %w", auditID, err)
	}
	return nil
}

// SecurityAuditSARIF returns the durable SARIF bytes and their digest.
func (s *Store) SecurityAuditSARIF(ctx context.Context, auditID int64) ([]byte, string, error) {
	return s.securityAuditSARIF(ctx, auditID, "")
}

// SecurityAuditSARIFForRequester returns SARIF only when the requester matches
// the pubkey that requested the audit.
func (s *Store) SecurityAuditSARIFForRequester(ctx context.Context, auditID int64, requester string) ([]byte, string, error) {
	requester = strings.TrimSpace(requester)
	if requester == "" {
		return nil, "", fmt.Errorf("load security audit %d SARIF: requester is required", auditID)
	}
	return s.securityAuditSARIF(ctx, auditID, requester)
}

func (s *Store) securityAuditSARIF(ctx context.Context, auditID int64, requester string) ([]byte, string, error) {
	query := `SELECT sarif_artifact, sarif_hash FROM security_audits WHERE id=?`
	args := []any{auditID}
	if requester != "" {
		query += " AND requested_by=?"
		args = append(args, requester)
	}
	var artifact []byte
	var hash sql.NullString
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&artifact, &hash); err != nil {
		return nil, "", fmt.Errorf("load security audit %d SARIF: %w", auditID, err)
	}
	if len(artifact) == 0 || hash.String == "" {
		return nil, "", fmt.Errorf("load security audit %d SARIF: artifact not available", auditID)
	}
	if actual := securityAuditArtifactHash(artifact); actual != strings.ToLower(strings.TrimSpace(hash.String)) {
		return nil, "", fmt.Errorf("load security audit %d SARIF: artifact hash mismatch", auditID)
	}
	return append([]byte(nil), artifact...), hash.String, nil
}

func securityAuditArtifactHash(artifact []byte) string {
	sum := sha256.Sum256(artifact)
	return hex.EncodeToString(sum[:])
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
