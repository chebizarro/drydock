package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip34"
	_ "modernc.org/sqlite"
)

var (
	ErrReviewAlreadyPublished = errors.New("review already published for patch/repo")
	ErrReviewNotFound         = errors.New("review log row not found")
)

type Store struct {
	db *sql.DB
}

type PatchEventRecord struct {
	EventID  string
	RepoID   string
	RootID   string
	Kind     int
	RawEvent string
}

type MetaReviewReuse struct {
	ResponseJSON string
}

// MetaReviewSample is a meta-review record returned by SampleRecentMetaReviews.
type MetaReviewSample struct {
	ID           int64
	PatchEventID string
	RepoID       string
	GateReason   string
	ResponseJSON string
	CreatedAt    int64
}

// DriftFlag records a human-flagged meta-review that exhibits convention drift.
type DriftFlag struct {
	ID           int64
	MetaReviewID int64
	Notes        string
	FlaggedAt    int64
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	dsn += separator + "_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite supports only one concurrent writer. Limit open connections
	// to avoid "database is locked" errors under load.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying *sql.DB for direct queries by packages
// that need ad-hoc access (e.g. conversation thread lookups).
func (s *Store) DB() *sql.DB {
	return s.db
}

// Ping verifies database connectivity.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

type schemaMigration struct {
	version int
	name    string
	apply   func(context.Context, *sql.Tx) error
}

var schemaMigrations = []schemaMigration{
	{
		version: 1,
		name:    "review_log_status_event_columns",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			for _, col := range []struct {
				table, name, ddl string
			}{
				{"review_log", "status_event_id", "ALTER TABLE review_log ADD COLUMN status_event_id TEXT"},
				{"review_log", "status_event_kind", "ALTER TABLE review_log ADD COLUMN status_event_kind INTEGER NOT NULL DEFAULT 0"},
				{"review_log", "status_published_at", "ALTER TABLE review_log ADD COLUMN status_published_at INTEGER NOT NULL DEFAULT 0"},
			} {
				exists, err := hasColumn(ctx, tx, col.table, col.name)
				if err != nil {
					return fmt.Errorf("check %s.%s: %w", col.table, col.name, err)
				}
				if exists {
					continue
				}
				if _, err := tx.ExecContext(ctx, col.ddl); err != nil {
					return fmt.Errorf("add %s.%s: %w", col.table, col.name, err)
				}
			}
			return nil
		},
	},
	{
		version: 2,
		name:    "review_feedback_assignment_rater_unique",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `DELETE FROM review_feedback
				WHERE id NOT IN (
					SELECT MIN(id) FROM review_feedback GROUP BY assignment_id, rater_pubkey
				)`); err != nil {
				return fmt.Errorf("deduplicate review feedback: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_review_feedback_assignment_rater
				ON review_feedback(assignment_id, rater_pubkey)`); err != nil {
				return fmt.Errorf("add review feedback assignment/rater uniqueness: %w", err)
			}
			return nil
		},
	},
	{
		version: 3,
		name:    "payment_melt_recovery_evidence",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			for _, col := range []struct{ name, ddl string }{
				{"expected_amount_sats", "ALTER TABLE review_payments ADD COLUMN expected_amount_sats INTEGER NOT NULL DEFAULT 0"},
				{"subscription_days", "ALTER TABLE review_payments ADD COLUMN subscription_days INTEGER NOT NULL DEFAULT 0"},
				{"invoice_amount_msats", "ALTER TABLE review_payments ADD COLUMN invoice_amount_msats INTEGER NOT NULL DEFAULT 0"},
				{"melt_quote_id", "ALTER TABLE review_payments ADD COLUMN melt_quote_id TEXT NOT NULL DEFAULT ''"},
				{"melt_quote_amount_sats", "ALTER TABLE review_payments ADD COLUMN melt_quote_amount_sats INTEGER NOT NULL DEFAULT 0"},
				{"melt_fee_reserve_sats", "ALTER TABLE review_payments ADD COLUMN melt_fee_reserve_sats INTEGER NOT NULL DEFAULT 0"},
				{"melt_state", "ALTER TABLE review_payments ADD COLUMN melt_state TEXT NOT NULL DEFAULT ''"},
			} {
				exists, err := hasColumn(ctx, tx, "review_payments", col.name)
				if err != nil {
					return fmt.Errorf("check review_payments.%s: %w", col.name, err)
				}
				if !exists {
					if _, err := tx.ExecContext(ctx, col.ddl); err != nil {
						return fmt.Errorf("add review_payments.%s: %w", col.name, err)
					}
				}
			}
			return nil
		},
	},
	{
		version: 4,
		name:    "marketplace_completion_payouts",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			for _, col := range []struct{ table, name, ddl string }{
				{"reviewer_profiles", "payout_destination", "ALTER TABLE reviewer_profiles ADD COLUMN payout_destination TEXT NOT NULL DEFAULT ''"},
				{"review_assignments", "review_event_id", "ALTER TABLE review_assignments ADD COLUMN review_event_id TEXT"},
			} {
				exists, err := hasColumn(ctx, tx, col.table, col.name)
				if err != nil {
					return fmt.Errorf("check %s.%s: %w", col.table, col.name, err)
				}
				if !exists {
					if _, err := tx.ExecContext(ctx, col.ddl); err != nil {
						return fmt.Errorf("add %s.%s: %w", col.table, col.name, err)
					}
				}
			}
			for _, ddl := range []string{
				`CREATE UNIQUE INDEX IF NOT EXISTS idx_review_assignments_review_event
				ON review_assignments(review_event_id) WHERE review_event_id IS NOT NULL`,
				`CREATE TABLE IF NOT EXISTS marketplace_payouts (
				assignment_id INTEGER PRIMARY KEY,
				idempotency_key TEXT NOT NULL UNIQUE,
				amount_sats INTEGER NOT NULL CHECK (amount_sats > 0),
				destination TEXT NOT NULL,
				status TEXT NOT NULL CHECK (status IN ('pending', 'submitted', 'settled', 'failed')),
				payment_hash TEXT NOT NULL DEFAULT '', preimage TEXT NOT NULL DEFAULT '',
				failure_reason TEXT NOT NULL DEFAULT '', submitted_at INTEGER NOT NULL DEFAULT 0,
				settled_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
				FOREIGN KEY (assignment_id) REFERENCES review_assignments(id) ON DELETE RESTRICT)`,
				`CREATE INDEX IF NOT EXISTS idx_marketplace_payouts_status ON marketplace_payouts(status)`,
				`CREATE UNIQUE INDEX IF NOT EXISTS idx_marketplace_payouts_destination ON marketplace_payouts(destination)`,
				`CREATE TABLE IF NOT EXISTS marketplace_payout_audit (
				id INTEGER PRIMARY KEY AUTOINCREMENT, assignment_id INTEGER NOT NULL,
				from_status TEXT NOT NULL DEFAULT '', to_status TEXT NOT NULL,
				completion_event_id TEXT NOT NULL DEFAULT '', payment_hash TEXT NOT NULL DEFAULT '',
				detail TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
				FOREIGN KEY (assignment_id) REFERENCES review_assignments(id) ON DELETE RESTRICT)`,
				`CREATE INDEX IF NOT EXISTS idx_marketplace_payout_audit_assignment
				ON marketplace_payout_audit(assignment_id, id)`,
				`CREATE TRIGGER IF NOT EXISTS marketplace_payout_audit_no_update
				BEFORE UPDATE ON marketplace_payout_audit BEGIN
				SELECT RAISE(ABORT, 'marketplace payout audit is immutable'); END`,
				`CREATE TRIGGER IF NOT EXISTS marketplace_payout_audit_no_delete
				BEFORE DELETE ON marketplace_payout_audit BEGIN
				SELECT RAISE(ABORT, 'marketplace payout audit is immutable'); END`,
			} {
				if _, err := tx.ExecContext(ctx, ddl); err != nil {
					return fmt.Errorf("apply marketplace payout ddl: %w", err)
				}
			}
			return nil
		},
	},
	{
		version: 5,
		name:    "review_payments_token_spent_constraint",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			var tableSQL string
			if err := tx.QueryRowContext(ctx,
				`SELECT sql FROM sqlite_master WHERE type='table' AND name='review_payments'`,
			).Scan(&tableSQL); err != nil {
				return fmt.Errorf("read review_payments schema: %w", err)
			}
			if strings.Contains(tableSQL, "'token_spent'") {
				return nil
			}

			const rebuildSQL = `
ALTER TABLE review_payments RENAME TO review_payments_before_token_spent;
CREATE TABLE review_payments (
	patch_event_id TEXT PRIMARY KEY,
	repo_id TEXT NOT NULL,
	author_pubkey TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('pending', 'token_spent', 'authorized')),
	access_kind TEXT NOT NULL DEFAULT ''
	CHECK (access_kind IN ('', 'free_tier', 'subscription', 'cashu_review', 'cashu_subscription')),
	requested_mode TEXT NOT NULL DEFAULT 'review'
	CHECK (requested_mode IN ('review', 'subscription')),
	token_hash TEXT,
	mint_url TEXT NOT NULL DEFAULT '',
	token_amount_sats INTEGER NOT NULL DEFAULT 0,
	expected_amount_sats INTEGER NOT NULL DEFAULT 0,
	subscription_days INTEGER NOT NULL DEFAULT 0,
	invoice_id TEXT NOT NULL DEFAULT '',
	invoice_request TEXT NOT NULL DEFAULT '',
	invoice_amount_msats INTEGER NOT NULL DEFAULT 0,
	invoice_expires_at INTEGER NOT NULL DEFAULT 0,
	melt_quote_id TEXT NOT NULL DEFAULT '',
	melt_quote_amount_sats INTEGER NOT NULL DEFAULT 0,
	melt_fee_reserve_sats INTEGER NOT NULL DEFAULT 0,
	melt_state TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
INSERT INTO review_payments (
	patch_event_id, repo_id, author_pubkey, status, access_kind, requested_mode,
	token_hash, mint_url, token_amount_sats, expected_amount_sats, subscription_days,
	invoice_id, invoice_request, invoice_amount_msats, invoice_expires_at,
	melt_quote_id, melt_quote_amount_sats, melt_fee_reserve_sats, melt_state,
	created_at, updated_at
)
SELECT
	patch_event_id, repo_id, author_pubkey, status, access_kind, requested_mode,
	token_hash, mint_url, token_amount_sats, expected_amount_sats, subscription_days,
	invoice_id, invoice_request, invoice_amount_msats, invoice_expires_at,
	melt_quote_id, melt_quote_amount_sats, melt_fee_reserve_sats, melt_state,
	created_at, updated_at
FROM review_payments_before_token_spent;
DROP TABLE review_payments_before_token_spent;
CREATE UNIQUE INDEX idx_review_payments_token_hash
	ON review_payments(token_hash) WHERE token_hash IS NOT NULL;
CREATE INDEX idx_review_payments_author_repo
	ON review_payments(author_pubkey, repo_id);`
			if _, err := tx.ExecContext(ctx, rebuildSQL); err != nil {
				return fmt.Errorf("rebuild review_payments: %w", err)
			}
			return nil
		},
	},
}

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schemaSQL)
	if err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if err := s.applySchemaMigrations(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) applySchemaMigrations(ctx context.Context) error {
	for _, migration := range schemaMigrations {
		if err := s.applySchemaMigration(ctx, migration); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applySchemaMigration(ctx context.Context, migration schemaMigration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration %d: %w", migration.version, err)
	}
	defer tx.Rollback()

	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, migration.version).Scan(&applied); err != nil {
		return fmt.Errorf("check schema migration %d: %w", migration.version, err)
	}
	if applied > 0 {
		return tx.Commit()
	}

	if err := migration.apply(ctx, tx); err != nil {
		return fmt.Errorf("apply schema migration %d %s: %w", migration.version, migration.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
		migration.version, migration.name, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record schema migration %d: %w", migration.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration %d: %w", migration.version, err)
	}
	return nil
}

// hasColumn checks whether a table has a specific column.
func (s *Store) hasColumn(ctx context.Context, table, column string) (bool, error) {
	return hasColumn(ctx, s.db, table, column)
}

type columnQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func hasColumn(ctx context.Context, q columnQuerier, table, column string) (bool, error) {
	quotedTable, err := quoteSQLiteIdent(table)
	if err != nil {
		return false, err
	}
	rows, err := q.QueryContext(ctx, "PRAGMA table_info("+quotedTable+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dfltValue *string
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func quoteSQLiteIdent(ident string) (string, error) {
	if ident == "" {
		return "", fmt.Errorf("empty sqlite identifier")
	}
	for i := 0; i < len(ident); i++ {
		c := ident[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			continue
		}
		return "", fmt.Errorf("invalid sqlite identifier %q", ident)
	}
	return `"` + ident + `"`, nil
}

// InsertIngestedEvent inserts a raw event idempotently.
// Returns true when the event was newly inserted, false when it was already seen.
func (s *Store) InsertIngestedEvent(ctx context.Context, event nostr.Event) (bool, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(
		ctx,
		`INSERT INTO ingested_events(event_id, kind, author_pubkey, created_at, first_seen_at, raw_event_json)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(event_id) DO NOTHING`,
		event.ID.Hex(), int(event.Kind), event.PubKey.Hex(), int64(event.CreatedAt), now, event.String(),
	)
	if err != nil {
		return false, fmt.Errorf("insert ingested event: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return affected == 1, nil
}

func (s *Store) UpsertRepositoryAnnouncement(ctx context.Context, event nostr.Event) error {
	repo := nip34.ParseRepository(event)
	repoID := RepoIDFromAnnouncement(event)
	now := time.Now().Unix()

	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO repositories
			(repo_id, pubkey, identifier, announcement_event_id, name, description, clone_urls, relays, raw_event_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(repo_id) DO UPDATE SET
		    announcement_event_id=excluded.announcement_event_id,
		    name=excluded.name,
		    description=excluded.description,
		    clone_urls=excluded.clone_urls,
		    relays=excluded.relays,
		    raw_event_json=excluded.raw_event_json,
		    updated_at=excluded.updated_at`,
		repoID, event.PubKey.Hex(), repo.ID, event.ID.Hex(), repo.Name, repo.Description,
		strings.Join(repo.Clone, ","), strings.Join(repo.Relays, ","), event.String(), now, now,
	)
	if err != nil {
		return fmt.Errorf("upsert repository: %w", err)
	}
	return nil
}

func (s *Store) GetRepositoryCloneURLs(ctx context.Context, repoID string) ([]string, error) {
	var cloneURLsCSV string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT clone_urls FROM repositories WHERE repo_id=?`,
		repoID,
	).Scan(&cloneURLsCSV)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("repository %s not found", repoID)
		}
		return nil, fmt.Errorf("lookup repository clone urls: %w", err)
	}

	raw := strings.Split(cloneURLsCSV, ",")
	urls := make([]string, 0, len(raw))
	for _, url := range raw {
		if trimmed := strings.TrimSpace(url); trimmed != "" {
			urls = append(urls, trimmed)
		}
	}
	return urls, nil
}

func (s *Store) UpsertRepositorySnapshot(ctx context.Context, event nostr.Event) error {
	state := nip34.ParseRepositoryState(event)
	repoID := event.PubKey.Hex() + ":" + state.ID
	commits := snapshotRefCommits(event)
	slices.Sort(commits)
	commits = slices.Compact(commits)
	now := time.Now().Unix()

	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO repository_snapshots
			(repo_id, snapshot_event_id, author_pubkey, head_branch, ref_commits_csv, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(repo_id) DO UPDATE SET
		    snapshot_event_id=excluded.snapshot_event_id,
		    author_pubkey=excluded.author_pubkey,
		    head_branch=excluded.head_branch,
		    ref_commits_csv=excluded.ref_commits_csv,
		    created_at=excluded.created_at,
		    updated_at=excluded.updated_at
		  WHERE excluded.created_at >= repository_snapshots.created_at`,
		repoID,
		event.ID.Hex(),
		event.PubKey.Hex(),
		state.HEAD,
		strings.Join(commits, ","),
		int64(event.CreatedAt),
		now,
	)
	if err != nil {
		return fmt.Errorf("upsert repository snapshot: %w", err)
	}
	return nil
}

func (s *Store) InsertPatchEvent(ctx context.Context, event nostr.Event) error {
	repoID := RepoIDFromPatch(event)
	rootID := RootEventID(event)
	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(
		ctx,
		`INSERT INTO patch_events
			(event_id, repo_id, kind, author_pubkey, root_id, created_at, content, raw_event_json, seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(event_id) DO NOTHING`,
		event.ID.Hex(), repoID, int(event.Kind), event.PubKey.Hex(), rootID, int64(event.CreatedAt), event.Content, event.String(), now,
	)
	if err != nil {
		return fmt.Errorf("insert patch event: %w", err)
	}

	_, err = tx.ExecContext(
		ctx,
		`INSERT INTO thread_cache(root_id, event_ids, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(root_id) DO UPDATE SET
		    event_ids=thread_cache.event_ids || CASE
		      WHEN instr(',' || thread_cache.event_ids || ',', ',' || excluded.event_ids || ',') > 0 THEN ''
		      WHEN thread_cache.event_ids = '' THEN excluded.event_ids
		      ELSE ',' || excluded.event_ids
		    END,
		    updated_at=excluded.updated_at`,
		rootID, event.ID.Hex(), now,
	)
	if err != nil {
		return fmt.Errorf("upsert thread cache: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

func (s *Store) RecordPatchEventRelay(ctx context.Context, patchEventID, relayURL string) error {
	if strings.TrimSpace(patchEventID) == "" || strings.TrimSpace(relayURL) == "" {
		return nil
	}
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO patch_event_relays(patch_event_id, relay_url, seen_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(patch_event_id, relay_url) DO NOTHING`,
		patchEventID, relayURL, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("record patch relay: %w", err)
	}
	return nil
}

func (s *Store) GetPatchEvent(ctx context.Context, eventID string) (PatchEventRecord, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT event_id, repo_id, root_id, kind, raw_event_json
		   FROM patch_events
		  WHERE event_id=?`,
		eventID,
	)
	var rec PatchEventRecord
	if err := row.Scan(&rec.EventID, &rec.RepoID, &rec.RootID, &rec.Kind, &rec.RawEvent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PatchEventRecord{}, fmt.Errorf("patch event %s not found", eventID)
		}
		return PatchEventRecord{}, fmt.Errorf("get patch event: %w", err)
	}
	return rec, nil
}

func (s *Store) GetPatchAuthorPubKey(ctx context.Context, eventID string) (string, error) {
	var pubkey string
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT author_pubkey FROM patch_events WHERE event_id=?`,
		eventID,
	).Scan(&pubkey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("patch event %s not found", eventID)
		}
		return "", fmt.Errorf("get patch author pubkey: %w", err)
	}
	return pubkey, nil
}

func (s *Store) GetPublishRelays(ctx context.Context, patchEventID, repoID string) ([]string, error) {
	uniq := map[string]struct{}{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" {
			uniq[v] = struct{}{}
		}
	}

	rows, err := s.db.QueryContext(
		ctx,
		`SELECT relay_url FROM patch_event_relays WHERE patch_event_id=?`,
		patchEventID,
	)
	if err != nil {
		return nil, fmt.Errorf("query patch relays: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var relay string
		if err := rows.Scan(&relay); err != nil {
			return nil, fmt.Errorf("scan patch relay row: %w", err)
		}
		add(relay)
	}

	var repoRelaysCSV string
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT relays FROM repositories WHERE repo_id=?`,
		repoID,
	).Scan(&repoRelaysCSV); err == nil {
		for _, relay := range strings.Split(repoRelaysCSV, ",") {
			add(relay)
		}
	}

	out := make([]string, 0, len(uniq))
	for relay := range uniq {
		out = append(out, relay)
	}
	slices.Sort(out)
	return out, nil
}

func (s *Store) ListPatchThreadEvents(ctx context.Context, rootID, repoID string) ([]nostr.Event, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT raw_event_json
		   FROM patch_events
		  WHERE root_id=? AND repo_id=? AND kind=1617
		  ORDER BY created_at ASC`,
		rootID, repoID,
	)
	if err != nil {
		return nil, fmt.Errorf("list patch thread events: %w", err)
	}
	defer rows.Close()

	events := make([]nostr.Event, 0, 16)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan patch thread row: %w", err)
		}
		var evt nostr.Event
		if err := json.Unmarshal([]byte(raw), &evt); err != nil {
			return nil, fmt.Errorf("decode patch thread event json: %w", err)
		}
		events = append(events, evt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate patch thread rows: %w", err)
	}
	return events, nil
}

func (s *Store) UpsertRootStatus(ctx context.Context, event nostr.Event) error {
	rootID := statusRootEventID(event)
	if rootID == "" {
		return nil
	}

	repoID := repoIDFromAddressTags(event.Tags)
	if repoID == "" {
		var inferred string
		if err := s.db.QueryRowContext(ctx, `SELECT repo_id FROM patch_events WHERE event_id=? LIMIT 1`, rootID).Scan(&inferred); err == nil {
			repoID = inferred
		}
	}

	allowed, err := s.isStatusAuthorAllowed(ctx, rootID, repoID, event.PubKey)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}

	now := time.Now().Unix()
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO root_statuses(root_event_id, repo_id, status_kind, status_event_id, author_pubkey, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(root_event_id, repo_id) DO UPDATE SET
		   status_kind=excluded.status_kind,
		   status_event_id=excluded.status_event_id,
		   author_pubkey=excluded.author_pubkey,
		   created_at=excluded.created_at,
		   updated_at=excluded.updated_at
		 WHERE excluded.created_at >= root_statuses.created_at`,
		rootID,
		repoID,
		int(event.Kind),
		event.ID.Hex(),
		event.PubKey.Hex(),
		int64(event.CreatedAt),
		now,
	)
	if err != nil {
		return fmt.Errorf("upsert root status: %w", err)
	}
	return nil
}

func (s *Store) isStatusAuthorAllowed(ctx context.Context, rootID, repoID string, author nostr.PubKey) (bool, error) {
	if strings.TrimSpace(rootID) == "" {
		return false, nil
	}

	var rootAuthorHex string
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT author_pubkey FROM patch_events WHERE event_id=? LIMIT 1`,
		rootID,
	).Scan(&rootAuthorHex); err == nil {
		if strings.EqualFold(rootAuthorHex, author.Hex()) {
			return true, nil
		}
	}

	if strings.TrimSpace(repoID) == "" {
		return false, nil
	}

	var rawRepo string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT raw_event_json FROM repositories WHERE repo_id=? LIMIT 1`,
		repoID,
	).Scan(&rawRepo)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("lookup repository announcement for status auth: %w", err)
	}

	var repoEvt nostr.Event
	if err := json.Unmarshal([]byte(rawRepo), &repoEvt); err != nil {
		return false, fmt.Errorf("decode repository announcement for status auth: %w", err)
	}
	repo := nip34.ParseRepository(repoEvt)
	if repoEvt.PubKey == author {
		return true, nil
	}
	for _, maintainer := range repo.Maintainers {
		if maintainer == author {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) IsRootClosedByStatus(ctx context.Context, rootID, repoID string) (bool, string, error) {
	if strings.TrimSpace(rootID) == "" {
		return false, "", nil
	}

	var statusKind int
	err := s.db.QueryRowContext(
		ctx,
		`SELECT status_kind
		   FROM root_statuses
		  WHERE root_event_id=? AND (repo_id=? OR repo_id='')
		  ORDER BY CASE WHEN repo_id=? THEN 0 ELSE 1 END
		  LIMIT 1`,
		rootID, repoID, repoID,
	).Scan(&statusKind)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("query root status: %w", err)
	}

	switch statusKind {
	case 1631:
		return true, "root status is applied/merged (1631)", nil
	case 1632:
		return true, "root status is closed (1632)", nil
	default:
		return false, "", nil
	}
}

func (s *Store) InsertReviewEvent(ctx context.Context, event nostr.Event, patchEventID, repoID string) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO review_events
			(event_id, patch_event_id, repo_id, created_at, raw_event_json)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(event_id) DO NOTHING`,
		event.ID.Hex(), patchEventID, repoID, int64(event.CreatedAt), event.String(),
	)
	if err != nil {
		return fmt.Errorf("insert review event: %w", err)
	}
	return nil
}

// GetReviewPublication returns an exact signed event reserved for relay delivery.
// The stored event is reused across retries so a repeated relay publish has the
// same Nostr event ID and is idempotent.
func (s *Store) GetReviewPublication(ctx context.Context, patchEventID, repoID, eventType string, detailIndex int) (event nostr.Event, delivered, found bool, err error) {
	var raw string
	var deliveredAt int64
	err = s.db.QueryRowContext(ctx,
		`SELECT raw_event_json, delivered_at
		FROM review_publication_outbox
		WHERE patch_event_id=? AND repo_id=? AND event_type=? AND detail_index=?`,
		patchEventID, repoID, eventType, detailIndex,
	).Scan(&raw, &deliveredAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nostr.Event{}, false, false, nil
		}
		return nostr.Event{}, false, false, fmt.Errorf("get review publication: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		return nostr.Event{}, false, false, fmt.Errorf("decode reserved review publication: %w", err)
	}
	return event, deliveredAt > 0, true, nil
}

// ReserveReviewPublication durably stores a signed event before relay delivery.
// If another attempt already reserved this logical event, that exact event is
// returned instead of replacing it.
func (s *Store) ReserveReviewPublication(ctx context.Context, patchEventID, repoID, eventType string, detailIndex int, event nostr.Event) (nostr.Event, bool, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO review_publication_outbox
			(patch_event_id, repo_id, event_type, detail_index, event_id, raw_event_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(patch_event_id, repo_id, event_type, detail_index) DO NOTHING`,
		patchEventID, repoID, eventType, detailIndex, event.ID.Hex(), event.String(), time.Now().Unix(),
	)
	if err != nil {
		return nostr.Event{}, false, fmt.Errorf("reserve review publication: %w", err)
	}
	reserved, delivered, found, err := s.GetReviewPublication(ctx, patchEventID, repoID, eventType, detailIndex)
	if err != nil {
		return nostr.Event{}, false, err
	}
	if !found {
		return nostr.Event{}, false, errors.New("reserved review publication not found")
	}
	return reserved, delivered, nil
}

func (s *Store) MarkReviewPublicationDelivered(ctx context.Context, patchEventID, repoID, eventType string, detailIndex int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE review_publication_outbox SET delivered_at=?
		WHERE patch_event_id=? AND repo_id=? AND event_type=? AND detail_index=?`,
		time.Now().Unix(), patchEventID, repoID, eventType, detailIndex,
	)
	if err != nil {
		return fmt.Errorf("mark review publication delivered: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("review publication rows affected: %w", err)
	}
	if affected == 0 {
		return errors.New("review publication reservation not found")
	}
	return nil
}

func (s *Store) UpsertThreadCache(ctx context.Context, rootID, eventID string, now int64) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO thread_cache(root_id, event_ids, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(root_id) DO UPDATE SET
		    event_ids=thread_cache.event_ids || CASE
		      WHEN instr(',' || thread_cache.event_ids || ',', ',' || excluded.event_ids || ',') > 0 THEN ''
		      WHEN thread_cache.event_ids = '' THEN excluded.event_ids
		      ELSE ',' || excluded.event_ids
		    END,
		    updated_at=excluded.updated_at`,
		rootID, eventID, now,
	)
	if err != nil {
		return fmt.Errorf("upsert thread cache: %w", err)
	}
	return nil
}

// BeginReview transitions a patch/repo from pending|failed -> reviewing.
// Returns true if caller obtained the lock and should proceed.
func (s *Store) BeginReview(ctx context.Context, patchEventID, repoID string) (bool, error) {
	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(
		ctx,
		`INSERT INTO review_log(patch_event_id, repo_id, status, created_at, updated_at)
		 VALUES (?, ?, 'pending', ?, ?)
		 ON CONFLICT(patch_event_id, repo_id) DO NOTHING`,
		patchEventID, repoID, now, now,
	)
	if err != nil {
		return false, fmt.Errorf("ensure review_log row: %w", err)
	}

	res, err := tx.ExecContext(
		ctx,
		`UPDATE review_log
		    SET status='reviewing', failure_reason=NULL, updated_at=?
		  WHERE patch_event_id=? AND repo_id=? AND status IN ('pending', 'failed')`,
		now, patchEventID, repoID,
	)
	if err != nil {
		return false, fmt.Errorf("begin review transition: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}

	if affected == 1 {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit transaction: %w", err)
		}
		return true, nil
	}

	var status string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT status FROM review_log WHERE patch_event_id=? AND repo_id=?`,
		patchEventID, repoID,
	).Scan(&status); err != nil {
		return false, fmt.Errorf("lookup existing review state: %w", err)
	}
	if status == "published" {
		return false, ErrReviewAlreadyPublished
	}
	return false, nil
}

func (s *Store) MarkReviewPublished(ctx context.Context, patchEventID, repoID, reviewEventID string) error {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(
		ctx,
		`UPDATE review_log
		    SET status='published', review_event_id=?, failure_reason=NULL, updated_at=?
		  WHERE patch_event_id=? AND repo_id=?`,
		reviewEventID, now, patchEventID, repoID,
	)
	if err != nil {
		return fmt.Errorf("mark review published: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return ErrReviewNotFound
	}
	return nil
}

// GetPublishedStatusEvent returns the previously published NIP-34 status event
// for a given patch/repo pair, if any. Returns empty strings and zero values
// when no status has been published.
func (s *Store) GetPublishedStatusEvent(ctx context.Context, patchEventID, repoID string) (eventID string, kind int, publishedAt int64, err error) {
	err = s.db.QueryRowContext(
		ctx,
		`SELECT COALESCE(status_event_id, ''), status_event_kind, status_published_at
		FROM review_log WHERE patch_event_id=? AND repo_id=?`,
		patchEventID, repoID,
	).Scan(&eventID, &kind, &publishedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, 0, nil
		}
		return "", 0, 0, fmt.Errorf("get published status event: %w", err)
	}
	return eventID, kind, publishedAt, nil
}

// RecordStatusPublished records a successfully published NIP-34 status event
// in the review_log for duplicate suppression. Only writes if no status has
// been recorded yet for this patch/repo pair.
func (s *Store) RecordStatusPublished(ctx context.Context, patchEventID, repoID, eventID string, kind int) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE review_log
		SET status_event_id=?, status_event_kind=?, status_published_at=?, updated_at=?
		WHERE patch_event_id=? AND repo_id=?
		AND (status_event_id IS NULL OR status_event_id = '')`,
		eventID, kind, now, now, patchEventID, repoID,
	)
	if err != nil {
		return fmt.Errorf("record status published: %w", err)
	}
	return nil
}

// GetRootStatus returns the current effective NIP-34 status for a root event
// in a given repository. Returns ok=false if no status exists.
func (s *Store) GetRootStatus(ctx context.Context, rootID, repoID string) (kind int, eventID string, createdAt int64, ok bool, err error) {
	err = s.db.QueryRowContext(
		ctx,
		`SELECT status_kind, status_event_id, created_at
		FROM root_statuses
		WHERE root_event_id=? AND (repo_id=? OR repo_id='')
		ORDER BY created_at DESC LIMIT 1`,
		rootID, repoID,
	).Scan(&kind, &eventID, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", 0, false, nil
		}
		return 0, "", 0, false, fmt.Errorf("get root status: %w", err)
	}
	return kind, eventID, createdAt, true, nil
}

// CanStatusAuthor checks whether the given pubkey is authorized to publish
// NIP-34 status events for the given root event in the given repository.
// This is a public wrapper around the existing isStatusAuthorAllowed logic.
func (s *Store) CanStatusAuthor(ctx context.Context, rootID, repoID string, author nostr.PubKey) (bool, error) {
	return s.isStatusAuthorAllowed(ctx, rootID, repoID, author)
}

// RecordReviewNote attaches an observable best-effort sub-stage outcome to an
// existing review without changing its pipeline status.
func (s *Store) RecordReviewNote(ctx context.Context, patchEventID, repoID, note string) error {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE review_log SET failure_reason=?, updated_at=?
		WHERE patch_event_id=? AND repo_id=?`,
		note, now, patchEventID, repoID,
	)
	if err != nil {
		return fmt.Errorf("record review note: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("review note rows affected: %w", err)
	}
	if affected == 0 {
		return ErrReviewNotFound
	}
	return nil
}

// GetReviewNote returns the current note attached to a review.
func (s *Store) GetReviewNote(ctx context.Context, patchEventID, repoID string) (string, error) {
	var note sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT failure_reason FROM review_log WHERE patch_event_id=? AND repo_id=?`,
		patchEventID, repoID,
	).Scan(&note); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrReviewNotFound
		}
		return "", fmt.Errorf("get review note: %w", err)
	}
	return note.String, nil
}

func (s *Store) MarkReviewFailed(ctx context.Context, patchEventID, repoID, reason string) error {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(
		ctx,
		`UPDATE review_log
		    SET status='failed', failure_reason=?, updated_at=?
		  WHERE patch_event_id=? AND repo_id=?`,
		reason, now, patchEventID, repoID,
	)
	if err != nil {
		return fmt.Errorf("mark review failed: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return ErrReviewNotFound
	}
	return nil
}

func (s *Store) CountIngestedEvents(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ingested_events`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count ingested events: %w", err)
	}
	return n, nil
}

func (s *Store) CountPatchEvents(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM patch_events`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count patch events: %w", err)
	}
	return n, nil
}

func (s *Store) CountRepositories(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repositories`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count repositories: %w", err)
	}
	return n, nil
}

// GetReviewStatus returns the current status of a review (pending, reviewing,
// published, failed) or empty string if no review log entry exists.
func (s *Store) GetReviewStatus(ctx context.Context, patchEventID, repoID string) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM review_log WHERE patch_event_id=? AND repo_id=?`,
		patchEventID, repoID,
	).Scan(&status)
	if err != nil {
		return "", fmt.Errorf("get review status: %w", err)
	}
	return status, nil
}

// GetReviewEventID returns the review_event_id for a review log entry, or empty
// string if no review event has been recorded yet. Used to detect partially-
// completed publishes from prior runs (crash recovery idempotency).
func (s *Store) GetReviewEventID(ctx context.Context, patchEventID, repoID string) (string, error) {
	var reviewEventID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT review_event_id FROM review_log WHERE patch_event_id=? AND repo_id=?`,
		patchEventID, repoID,
	).Scan(&reviewEventID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get review event id: %w", err)
	}
	if reviewEventID.Valid {
		return reviewEventID.String, nil
	}
	return "", nil
}

// SetReviewEventID records the signed review event ID on the review_log entry
// before publishing to relays. This is the crash-recovery breadcrumb: if the
// process crashes after relay publish but before MarkReviewPublished, the
// idempotency check in PublishReview will find this event ID and skip
// re-publishing.
func (s *Store) SetReviewEventID(ctx context.Context, patchEventID, repoID, reviewEventID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE review_log SET review_event_id=?
		WHERE patch_event_id=? AND repo_id=?
		AND (review_event_id IS NULL OR review_event_id=?)`,
		reviewEventID, patchEventID, repoID, reviewEventID,
	)
	if err != nil {
		return fmt.Errorf("set review event id: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set review event id rows affected: %w", err)
	}
	if affected == 0 {
		var exists int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM review_log WHERE patch_event_id=? AND repo_id=?`,
			patchEventID, repoID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check review event id reservation: %w", err)
		}
		if exists == 0 {
			return ErrReviewNotFound
		}
	}
	return nil
}

// ClearReviewEventID removes a provisional review event ID that was set before
// publishing but for which the actual relay publish failed. This allows the
// next retry to generate and publish a new event rather than incorrectly
// assuming the prior event was already published.
func (s *Store) ClearReviewEventID(ctx context.Context, patchEventID, repoID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE review_log SET review_event_id=NULL WHERE patch_event_id=? AND repo_id=? AND status != 'published'`,
		patchEventID, repoID,
	)
	if err != nil {
		return fmt.Errorf("clear review event id: %w", err)
	}
	return nil
}

// GetReviewEventIDAndStatus returns the review_event_id and status for a review
// log entry. Used for idempotency checks that need to verify both that an event
// was signed AND successfully published. Returns empty strings if not found.
func (s *Store) GetReviewEventIDAndStatus(ctx context.Context, patchEventID, repoID string) (eventID, status string, err error) {
	var reviewEventID sql.NullString
	var reviewStatus sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT review_event_id, status FROM review_log WHERE patch_event_id=? AND repo_id=?`,
		patchEventID, repoID,
	).Scan(&reviewEventID, &reviewStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("get review event id and status: %w", err)
	}
	if reviewEventID.Valid {
		eventID = reviewEventID.String
	}
	if reviewStatus.Valid {
		status = reviewStatus.String
	}
	return eventID, status, nil
}

func (s *Store) CountReviewLog(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_log`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count review_log: %w", err)
	}
	return n, nil
}

func (s *Store) CountRepositorySnapshots(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_snapshots`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count repository snapshots: %w", err)
	}
	return n, nil
}

func (s *Store) InsertMetaReviewLog(ctx context.Context, patchEventID, repoID, contextHash string, changedLines []string, gateReason, responseJSON string) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO meta_review_log(patch_event_id, repo_id, context_hash, changed_lines_csv, gate_reason, response_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		patchEventID,
		repoID,
		contextHash,
		strings.Join(changedLines, ","),
		gateReason,
		responseJSON,
		time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("insert meta review log: %w", err)
	}
	return nil
}

func (s *Store) FindReusableMetaReview(ctx context.Context, contextHash string, changedLines []string, minJaccard float64) (*MetaReviewReuse, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT changed_lines_csv, response_json
		   FROM meta_review_log
		  WHERE context_hash=?
		  ORDER BY created_at DESC`,
		contextHash,
	)
	if err != nil {
		return nil, fmt.Errorf("query reusable meta review rows: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var changedCSV, response string
		if err := rows.Scan(&changedCSV, &response); err != nil {
			return nil, fmt.Errorf("scan reusable meta review row: %w", err)
		}
		score := jaccard(changedLines, splitCSV(changedCSV))
		if score >= minJaccard {
			return &MetaReviewReuse{ResponseJSON: response}, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reusable meta review rows: %w", err)
	}
	return nil, nil
}

func (s *Store) InsertMetaReviewRoute(ctx context.Context, patchEventID, repoID, whyMissed, action string) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO meta_review_routes(patch_event_id, repo_id, why_missed, action, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		patchEventID, repoID, whyMissed, action, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("insert meta review route: %w", err)
	}
	return nil
}

func (s *Store) InsertFewShot(ctx context.Context, patchEventID, repoID, exampleType, content string, confidence float64) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO few_shot_reviews(patch_event_id, repo_id, example_type, content, confidence, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		patchEventID, repoID, exampleType, content, confidence, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("insert few shot review: %w", err)
	}
	return nil
}

func (s *Store) GetRecentFewShots(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT content FROM few_shot_reviews
		WHERE example_type = 'positive'
		ORDER BY created_at DESC
		LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get recent few shots: %w", err)
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return nil, fmt.Errorf("scan few shot row: %w", err)
		}
		results = append(results, content)
	}
	return results, rows.Err()
}

func (s *Store) PruneFewShotToCap(ctx context.Context, cap int) error {
	if cap <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(
		ctx,
		`DELETE FROM few_shot_reviews
		  WHERE id IN (
		    SELECT id FROM few_shot_reviews
		    ORDER BY confidence ASC, created_at ASC
		    LIMIT (
		      SELECT CASE WHEN COUNT(*) > ? THEN COUNT(*) - ? ELSE 0 END FROM few_shot_reviews
		    )
		  )`,
		cap, cap,
	)
	if err != nil {
		return fmt.Errorf("prune few shot cap: %w", err)
	}
	return nil
}

func (s *Store) InsertEvalRun(
	ctx context.Context,
	datasetID string,
	totalCases int,
	expectedFindings int,
	predictedFindings int,
	truePositives int,
	falsePositives int,
	falseNegatives int,
	recall float64,
	falsePositiveRate float64,
	calibrationMAE float64,
	highConfidencePrecision float64,
	detailsJSON string,
) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO eval_runs(
		    dataset_id, total_cases, expected_findings, predicted_findings, true_positives, false_positives, false_negatives,
		    recall, false_positive_rate, calibration_mae, high_conf_precision, details_json, created_at
		  ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		datasetID,
		totalCases,
		expectedFindings,
		predictedFindings,
		truePositives,
		falsePositives,
		falseNegatives,
		recall,
		falsePositiveRate,
		calibrationMAE,
		highConfidencePrecision,
		detailsJSON,
		time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("insert eval run: %w", err)
	}
	return nil
}

func (s *Store) CountEvalRuns(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM eval_runs`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count eval runs: %w", err)
	}
	return n, nil
}

func (s *Store) IsPatchStaleBySnapshot(ctx context.Context, event nostr.Event) (bool, string, error) {
	repoID := RepoIDFromPatch(event)
	if repoID == "" {
		return false, "", nil
	}

	var commitsCSV string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT ref_commits_csv FROM repository_snapshots WHERE repo_id=?`,
		repoID,
	).Scan(&commitsCSV)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("read snapshot commits: %w", err)
	}

	known := make(map[string]struct{}, 32)
	for _, c := range strings.Split(commitsCSV, ",") {
		c = strings.TrimSpace(c)
		if c != "" {
			known[c] = struct{}{}
		}
	}

	for _, c := range patchTipCandidates(event) {
		if _, ok := known[c]; ok {
			return true, "patch tip already present in latest repository snapshot refs", nil
		}
	}
	return false, "", nil
}

func RepoIDFromAnnouncement(event nostr.Event) string {
	repo := nip34.ParseRepository(event)
	return event.PubKey.Hex() + ":" + repo.ID
}

func RepoIDFromPatch(event nostr.Event) string {
	patch := nip34.ParsePatch(event)
	if patch.Repository.PublicKey == nostr.ZeroPK || patch.Repository.Identifier == "" {
		return ""
	}
	return patch.Repository.PublicKey.Hex() + ":" + patch.Repository.Identifier
}

func RootEventID(event nostr.Event) string {
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		if tag[0] != "e" && tag[0] != "E" {
			continue
		}
		if len(tag) >= 4 && tag[3] == "root" {
			return tag[1]
		}
	}

	// NIP-34 PR updates carry root with NIP-22 `E`.
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "E" {
			return tag[1]
		}
	}
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			return tag[1]
		}
	}
	return event.ID.Hex()
}

func statusRootEventID(event nostr.Event) string {
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		if tag[0] != "e" && tag[0] != "E" {
			continue
		}
		if len(tag) >= 4 && tag[3] == "root" {
			return tag[1]
		}
	}
	for _, tag := range event.Tags {
		if len(tag) >= 2 && (tag[0] == "e" || tag[0] == "E") {
			return tag[1]
		}
	}
	return ""
}

func repoIDFromAddressTags(tags nostr.Tags) string {
	for _, tag := range tags {
		if len(tag) < 2 {
			continue
		}
		if tag[0] != "a" && tag[0] != "A" {
			continue
		}
		spl := strings.SplitN(tag[1], ":", 3)
		if len(spl) != 3 || spl[0] != "30617" {
			continue
		}
		if !isHex(spl[1], 64) {
			continue
		}
		return strings.ToLower(spl[1]) + ":" + spl[2]
	}
	return ""
}

func patchTipCandidates(event nostr.Event) []string {
	candidates := make([]string, 0, 2)
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		if tag[0] != "commit" && tag[0] != "c" {
			continue
		}
		if isHex(tag[1], 40) {
			candidates = append(candidates, strings.ToLower(tag[1]))
		}
	}
	return candidates
}

func snapshotRefCommits(event nostr.Event) []string {
	commits := make([]string, 0, 16)
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		if !strings.HasPrefix(tag[0], "refs/heads/") && !strings.HasPrefix(tag[0], "refs/tags/") {
			continue
		}
		for _, candidate := range tag[1:] {
			if isHex(candidate, 40) {
				commits = append(commits, strings.ToLower(candidate))
			}
		}
	}
	return commits
}

func isHex(v string, exactLen int) bool {
	if len(v) != exactLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func splitCSV(v string) []string {
	raw := strings.Split(v, ",")
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func jaccard(a, b []string) float64 {
	as := map[string]struct{}{}
	bs := map[string]struct{}{}
	for _, v := range a {
		if v != "" {
			as[v] = struct{}{}
		}
	}
	for _, v := range b {
		if v != "" {
			bs[v] = struct{}{}
		}
	}
	if len(as) == 0 && len(bs) == 0 {
		return 1
	}
	intersection := 0
	union := map[string]struct{}{}
	for v := range as {
		union[v] = struct{}{}
		if _, ok := bs[v]; ok {
			intersection++
		}
	}
	for v := range bs {
		union[v] = struct{}{}
	}
	if len(union) == 0 {
		return 0
	}
	return float64(intersection) / float64(len(union))
}

// GetListenerHighWaterMark returns the timestamp of the most recently processed
// event, or 0 if no events have been tracked yet.
func (s *Store) GetListenerHighWaterMark(ctx context.Context) (int64, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM listener_state WHERE key='high_water_mark'`,
	).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("get listener high water mark: %w", err)
	}
	var ts int64
	if _, err := fmt.Sscanf(value, "%d", &ts); err != nil {
		return 0, nil
	}
	return ts, nil
}

// UpdateListenerHighWaterMark persists the timestamp of the most recently processed
// event so it can be used for lookback on restart.
func (s *Store) UpdateListenerHighWaterMark(ctx context.Context, ts int64) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO listener_state(key, value, updated_at)
		VALUES ('high_water_mark', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at
		WHERE CAST(excluded.value AS INTEGER) > CAST(listener_state.value AS INTEGER)`,
		fmt.Sprintf("%d", ts), now,
	)
	if err != nil {
		return fmt.Errorf("update listener high water mark: %w", err)
	}
	return nil
}

// ReviewTask is a queued patch/repo pair ready for pipeline execution.
type ReviewTask struct {
	PatchEventID string
	RepoID       string
}

// ResetStuckReviews transitions entries stuck in "reviewing" (e.g. from a crash)
// back to "pending" so they can be retried.
func (s *Store) ResetStuckReviews(ctx context.Context) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE review_log SET status='pending', updated_at=?
		  WHERE status='reviewing'`, now)
	if err != nil {
		return 0, fmt.Errorf("reset stuck reviews: %w", err)
	}
	return res.RowsAffected()
}

// RequeueFailedReviews transitions entries in "failed" state back to "pending"
// if they were last updated more than minAge seconds ago. This recovers tasks
// that failed due to transient issues (queue overflow, temporary LLM failures).
// Returns the tasks that were requeued.
func (s *Store) RequeueFailedReviews(ctx context.Context, minAgeSeconds int64, limit int) ([]ReviewTask, error) {
	now := time.Now().Unix()
	cutoff := now - minAgeSeconds

	// Exclude permanent payment denials from requeue (they start with 'payment_blocked:')
	rows, err := s.db.QueryContext(ctx,
		`SELECT patch_event_id, repo_id FROM review_log
		WHERE status='failed' AND updated_at < ?
		AND (failure_reason IS NULL OR failure_reason = '' OR failure_reason NOT LIKE 'payment_blocked:%')
		ORDER BY updated_at ASC
		LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("query failed reviews: %w", err)
	}
	defer rows.Close()

	var tasks []ReviewTask
	for rows.Next() {
		var t ReviewTask
		if err := rows.Scan(&t.PatchEventID, &t.RepoID); err != nil {
			return nil, fmt.Errorf("scan failed review: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate failed reviews: %w", err)
	}

	for _, t := range tasks {
		_, err := s.db.ExecContext(ctx,
			`UPDATE review_log SET status='pending', failure_reason=NULL, updated_at=?
			WHERE patch_event_id=? AND repo_id=? AND status='failed'`,
			now, t.PatchEventID, t.RepoID)
		if err != nil {
			return nil, fmt.Errorf("requeue failed review %s: %w", t.PatchEventID, err)
		}
	}
	return tasks, nil
}

// IsPatchSuperseded returns true if a newer patch event exists for the same
// root_id and repo_id. A patch is considered superseded when a later revision
// has been submitted to the same thread.
func (s *Store) IsPatchSuperseded(ctx context.Context, eventID, rootID, repoID string) (bool, error) {
	if rootID == "" || rootID == eventID {
		// This IS the root event — check if any children exist with later timestamps.
		var count int64
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM patch_events
			WHERE root_id=? AND repo_id=? AND event_id!=? AND kind IN (1617,1618,1619)`,
			eventID, repoID, eventID,
		).Scan(&count)
		if err != nil {
			return false, fmt.Errorf("check superseded (root): %w", err)
		}
		return count > 0, nil
	}

	// Non-root patch: superseded if a newer event in the same thread exists.
	var newerCount int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM patch_events
		WHERE root_id=? AND repo_id=? AND event_id!=?
		AND kind IN (1617,1618,1619)
		AND created_at > (SELECT created_at FROM patch_events WHERE event_id=?)`,
		rootID, repoID, eventID, eventID,
	).Scan(&newerCount)
	if err != nil {
		return false, fmt.Errorf("check superseded: %w", err)
	}
	return newerCount > 0, nil
}

// --- Prompt Gap Queue ---

// InsertPromptGap enqueues a prompt gap identified by meta-review.
func (s *Store) InsertPromptGap(ctx context.Context, patchEventID, repoID, gapText string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO prompt_gap_queue(patch_event_id, repo_id, gap_text, consumed, created_at)
		VALUES (?, ?, ?, 0, ?)`,
		patchEventID, repoID, gapText, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("insert prompt gap: %w", err)
	}
	return nil
}

// CountUnconsumedPromptGaps returns the number of prompt gaps not yet processed.
func (s *Store) CountUnconsumedPromptGaps(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM prompt_gap_queue WHERE consumed=0`,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count unconsumed prompt gaps: %w", err)
	}
	return n, nil
}

// PromptGapRecord is a single prompt gap entry from the queue.
type PromptGapRecord struct {
	ID      int64
	GapText string
}

// FetchUnconsumedPromptGaps retrieves up to limit unconsumed prompt gaps ordered by creation time.
func (s *Store) FetchUnconsumedPromptGaps(ctx context.Context, limit int) ([]PromptGapRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, gap_text FROM prompt_gap_queue
		WHERE consumed=0
		ORDER BY created_at ASC
		LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("fetch unconsumed prompt gaps: %w", err)
	}
	defer rows.Close()

	var gaps []PromptGapRecord
	for rows.Next() {
		var g PromptGapRecord
		if err := rows.Scan(&g.ID, &g.GapText); err != nil {
			return nil, fmt.Errorf("scan prompt gap row: %w", err)
		}
		gaps = append(gaps, g)
	}
	return gaps, rows.Err()
}

// MarkPromptGapsConsumed sets consumed=1 for the given gap IDs.
func (s *Store) MarkPromptGapsConsumed(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE prompt_gap_queue SET consumed=1 WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("mark prompt gaps consumed: %w", err)
	}
	return nil
}

// --- Prompt Versions ---

// PromptVersion represents a stored version of a named prompt.
type PromptVersion struct {
	ID            int64
	PromptName    string
	Version       int
	Content       string
	ParentVersion int
	SourceGapIDs  string
	Status        string
	EvalScore     *float64
	CreatedAt     int64
}

// InsertPromptVersion stores a new prompt version as candidate.
// Returns the auto-generated row ID.
func (s *Store) InsertPromptVersion(ctx context.Context, promptName, content string, parentVersion int, sourceGapIDs string) (int64, error) {
	// Determine next version number for this prompt.
	var maxVersion int
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM prompt_versions WHERE prompt_name=?`,
		promptName,
	).Scan(&maxVersion)
	if err != nil {
		return 0, fmt.Errorf("get max prompt version: %w", err)
	}
	nextVersion := maxVersion + 1
	now := time.Now().Unix()

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO prompt_versions(prompt_name, version, content, parent_version, source_gap_ids, status, created_at)
		VALUES (?, ?, ?, ?, ?, 'candidate', ?)`,
		promptName, nextVersion, content, parentVersion, sourceGapIDs, now,
	)
	if err != nil {
		return 0, fmt.Errorf("insert prompt version: %w", err)
	}
	return res.LastInsertId()
}

// GetActivePromptVersion returns the active version for the given prompt name.
// Returns sql.ErrNoRows wrapped in error if no active version exists.
func (s *Store) GetActivePromptVersion(ctx context.Context, promptName string) (PromptVersion, error) {
	var pv PromptVersion
	var evalScore sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, prompt_name, version, content, parent_version, source_gap_ids, status, eval_score, created_at
		FROM prompt_versions
		WHERE prompt_name=? AND status='active'
		ORDER BY version DESC LIMIT 1`,
		promptName,
	).Scan(&pv.ID, &pv.PromptName, &pv.Version, &pv.Content, &pv.ParentVersion, &pv.SourceGapIDs, &pv.Status, &evalScore, &pv.CreatedAt)
	if err != nil {
		return PromptVersion{}, fmt.Errorf("get active prompt version %q: %w", promptName, err)
	}
	if evalScore.Valid {
		pv.EvalScore = &evalScore.Float64
	}
	return pv, nil
}

// ActivatePromptVersion sets the given version to 'active' and demotes any
// previously active version for the same prompt_name to 'candidate'.
func (s *Store) ActivatePromptVersion(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var promptName string
	if err := tx.QueryRowContext(ctx,
		`SELECT prompt_name FROM prompt_versions WHERE id=?`, id,
	).Scan(&promptName); err != nil {
		return fmt.Errorf("lookup prompt version for activation: %w", err)
	}

	// Demote current active version(s).
	_, err = tx.ExecContext(ctx,
		`UPDATE prompt_versions SET status='candidate'
		WHERE prompt_name=? AND status='active'`, promptName)
	if err != nil {
		return fmt.Errorf("demote active prompt version: %w", err)
	}

	// Activate the requested version.
	_, err = tx.ExecContext(ctx,
		`UPDATE prompt_versions SET status='active' WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("activate prompt version: %w", err)
	}

	return tx.Commit()
}

// RollbackPromptVersion marks a version as rolled_back and re-activates its parent.
func (s *Store) RollbackPromptVersion(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var promptName string
	var parentVersion int
	if err := tx.QueryRowContext(ctx,
		`SELECT prompt_name, parent_version FROM prompt_versions WHERE id=?`, id,
	).Scan(&promptName, &parentVersion); err != nil {
		return fmt.Errorf("lookup prompt version for rollback: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE prompt_versions SET status='rolled_back' WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("mark prompt version rolled back: %w", err)
	}

	// Re-activate the parent version if it exists.
	if parentVersion > 0 {
		_, err = tx.ExecContext(ctx,
			`UPDATE prompt_versions SET status='active'
			WHERE prompt_name=? AND version=?`, promptName, parentVersion)
		if err != nil {
			return fmt.Errorf("re-activate parent prompt version: %w", err)
		}
	}

	return tx.Commit()
}

// SetPromptVersionEvalScore records the evaluation score for a prompt version.
func (s *Store) SetPromptVersionEvalScore(ctx context.Context, id int64, score float64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE prompt_versions SET eval_score=? WHERE id=?`, score, id)
	if err != nil {
		return fmt.Errorf("set prompt version eval score: %w", err)
	}
	return nil
}

// GetPromptVersionByNumber returns a prompt version by its name and version number.
func (s *Store) GetPromptVersionByNumber(ctx context.Context, promptName string, version int) (PromptVersion, error) {
	var pv PromptVersion
	var evalScore sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, prompt_name, version, content, parent_version, source_gap_ids, status, eval_score, created_at
		FROM prompt_versions
		WHERE prompt_name=? AND version=?`,
		promptName, version,
	).Scan(&pv.ID, &pv.PromptName, &pv.Version, &pv.Content, &pv.ParentVersion, &pv.SourceGapIDs, &pv.Status, &evalScore, &pv.CreatedAt)
	if err != nil {
		return PromptVersion{}, fmt.Errorf("get prompt version %q v%d: %w", promptName, version, err)
	}
	if evalScore.Valid {
		pv.EvalScore = &evalScore.Float64
	}
	return pv, nil
}

// --- Drift Guard ---

// SampleRecentMetaReviews returns a random sample of N recent meta-reviews.
func (s *Store) SampleRecentMetaReviews(ctx context.Context, n int) ([]MetaReviewSample, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, patch_event_id, repo_id, gate_reason, response_json, created_at
		FROM meta_review_log
		WHERE response_json != ''
		ORDER BY RANDOM()
		LIMIT ?`, n,
	)
	if err != nil {
		return nil, fmt.Errorf("sample meta reviews: %w", err)
	}
	defer rows.Close()

	var samples []MetaReviewSample
	for rows.Next() {
		var s MetaReviewSample
		if err := rows.Scan(&s.ID, &s.PatchEventID, &s.RepoID, &s.GateReason, &s.ResponseJSON, &s.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan meta review sample: %w", err)
		}
		samples = append(samples, s)
	}
	return samples, rows.Err()
}

// InsertDriftFlag marks a meta-review as exhibiting convention drift.
func (s *Store) InsertDriftFlag(ctx context.Context, metaReviewID int64, notes string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO drift_flags(meta_review_id, notes, flagged_at) VALUES (?, ?, ?)`,
		metaReviewID, notes, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("insert drift flag: %w", err)
	}
	return nil
}

// GetDriftFlaggedExamples returns the response_json and notes for all drift-flagged meta-reviews.
func (s *Store) GetDriftFlaggedExamples(ctx context.Context, limit int) ([]DriftFlag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT df.id, df.meta_review_id, df.notes, df.flagged_at
		FROM drift_flags df
		ORDER BY df.flagged_at DESC
		LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get drift flagged examples: %w", err)
	}
	defer rows.Close()

	var flags []DriftFlag
	for rows.Next() {
		var f DriftFlag
		if err := rows.Scan(&f.ID, &f.MetaReviewID, &f.Notes, &f.FlaggedAt); err != nil {
			return nil, fmt.Errorf("scan drift flag: %w", err)
		}
		flags = append(flags, f)
	}
	return flags, rows.Err()
}

// GetDriftFlaggedResponses returns response JSON and notes for flagged reviews,
// used as negative examples in the meta-reviewer prompt.
func (s *Store) GetDriftFlaggedResponses(ctx context.Context, limit int) ([]struct {
	ResponseJSON string
	Notes        string
}, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.response_json, df.notes
		FROM drift_flags df
		JOIN meta_review_log m ON m.id = df.meta_review_id
		ORDER BY df.flagged_at DESC
		LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get drift flagged responses: %w", err)
	}
	defer rows.Close()

	var results []struct {
		ResponseJSON string
		Notes        string
	}
	for rows.Next() {
		var r struct {
			ResponseJSON string
			Notes        string
		}
		if err := rows.Scan(&r.ResponseJSON, &r.Notes); err != nil {
			return nil, fmt.Errorf("scan drift flagged response: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// GetLatestEvalRecall returns the recall from the most recent eval run, or 0 if none.
func (s *Store) GetLatestEvalRecall(ctx context.Context) (float64, error) {
	var recall float64
	err := s.db.QueryRowContext(ctx,
		`SELECT recall FROM eval_runs ORDER BY created_at DESC LIMIT 1`,
	).Scan(&recall)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("get latest eval recall: %w", err)
	}
	return recall, nil
}

// --- Conversations ---

// ConversationTurn represents a single turn in a review conversation.
type ConversationTurn struct {
	ID              int64
	ReviewEventID   string
	ReplyEventID    string
	ResponseEventID string
	RepoID          string
	PatchEventID    string
	ReplyAuthor     string
	ReplyContent    string
	ResponseContent string
	TurnNumber      int
	Status          string
	CreatedAt       int64
}

var ErrConversationRateLimited = errors.New("conversation rate limit reached")

// BeginConversationTurn atomically checks the turn limit and inserts a new
// conversation turn in a single transaction. Returns the assigned turn number
// or ErrConversationRateLimited if the limit is reached.
// If the reply_event_id already exists with status "failed", it is retried.
func (s *Store) BeginConversationTurn(ctx context.Context, turn ConversationTurn, maxTurns int) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin conversation tx: %w", err)
	}
	defer tx.Rollback()

	// Check if this reply was already processed (duplicate or retry).
	var existingStatus string
	var existingTurn int
	err = tx.QueryRowContext(ctx,
		`SELECT turn_number, status FROM conversations WHERE reply_event_id=?`,
		turn.ReplyEventID,
	).Scan(&existingTurn, &existingStatus)
	if err == nil {
		// Row exists.
		if existingStatus == "published" {
			return existingTurn, nil // already done — idempotent
		}
		if existingStatus == "failed" || existingStatus == "pending" {
			// Allow retry — return the existing turn number.
			if err := tx.Commit(); err != nil {
				return 0, fmt.Errorf("commit conversation retry: %w", err)
			}
			return existingTurn, nil
		}
		return existingTurn, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("check existing conversation: %w", err)
	}

	// Count existing turns (atomically within transaction).
	var turnCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM conversations WHERE review_event_id=?`,
		turn.ReviewEventID,
	).Scan(&turnCount); err != nil {
		return 0, fmt.Errorf("count conversation turns: %w", err)
	}
	if turnCount >= maxTurns {
		return 0, ErrConversationRateLimited
	}

	nextTurn := turnCount + 1
	_, err = tx.ExecContext(ctx,
		`INSERT INTO conversations(review_event_id, reply_event_id, response_event_id, repo_id, patch_event_id,
			reply_author, reply_content, response_content, turn_number, status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		turn.ReviewEventID, turn.ReplyEventID, turn.ResponseEventID, turn.RepoID, turn.PatchEventID,
		turn.ReplyAuthor, turn.ReplyContent, turn.ResponseContent, nextTurn, turn.CreatedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			// Race with another goroutine — treat as duplicate.
			return 0, nil
		}
		return 0, fmt.Errorf("insert conversation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit conversation: %w", err)
	}
	return nextTurn, nil
}

// CountConversationTurns returns the number of conversation turns for a review event.
func (s *Store) CountConversationTurns(ctx context.Context, reviewEventID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM conversations WHERE review_event_id=?`,
		reviewEventID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count conversation turns: %w", err)
	}
	return n, nil
}

// InsertConversation records a new conversation turn. Returns the auto-generated row ID.
// Prefer BeginConversationTurn for rate-limited atomic inserts.
func (s *Store) InsertConversation(ctx context.Context, turn ConversationTurn) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations(review_event_id, reply_event_id, response_event_id, repo_id, patch_event_id,
			reply_author, reply_content, response_content, turn_number, status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		turn.ReviewEventID, turn.ReplyEventID, turn.ResponseEventID, turn.RepoID, turn.PatchEventID,
		turn.ReplyAuthor, turn.ReplyContent, turn.ResponseContent, turn.TurnNumber, turn.CreatedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("insert conversation: %w", err)
	}
	return res.LastInsertId()
}

// SetConversationResponse updates the response event ID, content, and status after publishing.
func (s *Store) SetConversationResponse(ctx context.Context, replyEventID, responseEventID, responseContent string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET response_event_id=?, response_content=?, status='published' WHERE reply_event_id=?`,
		responseEventID, responseContent, replyEventID,
	)
	if err != nil {
		return fmt.Errorf("set conversation response: %w", err)
	}
	return nil
}

// MarkConversationFailed marks a conversation turn as failed for retry.
func (s *Store) MarkConversationFailed(ctx context.Context, replyEventID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET status='failed' WHERE reply_event_id=?`,
		replyEventID,
	)
	if err != nil {
		return fmt.Errorf("mark conversation failed: %w", err)
	}
	return nil
}

// GetConversationHistory returns all prior conversation turns for a review,
// ordered by turn number ascending.
func (s *Store) GetConversationHistory(ctx context.Context, reviewEventID string) ([]ConversationTurn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, review_event_id, reply_event_id, COALESCE(response_event_id,''), repo_id, patch_event_id,
			reply_author, reply_content, response_content, turn_number, status, created_at
			FROM conversations
			WHERE review_event_id=?
			ORDER BY turn_number ASC`,
		reviewEventID,
	)
	if err != nil {
		return nil, fmt.Errorf("get conversation history: %w", err)
	}
	defer rows.Close()

	var turns []ConversationTurn
	for rows.Next() {
		var t ConversationTurn
		if err := rows.Scan(&t.ID, &t.ReviewEventID, &t.ReplyEventID, &t.ResponseEventID,
			&t.RepoID, &t.PatchEventID, &t.ReplyAuthor, &t.ReplyContent, &t.ResponseContent,
			&t.TurnNumber, &t.Status, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan conversation turn: %w", err)
		}
		turns = append(turns, t)
	}
	return turns, rows.Err()
}

// FindReviewForReply looks up which review event a reply is targeting.
// It searches the review_events table for an event matching the given ID.
// Returns the review event ID, patch event ID, and repo ID, or empty strings if not found.
func (s *Store) FindReviewForReply(ctx context.Context, targetEventID string) (reviewEventID, patchEventID, repoID string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT event_id, patch_event_id, repo_id FROM review_events WHERE event_id=?`,
		targetEventID,
	).Scan(&reviewEventID, &patchEventID, &repoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", "", nil
		}
		return "", "", "", fmt.Errorf("find review for reply: %w", err)
	}
	return reviewEventID, patchEventID, repoID, nil
}

// GetReviewSummary returns the raw content of the review summary event.
func (s *Store) GetReviewSummary(ctx context.Context, reviewEventID string) (string, error) {
	var rawJSON string
	err := s.db.QueryRowContext(ctx,
		`SELECT raw_event_json FROM review_events WHERE event_id=?`,
		reviewEventID,
	).Scan(&rawJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("review event %s not found", reviewEventID)
		}
		return "", fmt.Errorf("get review summary: %w", err)
	}
	return rawJSON, nil
}
