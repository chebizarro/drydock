package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"fiatjaf.com/nostr"
)

// ErrDependencyUpgradeNotFound is returned when a dependency-upgrade row is
// addressed by an id that does not exist.
var ErrDependencyUpgradeNotFound = errors.New("dependency upgrade row not found")

// DependencyUpgradeStatus is the lifecycle state of a proposed upgrade.
//
// The set is closed and owned entirely by this package, which is why the schema
// enforces it with a CHECK. A row starts 'open' at proposal, and moves to a
// terminal or transient state as the world changes:
//   - superseded: a newer/different upgrade for the same package replaced this
//     proposal, or the installed version moved out from under it.
//   - merged: the published patch's NIP-34 root status became applied (1631).
//   - rejected: the published patch's root status became closed (1632).
//   - failed: an operational error (edit/publish) prevented completion.
//   - no_fix_available: the package is vulnerable but no fixed version is known;
//     recorded (with an empty to_version) so a re-scan does not re-resolve it.
type DependencyUpgradeStatus string

const (
	DependencyUpgradeOpen           DependencyUpgradeStatus = "open"
	DependencyUpgradeSuperseded     DependencyUpgradeStatus = "superseded"
	DependencyUpgradeMerged         DependencyUpgradeStatus = "merged"
	DependencyUpgradeRejected       DependencyUpgradeStatus = "rejected"
	DependencyUpgradeFailed         DependencyUpgradeStatus = "failed"
	DependencyUpgradeNoFixAvailable DependencyUpgradeStatus = "no_fix_available"
)

func (s DependencyUpgradeStatus) valid() bool {
	switch s {
	case DependencyUpgradeOpen, DependencyUpgradeSuperseded, DependencyUpgradeMerged,
		DependencyUpgradeRejected, DependencyUpgradeFailed, DependencyUpgradeNoFixAvailable:
		return true
	default:
		return false
	}
}

// DependencyUpgrade is one durable upgrade record. Its identity — the columns
// in the UNIQUE constraint — is (RepoID, Ecosystem, PackageName, FromVersion,
// ToVersion); every other field is mutable state or metadata attached to that
// identity.
type DependencyUpgrade struct {
	ID           int64
	RepoID       string
	Ecosystem    string
	PackageName  string
	PURL         string
	FromVersion  string
	ToVersion    string
	ManifestPath string
	LockfilePath string
	Advisories   []string
	Policy       string
	Status       DependencyUpgradeStatus
	PatchEventID string
	LastError    string
	CreatedAt    int64
	UpdatedAt    int64
}

// DependencyUpgradeDisposition reports whether InsertDependencyUpgrade minted a
// new record or found an existing one for the same identity.
type DependencyUpgradeDisposition string

const (
	DependencyUpgradeCreated  DependencyUpgradeDisposition = "created"
	DependencyUpgradeExisting DependencyUpgradeDisposition = "existing"
)

// DependencyUpgradeResult is the outcome of an idempotent insert.
type DependencyUpgradeResult struct {
	Disposition DependencyUpgradeDisposition
	Upgrade     DependencyUpgrade
}

// dependencyUpgradeColumns is the canonical SELECT column order consumed by
// scanDependencyUpgrade.
const dependencyUpgradeColumns = `id, repo_id, ecosystem, package_name, purl,
	from_version, to_version, manifest_path, lockfile_path, advisories_csv,
	policy, status, patch_event_id, last_error, created_at, updated_at`

func scanDependencyUpgrade(scanner rowScanner) (DependencyUpgrade, error) {
	var up DependencyUpgrade
	var advisoriesCSV, status string
	err := scanner.Scan(
		&up.ID, &up.RepoID, &up.Ecosystem, &up.PackageName, &up.PURL,
		&up.FromVersion, &up.ToVersion, &up.ManifestPath, &up.LockfilePath, &advisoriesCSV,
		&up.Policy, &status, &up.PatchEventID, &up.LastError, &up.CreatedAt, &up.UpdatedAt,
	)
	up.Advisories = splitCSV(advisoriesCSV)
	up.Status = DependencyUpgradeStatus(status)
	return up, err
}

func normalizeDependencyUpgrade(up DependencyUpgrade) DependencyUpgrade {
	up.RepoID = strings.TrimSpace(up.RepoID)
	up.Ecosystem = strings.TrimSpace(up.Ecosystem)
	up.PackageName = strings.TrimSpace(up.PackageName)
	up.PURL = strings.TrimSpace(up.PURL)
	up.FromVersion = strings.TrimSpace(up.FromVersion)
	up.ToVersion = strings.TrimSpace(up.ToVersion)
	up.ManifestPath = strings.TrimSpace(up.ManifestPath)
	up.LockfilePath = strings.TrimSpace(up.LockfilePath)
	up.Policy = strings.TrimSpace(up.Policy)
	up.PatchEventID = strings.TrimSpace(up.PatchEventID)
	trimmed := make([]string, 0, len(up.Advisories))
	for _, a := range up.Advisories {
		if a = strings.TrimSpace(a); a != "" {
			trimmed = append(trimmed, a)
		}
	}
	up.Advisories = trimmed
	if up.Status == "" {
		up.Status = DependencyUpgradeOpen
	}
	return up
}

// InsertDependencyUpgrade durably records a proposed upgrade. It is idempotent
// on the identity key: if a row already exists for (repo, ecosystem, package,
// from, to) it is returned unchanged with an Existing disposition — the caller
// must not resurrect a superseded/rejected proposal by re-proposing the exact
// same upgrade. A genuinely different upgrade (any identity column differs) is a
// new record with its own lifecycle.
func (s *Store) InsertDependencyUpgrade(ctx context.Context, up DependencyUpgrade) (DependencyUpgradeResult, error) {
	up = normalizeDependencyUpgrade(up)
	if up.RepoID == "" || up.Ecosystem == "" || up.PackageName == "" || up.FromVersion == "" {
		return DependencyUpgradeResult{}, errors.New("dependency upgrade is missing repo, ecosystem, package, or from-version")
	}
	if !up.Status.valid() {
		return DependencyUpgradeResult{}, fmt.Errorf("invalid dependency upgrade status %q", up.Status)
	}
	if up.CreatedAt <= 0 {
		up.CreatedAt = time.Now().Unix()
	}
	up.UpdatedAt = up.CreatedAt

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DependencyUpgradeResult{}, fmt.Errorf("begin dependency upgrade transaction: %w", err)
	}
	defer tx.Rollback()

	if existing, ok, err := getDependencyUpgradeByKeyTx(ctx, tx, up); err != nil {
		return DependencyUpgradeResult{}, err
	} else if ok {
		return DependencyUpgradeResult{Disposition: DependencyUpgradeExisting, Upgrade: existing}, nil
	}

	res, err := tx.ExecContext(ctx, `INSERT INTO dependency_upgrades(
		repo_id, ecosystem, package_name, purl, from_version, to_version,
		manifest_path, lockfile_path, advisories_csv, policy, status,
		patch_event_id, last_error, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		up.RepoID, up.Ecosystem, up.PackageName, up.PURL, up.FromVersion, up.ToVersion,
		up.ManifestPath, up.LockfilePath, strings.Join(up.Advisories, ","), up.Policy, string(up.Status),
		up.PatchEventID, up.LastError, up.CreatedAt, up.UpdatedAt,
	)
	if err != nil {
		// A concurrent writer may have inserted the identity first; return it.
		if existing, ok, getErr := getDependencyUpgradeByKeyTx(ctx, tx, up); getErr == nil && ok {
			return DependencyUpgradeResult{Disposition: DependencyUpgradeExisting, Upgrade: existing}, nil
		}
		return DependencyUpgradeResult{}, fmt.Errorf("insert dependency upgrade: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return DependencyUpgradeResult{}, fmt.Errorf("read dependency upgrade id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DependencyUpgradeResult{}, fmt.Errorf("commit dependency upgrade transaction: %w", err)
	}
	up.ID = id
	return DependencyUpgradeResult{Disposition: DependencyUpgradeCreated, Upgrade: up}, nil
}

// GetDependencyUpgrade returns a single upgrade row by id.
func (s *Store) GetDependencyUpgrade(ctx context.Context, id int64) (DependencyUpgrade, bool, error) {
	up, err := scanDependencyUpgrade(s.db.QueryRowContext(ctx,
		`SELECT `+dependencyUpgradeColumns+` FROM dependency_upgrades WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return DependencyUpgrade{}, false, nil
	}
	if err != nil {
		return DependencyUpgrade{}, false, fmt.Errorf("get dependency upgrade: %w", err)
	}
	return up, true, nil
}

// GetDependencyUpgradeByKey returns the upgrade for a full identity key.
func (s *Store) GetDependencyUpgradeByKey(ctx context.Context, repoID, ecosystem, packageName, fromVersion, toVersion string) (DependencyUpgrade, bool, error) {
	up, err := scanDependencyUpgrade(s.db.QueryRowContext(ctx,
		`SELECT `+dependencyUpgradeColumns+` FROM dependency_upgrades
		 WHERE repo_id=? AND ecosystem=? AND package_name=? AND from_version=? AND to_version=?`,
		strings.TrimSpace(repoID), strings.TrimSpace(ecosystem), strings.TrimSpace(packageName),
		strings.TrimSpace(fromVersion), strings.TrimSpace(toVersion)))
	if errors.Is(err, sql.ErrNoRows) {
		return DependencyUpgrade{}, false, nil
	}
	if err != nil {
		return DependencyUpgrade{}, false, fmt.Errorf("get dependency upgrade by key: %w", err)
	}
	return up, true, nil
}

// GetOpenDependencyUpgrades returns every currently-open upgrade for a repo,
// oldest first.
func (s *Store) GetOpenDependencyUpgrades(ctx context.Context, repoID string) ([]DependencyUpgrade, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+dependencyUpgradeColumns+` FROM dependency_upgrades
		 WHERE repo_id=? AND status=? ORDER BY created_at, id`,
		strings.TrimSpace(repoID), string(DependencyUpgradeOpen))
	if err != nil {
		return nil, fmt.Errorf("query open dependency upgrades: %w", err)
	}
	defer rows.Close()

	var upgrades []DependencyUpgrade
	for rows.Next() {
		up, err := scanDependencyUpgrade(rows)
		if err != nil {
			return nil, fmt.Errorf("scan open dependency upgrade: %w", err)
		}
		upgrades = append(upgrades, up)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate open dependency upgrades: %w", err)
	}
	return upgrades, nil
}

// MarkDependencyUpgradeStatus transitions a row to a new lifecycle state,
// recording an operational error string (empty to clear it).
func (s *Store) MarkDependencyUpgradeStatus(ctx context.Context, id int64, status DependencyUpgradeStatus, lastError string) error {
	if !status.valid() {
		return fmt.Errorf("invalid dependency upgrade status %q", status)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE dependency_upgrades SET status=?, last_error=?, updated_at=? WHERE id=?`,
		string(status), strings.TrimSpace(lastError), time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("mark dependency upgrade status: %w", err)
	}
	return requireDependencyUpgradeAffected(res)
}

// RecordDependencyUpgradePatchEvent stores the published patch's root event id
// on an upgrade. Publication does not itself change the lifecycle state — the
// row stays 'open' until reconciliation observes a merged/closed root status.
func (s *Store) RecordDependencyUpgradePatchEvent(ctx context.Context, id int64, patchEventID string) error {
	patchEventID = strings.TrimSpace(patchEventID)
	if patchEventID == "" {
		return errors.New("dependency upgrade patch event id is required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE dependency_upgrades SET patch_event_id=?, updated_at=? WHERE id=?`,
		patchEventID, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("record dependency upgrade patch event: %w", err)
	}
	return requireDependencyUpgradeAffected(res)
}

// SupersedeOpenDependencyUpgrades enforces the "at most one open upgrade per
// (repo, ecosystem, package)" invariant. It marks every OPEN row for the given
// package superseded except the identity being kept (keepFrom, keepTo), and
// returns how many rows it superseded. Callers invoke it when a newer fix
// version arrives for a package that already has an open proposal, or when the
// installed version has moved on. Pass empty keep versions to supersede all
// open rows for the package (e.g. the package is no longer vulnerable).
func (s *Store) SupersedeOpenDependencyUpgrades(ctx context.Context, repoID, ecosystem, packageName, keepFrom, keepTo string) (int, error) {
	repoID = strings.TrimSpace(repoID)
	ecosystem = strings.TrimSpace(ecosystem)
	packageName = strings.TrimSpace(packageName)
	if repoID == "" || ecosystem == "" || packageName == "" {
		return 0, errors.New("supersede requires repo, ecosystem, and package")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE dependency_upgrades SET status=?, updated_at=?
		 WHERE repo_id=? AND ecosystem=? AND package_name=? AND status=?
		   AND NOT (from_version=? AND to_version=?)`,
		string(DependencyUpgradeSuperseded), time.Now().Unix(),
		repoID, ecosystem, packageName, string(DependencyUpgradeOpen),
		strings.TrimSpace(keepFrom), strings.TrimSpace(keepTo))
	if err != nil {
		return 0, fmt.Errorf("supersede open dependency upgrades: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count superseded dependency upgrades: %w", err)
	}
	return int(affected), nil
}

func requireDependencyUpgradeAffected(res sql.Result) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("dependency upgrade rows affected: %w", err)
	}
	if affected == 0 {
		return ErrDependencyUpgradeNotFound
	}
	return nil
}

func getDependencyUpgradeByKeyTx(ctx context.Context, tx *sql.Tx, up DependencyUpgrade) (DependencyUpgrade, bool, error) {
	existing, err := scanDependencyUpgrade(tx.QueryRowContext(ctx,
		`SELECT `+dependencyUpgradeColumns+` FROM dependency_upgrades
		 WHERE repo_id=? AND ecosystem=? AND package_name=? AND from_version=? AND to_version=?`,
		up.RepoID, up.Ecosystem, up.PackageName, up.FromVersion, up.ToVersion))
	if errors.Is(err, sql.ErrNoRows) {
		return DependencyUpgrade{}, false, nil
	}
	if err != nil {
		return DependencyUpgrade{}, false, fmt.Errorf("get existing dependency upgrade: %w", err)
	}
	return existing, true, nil
}

// dependencyUpgradeOutbox is the reserve-then-mark publication outbox for
// dependency-upgrade patches. It reuses the shared reviewOutbox helper over a
// single upgrade_id key, so a retried publish reuses the exact reserved Nostr
// event — the same idempotency property the review outboxes rely on.
var dependencyUpgradeOutbox = reviewOutbox{
	table:   "dependency_upgrade_outbox",
	keyCols: []string{"upgrade_id"},
	noun:    "dependency upgrade publication",
}

// GetDependencyUpgradePublication returns the exact signed patch event reserved
// for relay delivery for an upgrade, if any.
func (s *Store) GetDependencyUpgradePublication(ctx context.Context, upgradeID int64) (event nostr.Event, delivered, found bool, err error) {
	return dependencyUpgradeOutbox.get(ctx, s.db, upgradeID)
}

// ReserveDependencyUpgradePublication durably stores a signed patch event before
// relay delivery. If another attempt already reserved this upgrade's event, that
// exact event is returned instead of replacing it, keeping the Nostr event ID
// stable across retries.
func (s *Store) ReserveDependencyUpgradePublication(ctx context.Context, upgradeID int64, event nostr.Event) (nostr.Event, bool, error) {
	return dependencyUpgradeOutbox.reserve(ctx, s.db, event, upgradeID)
}

// MarkDependencyUpgradePublicationDelivered records that the reserved event was
// delivered to relays.
func (s *Store) MarkDependencyUpgradePublicationDelivered(ctx context.Context, upgradeID int64) error {
	return dependencyUpgradeOutbox.markDelivered(ctx, s.db, upgradeID)
}

// PendingDependencyUpgradeScan is one coalesced reactive scan request. The
// generation changes when a new head movement arrives while a scan is running,
// allowing completion of the older scan to preserve the newer request.
type PendingDependencyUpgradeScan struct {
	RepoID      string
	Status      string
	Generation  int64
	AvailableAt int64
	LeaseUntil  int64
	LastError   string
	CreatedAt   int64
	UpdatedAt   int64
}

const pendingDependencyUpgradeScanColumns = `repo_id, status, generation, available_at,
	lease_until, last_error, created_at, updated_at`

func scanPendingDependencyUpgradeScan(scanner rowScanner) (PendingDependencyUpgradeScan, error) {
	var scan PendingDependencyUpgradeScan
	err := scanner.Scan(&scan.RepoID, &scan.Status, &scan.Generation, &scan.AvailableAt,
		&scan.LeaseUntil, &scan.LastError, &scan.CreatedAt, &scan.UpdatedAt)
	return scan, err
}

// UpsertPendingDependencyUpgradeScan durably records a reactive scan without
// creating more than one row per repository. A trigger received while a scan is
// processing advances its generation; pending triggers otherwise coalesce.
// The returned bool reports whether the caller should offer a wake-up hint to
// the in-memory queue.
func (s *Store) UpsertPendingDependencyUpgradeScan(ctx context.Context, repoID string, now int64) (bool, error) {
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return false, errors.New("pending dependency-upgrade scan repo id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin pending dependency-upgrade scan upsert: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pending_dependency_upgrade_scans
		(repo_id, status, generation, available_at, lease_until, last_error, created_at, updated_at)
		VALUES (?, 'pending', 1, ?, 0, '', ?, ?)`, repoID, now, now, now)
	if err != nil {
		return false, fmt.Errorf("insert pending dependency-upgrade scan: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read pending dependency-upgrade scan insert: %w", err)
	}
	if inserted == 0 {
		// Do not start a concurrent scan. Advancing the generation is enough for
		// the running worker to preserve and re-enqueue the newer request.
		if _, err := tx.ExecContext(ctx, `UPDATE pending_dependency_upgrade_scans
			SET generation=generation+1, updated_at=?
			WHERE repo_id=? AND status='processing'`, now, repoID); err != nil {
			return false, fmt.Errorf("coalesce pending dependency-upgrade scan: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit pending dependency-upgrade scan upsert: %w", err)
	}
	return inserted == 1, nil
}

// ReservePendingDependencyUpgradeScan atomically claims a due row before work.
// Duplicate queue hints are harmless because only pending rows can be claimed.
func (s *Store) ReservePendingDependencyUpgradeScan(ctx context.Context, repoID string, now, leaseUntil int64) (PendingDependencyUpgradeScan, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PendingDependencyUpgradeScan{}, false, fmt.Errorf("begin pending dependency-upgrade scan reserve: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE pending_dependency_upgrade_scans
		SET status='processing', lease_until=?, last_error='', updated_at=?
		WHERE repo_id=? AND status='pending' AND available_at<=?`, leaseUntil, now, repoID, now)
	if err != nil {
		return PendingDependencyUpgradeScan{}, false, fmt.Errorf("reserve pending dependency-upgrade scan: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return PendingDependencyUpgradeScan{}, false, fmt.Errorf("read pending dependency-upgrade scan reserve: %w", err)
	}
	if affected == 0 {
		if err := tx.Commit(); err != nil {
			return PendingDependencyUpgradeScan{}, false, fmt.Errorf("commit empty dependency-upgrade scan reserve: %w", err)
		}
		return PendingDependencyUpgradeScan{}, false, nil
	}
	scan, err := scanPendingDependencyUpgradeScan(tx.QueryRowContext(ctx,
		`SELECT `+pendingDependencyUpgradeScanColumns+` FROM pending_dependency_upgrade_scans WHERE repo_id=?`, repoID))
	if err != nil {
		return PendingDependencyUpgradeScan{}, false, fmt.Errorf("read reserved dependency-upgrade scan: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PendingDependencyUpgradeScan{}, false, fmt.Errorf("commit pending dependency-upgrade scan reserve: %w", err)
	}
	return scan, true, nil
}

// CompletePendingDependencyUpgradeScan removes a completed row unless a newer
// head movement advanced its generation during the scan. In that case the row
// returns to pending and true tells the worker to enqueue another wake-up hint.
func (s *Store) CompletePendingDependencyUpgradeScan(ctx context.Context, repoID string, generation, now int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin pending dependency-upgrade scan completion: %w", err)
	}
	defer tx.Rollback()
	var status string
	var currentGeneration int64
	err = tx.QueryRowContext(ctx, `SELECT status, generation FROM pending_dependency_upgrade_scans WHERE repo_id=?`, repoID).
		Scan(&status, &currentGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read pending dependency-upgrade scan completion: %w", err)
	}
	if status != "processing" || currentGeneration < generation {
		return false, nil
	}
	if currentGeneration == generation {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_dependency_upgrade_scans
			WHERE repo_id=? AND status='processing' AND generation=?`, repoID, generation); err != nil {
			return false, fmt.Errorf("delete completed dependency-upgrade scan: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit dependency-upgrade scan completion: %w", err)
		}
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pending_dependency_upgrade_scans
		SET status='pending', available_at=?, lease_until=0, last_error='', updated_at=?
		WHERE repo_id=? AND status='processing'`, now, now, repoID); err != nil {
		return false, fmt.Errorf("preserve newer dependency-upgrade scan: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit newer dependency-upgrade scan: %w", err)
	}
	return true, nil
}

// DeferPendingDependencyUpgradeScan releases a failed reservation after a
// cooldown. Updating generations newer than the reservation is intentional: a
// persistently failing repository must not spin merely because its head moved.
func (s *Store) DeferPendingDependencyUpgradeScan(ctx context.Context, repoID string, generation, now, availableAt int64, lastError string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pending_dependency_upgrade_scans
		SET status='pending', available_at=?, lease_until=0, last_error=?, updated_at=?
		WHERE repo_id=? AND status='processing' AND generation>=?`,
		availableAt, lastError, now, repoID, generation)
	if err != nil {
		return fmt.Errorf("defer pending dependency-upgrade scan: %w", err)
	}
	return nil
}

// ResetStuckPendingDependencyUpgradeScans releases expired reservations after a
// process crash. At-least-once replay is safe because scan persistence is
// idempotent; a duplicate scan is wasteful but does not duplicate upgrades.
func (s *Store) ResetStuckPendingDependencyUpgradeScans(ctx context.Context, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE pending_dependency_upgrade_scans
		SET status='pending', available_at=?, lease_until=0, updated_at=?
		WHERE status='processing' AND lease_until>0 AND lease_until<=?`, now, now, now)
	if err != nil {
		return 0, fmt.Errorf("reset stuck pending dependency-upgrade scans: %w", err)
	}
	return res.RowsAffected()
}

// ListDuePendingDependencyUpgradeScans returns durable work that is ready for a
// queue wake-up. Reservation remains the atomic claim, so repeated listings are
// safe and do not duplicate processing.
func (s *Store) ListDuePendingDependencyUpgradeScans(ctx context.Context, now int64, limit int) ([]PendingDependencyUpgradeScan, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+pendingDependencyUpgradeScanColumns+`
		FROM pending_dependency_upgrade_scans
		WHERE status='pending' AND available_at<=?
		ORDER BY available_at ASC, repo_id ASC
		LIMIT ?`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("query due pending dependency-upgrade scans: %w", err)
	}
	defer rows.Close()
	var scans []PendingDependencyUpgradeScan
	for rows.Next() {
		scan, err := scanPendingDependencyUpgradeScan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan due pending dependency-upgrade scan: %w", err)
		}
		scans = append(scans, scan)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due pending dependency-upgrade scans: %w", err)
	}
	return scans, nil
}
