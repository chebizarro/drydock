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

func Open(ctx context.Context, dsn string) (*Store, error) {
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

// Ping verifies database connectivity.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schemaSQL)
	if err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
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
