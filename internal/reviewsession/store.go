package reviewsession

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Clock func() time.Time

type SQLStore struct {
	db  *sql.DB
	now Clock
}

var _ Store = (*SQLStore)(nil)

func NewSQLiteStore(db *sql.DB, clock Clock) (*SQLStore, error) {
	if db == nil {
		return nil, fmt.Errorf("review session: database is required")
	}
	if clock == nil {
		clock = time.Now
	}
	return &SQLStore{db: db, now: clock}, nil
}

func (s *SQLStore) Create(ctx context.Context, p CreateParams) (Reservation, error) {
	if p.ChatID == "" {
		var err error
		p.ChatID, err = NewChatID()
		if err != nil {
			return Reservation{}, err
		}
	}
	decodedChatID, decodeErr := hex.DecodeString(p.ChatID)
	if decodeErr != nil || len(decodedChatID) != 16 || strings.ToLower(p.ChatID) != p.ChatID || p.Owner.Validate() != nil || !validMode(p.Mode) {
		return Reservation{}, fmt.Errorf("review session: invalid create parameters")
	}
	if p.Snapshot.ID == "" || p.Snapshot.Kind == "" || p.Snapshot.StoragePath == "" ||
		p.Snapshot.ManifestHash == "" || p.Snapshot.DiffHash == "" || p.BundleHash == "" ||
		!json.Valid(p.TargetEnvelope) || strings.TrimSpace(p.RequestID) == "" {
		return Reservation{}, fmt.Errorf("review session: incomplete create parameters")
	}
	now := s.now().UTC()
	if p.ExpiresAt.IsZero() {
		p.ExpiresAt = p.Snapshot.ExpiresAt
	}
	if !p.ExpiresAt.After(now) {
		return Reservation{}, ErrExpired
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `INSERT INTO review_snapshots
		(snapshot_id, kind, storage_path, manifest_sha256, diff_sha256, ref_count, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?)
		ON CONFLICT(snapshot_id) DO NOTHING`,
		p.Snapshot.ID, p.Snapshot.Kind, p.Snapshot.StoragePath, p.Snapshot.ManifestHash,
		p.Snapshot.DiffHash, p.Snapshot.ExpiresAt.Unix(), now.Unix(), now.Unix())
	if err != nil {
		return Reservation{}, fmt.Errorf("review session: insert snapshot: %w", err)
	}
	var kind, storage, manifest, diff string
	if err := tx.QueryRowContext(ctx, `SELECT kind, storage_path, manifest_sha256, diff_sha256
		FROM review_snapshots WHERE snapshot_id=?`, p.Snapshot.ID).Scan(&kind, &storage, &manifest, &diff); err != nil {
		return Reservation{}, err
	}
	if kind != p.Snapshot.Kind || storage != p.Snapshot.StoragePath || manifest != p.Snapshot.ManifestHash || diff != p.Snapshot.DiffHash {
		return Reservation{}, fmt.Errorf("review session: snapshot binding mismatch")
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO review_sessions
		(chat_id, owner_kind, owner_id, mode, state, snapshot_id, target_envelope_json,
		 bundle_sha256, version, active_request_id, lease_id, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'initializing', ?, ?, ?, 0, ?, ?, ?, ?, ?)`,
		p.ChatID, p.Owner.Kind, p.Owner.ID, p.Mode, p.Snapshot.ID, string(p.TargetEnvelope),
		p.BundleHash, p.RequestID, p.LeaseID, p.ExpiresAt.Unix(), now.Unix(), now.Unix())
	if err != nil {
		return Reservation{}, fmt.Errorf("review session: insert session: %w", err)
	}
	for i, artifact := range p.Artifacts {
		if artifact.Ordinal != 0 && artifact.Ordinal != i {
			return Reservation{}, fmt.Errorf("review session: non-canonical artifact ordinal")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_session_artifacts
			(chat_id, ordinal, kind, path, start_line, end_line, content_sha256, mandatory)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			p.ChatID, i, artifact.Kind, artifact.Path, artifact.StartLine, artifact.EndLine,
			artifact.Hash, boolInt(artifact.Mandatory)); err != nil {
			return Reservation{}, fmt.Errorf("review session: insert artifact %d: %w", i, err)
		}
	}
	requestHash := turnRequestHash(0, p.RequestText)
	_, err = tx.ExecContext(ctx, `INSERT INTO review_session_turns
		(chat_id, turn_no, request_id, request_sha256, request_text, expected_version, status, created_at)
		VALUES (?, 0, ?, ?, ?, 0, 'reserved', ?)`,
		p.ChatID, p.RequestID, requestHash, p.RequestText, now.Unix())
	if err != nil {
		return Reservation{}, fmt.Errorf("review session: insert initial turn: %w", err)
	}
	if p.RequestText != "" {
		if err := insertMessage(ctx, tx, p.ChatID, 0, 0, Message{Role: "user", Content: p.RequestText}, now); err != nil {
			return Reservation{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_snapshots
		SET ref_count=ref_count+1, expires_at=MAX(expires_at, ?), updated_at=? WHERE snapshot_id=?`,
		p.ExpiresAt.Unix(), now.Unix(), p.Snapshot.ID); err != nil {
		return Reservation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Reservation{}, err
	}
	loaded, err := s.LoadForContinuation(ctx, p.ChatID)
	if err != nil {
		return Reservation{}, err
	}
	return Reservation{Session: loaded.Session, Turn: loaded.Turns[0]}, nil
}

func (s *SQLStore) ReserveTurn(ctx context.Context, p ReserveTurnParams) (Reservation, error) {
	if p.Owner.Validate() != nil || strings.TrimSpace(p.ChatID) == "" || strings.TrimSpace(p.RequestID) == "" ||
		strings.TrimSpace(p.RequestText) == "" || p.ExpectedVersion < 0 {
		return Reservation{}, fmt.Errorf("review session: invalid turn reservation")
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, err
	}
	defer tx.Rollback()

	session, err := loadSession(ctx, tx, p.ChatID)
	if err != nil {
		return Reservation{}, err
	}
	if session.Owner != p.Owner {
		return Reservation{}, ErrOwnerMismatch
	}
	hash := turnRequestHash(p.ExpectedVersion, p.RequestText)
	var existing Turn
	var resultText string
	err = tx.QueryRowContext(ctx, `SELECT turn_no, request_sha256, request_text, expected_version,
		status, result_json, error_text, created_at, completed_at
		FROM review_session_turns WHERE chat_id=? AND request_id=?`, p.ChatID, p.RequestID).Scan(
		&existing.TurnNo, &existing.RequestHash, &existing.RequestText, &existing.ExpectedVersion,
		&existing.Status, &resultText, &existing.Error, unixScanner(&existing.CreatedAt), unixScanner(&existing.CompletedAt))
	if err == nil {
		if existing.RequestHash != hash {
			return Reservation{}, ErrIdempotencyConflict
		}
		existing.RequestID = p.RequestID
		existing.Result = json.RawMessage(resultText)
		if existing.Status == TurnReserved {
			return Reservation{}, ErrRequestInProgress
		}
		if err := tx.Commit(); err != nil {
			return Reservation{}, err
		}
		session.Version = max(session.Version, existing.TurnNo)
		return Reservation{Session: session, Turn: existing, Replay: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Reservation{}, err
	}
	if session.State == StateExpired || !now.Before(session.ExpiresAt) {
		return Reservation{}, ErrExpired
	}
	if session.State == StateBroken {
		return Reservation{}, ErrBroken
	}
	if session.State != StateActive {
		return Reservation{}, ErrActiveTurn
	}
	if session.ActiveRequest != "" {
		return Reservation{}, ErrActiveTurn
	}
	if session.Version != p.ExpectedVersion {
		return Reservation{}, fmt.Errorf("%w: current=%d expected=%d", ErrVersionConflict, session.Version, p.ExpectedVersion)
	}
	turnNo := p.ExpectedVersion + 1
	if p.ExpiresAt.IsZero() {
		p.ExpiresAt = session.ExpiresAt
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO review_session_turns
		(chat_id, turn_no, request_id, request_sha256, request_text, expected_version, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'reserved', ?)`,
		p.ChatID, turnNo, p.RequestID, hash, p.RequestText, p.ExpectedVersion, now.Unix())
	if err != nil {
		return Reservation{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE review_sessions
		SET version=?, active_request_id=?, lease_id=?, expires_at=MAX(expires_at, ?), updated_at=?
		WHERE chat_id=? AND version=? AND state='active' AND active_request_id=''`,
		turnNo, p.RequestID, p.LeaseID, p.ExpiresAt.Unix(), now.Unix(),
		p.ChatID, p.ExpectedVersion)
	if err != nil {
		return Reservation{}, err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return Reservation{}, ErrVersionConflict
	}
	if err := insertMessage(ctx, tx, p.ChatID, turnNo, 0, Message{
		Role: "user", Content: p.RequestText,
	}, now); err != nil {
		return Reservation{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_snapshots
		SET expires_at=MAX(expires_at, ?), updated_at=? WHERE snapshot_id=?`,
		p.ExpiresAt.Unix(), now.Unix(), session.Snapshot.ID); err != nil {
		return Reservation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Reservation{}, err
	}
	session.Version = turnNo
	session.ActiveRequest = p.RequestID
	session.LeaseID = p.LeaseID
	return Reservation{Session: session, Turn: Turn{
		TurnNo: turnNo, RequestID: p.RequestID, RequestHash: hash, RequestText: p.RequestText,
		ExpectedVersion: p.ExpectedVersion, Status: TurnReserved, CreatedAt: now,
	}}, nil
}

func (s *SQLStore) AppendMessages(ctx context.Context, chatID, requestID string, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var turnNo int
	var status TurnStatus
	if err := tx.QueryRowContext(ctx, `SELECT turn_no, status FROM review_session_turns
		WHERE chat_id=? AND request_id=?`, chatID, requestID).Scan(&turnNo, &status); err != nil {
		return mapNotFound(err)
	}
	if status != TurnReserved {
		return ErrInvalidTranscript
	}
	var active string
	if err := tx.QueryRowContext(ctx, `SELECT active_request_id FROM review_sessions WHERE chat_id=?`, chatID).Scan(&active); err != nil {
		return mapNotFound(err)
	}
	if active != requestID {
		return ErrInvalidTranscript
	}
	var next int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq)+1, 0) FROM review_session_messages
		WHERE chat_id=? AND turn_no=?`, chatID, turnNo).Scan(&next); err != nil {
		return err
	}
	now := s.now().UTC()
	for i, message := range messages {
		if err := validateMessage(message); err != nil {
			return err
		}
		if err := insertMessage(ctx, tx, chatID, turnNo, next+i, message, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLStore) CompleteTurn(ctx context.Context, chatID, requestID string, result json.RawMessage) error {
	if len(result) == 0 || !json.Valid(result) {
		return fmt.Errorf("review session: turn result must be valid JSON")
	}
	return s.finishTurn(ctx, chatID, requestID, TurnComplete, result, "")
}

func (s *SQLStore) FailTurn(ctx context.Context, chatID, requestID string, turnErr error) error {
	message := "turn failed"
	if turnErr != nil {
		message = turnErr.Error()
	}
	return s.finishTurn(ctx, chatID, requestID, TurnFailed, nil, message)
}

func (s *SQLStore) finishTurn(ctx context.Context, chatID, requestID string, status TurnStatus, result json.RawMessage, errorText string) error {
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var turnNo int
	var current TurnStatus
	if err := tx.QueryRowContext(ctx, `SELECT turn_no, status FROM review_session_turns
		WHERE chat_id=? AND request_id=?`, chatID, requestID).Scan(&turnNo, &current); err != nil {
		return mapNotFound(err)
	}
	if current != TurnReserved {
		if current == status {
			return tx.Commit()
		}
		return ErrInvalidTranscript
	}
	update, err := tx.ExecContext(ctx, `UPDATE review_session_turns SET status=?, result_json=?, error_text=?, completed_at=?
		WHERE chat_id=? AND request_id=? AND status='reserved'`,
		status, string(result), errorText, now.Unix(), chatID, requestID)
	if err != nil {
		return err
	}
	affected, _ := update.RowsAffected()
	if affected != 1 {
		return ErrInvalidTranscript
	}
	nextState := "active"
	if turnNo == 0 && status == TurnFailed {
		nextState = "broken"
	}
	update, err = tx.ExecContext(ctx, `UPDATE review_sessions SET state=?, active_request_id='', updated_at=?
		WHERE chat_id=? AND active_request_id=? AND version=?`,
		nextState, now.Unix(), chatID, requestID, turnNo)
	if err != nil {
		return err
	}
	affected, _ = update.RowsAffected()
	if affected != 1 {
		return ErrInvalidTranscript
	}
	if turnNo == 0 && status == TurnFailed {
		var snapshotID string
		if err := tx.QueryRowContext(ctx, `SELECT snapshot_id FROM review_sessions WHERE chat_id=?`, chatID).Scan(&snapshotID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_snapshots SET ref_count=ref_count-1, updated_at=?
			WHERE snapshot_id=? AND ref_count>0`, now.Unix(), snapshotID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLStore) LoadForContinuation(ctx context.Context, chatID string) (Loaded, error) {
	session, err := loadSession(ctx, s.db, chatID)
	if err != nil {
		return Loaded{}, err
	}
	loaded := Loaded{Session: session}
	rows, err := s.db.QueryContext(ctx, `SELECT ordinal, kind, path, start_line, end_line, content_sha256, mandatory
		FROM review_session_artifacts WHERE chat_id=? ORDER BY ordinal`, chatID)
	if err != nil {
		return Loaded{}, err
	}
	for rows.Next() {
		var artifact Artifact
		var mandatory int
		if err := rows.Scan(&artifact.Ordinal, &artifact.Kind, &artifact.Path, &artifact.StartLine,
			&artifact.EndLine, &artifact.Hash, &mandatory); err != nil {
			rows.Close()
			return Loaded{}, err
		}
		artifact.Mandatory = mandatory != 0
		loaded.Artifacts = append(loaded.Artifacts, artifact)
	}
	if err := rows.Close(); err != nil {
		return Loaded{}, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT turn_no, request_id, request_sha256, request_text,
		expected_version, status, result_json, error_text, created_at, completed_at
		FROM review_session_turns WHERE chat_id=? ORDER BY turn_no`, chatID)
	if err != nil {
		return Loaded{}, err
	}
	for rows.Next() {
		var turn Turn
		var resultText string
		var created, completed int64
		if err := rows.Scan(&turn.TurnNo, &turn.RequestID, &turn.RequestHash, &turn.RequestText,
			&turn.ExpectedVersion, &turn.Status, &resultText, &turn.Error, &created, &completed); err != nil {
			rows.Close()
			return Loaded{}, err
		}
		turn.Result = json.RawMessage(resultText)
		turn.CreatedAt = time.Unix(created, 0).UTC()
		if completed > 0 {
			turn.CompletedAt = time.Unix(completed, 0).UTC()
		}
		loaded.Turns = append(loaded.Turns, turn)
	}
	if err := rows.Close(); err != nil {
		return Loaded{}, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT turn_no, seq, role, name, tool_call_id, content,
		tool_calls_json, prompt_tokens, completion_tokens
		FROM review_session_messages WHERE chat_id=? ORDER BY turn_no, seq`, chatID)
	if err != nil {
		return Loaded{}, err
	}
	for rows.Next() {
		var message Message
		var toolCalls string
		if err := rows.Scan(&message.TurnNo, &message.Seq, &message.Role, &message.Name,
			&message.ToolCallID, &message.Content, &toolCalls, &message.PromptTokens,
			&message.CompletionTokens); err != nil {
			rows.Close()
			return Loaded{}, err
		}
		if err := json.Unmarshal([]byte(toolCalls), &message.ToolCalls); err != nil {
			rows.Close()
			return Loaded{}, fmt.Errorf("%w: decode tool calls: %v", ErrInvalidTranscript, err)
		}
		loaded.Messages = append(loaded.Messages, message)
	}
	if err := rows.Close(); err != nil {
		return Loaded{}, err
	}
	return loaded, nil
}

func (s *SQLStore) Expire(ctx context.Context, chatID string) (string, error) {
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var state State
	var snapshotID, leaseID string
	if err := tx.QueryRowContext(ctx, `SELECT state, snapshot_id, lease_id FROM review_sessions WHERE chat_id=?`,
		chatID).Scan(&state, &snapshotID, &leaseID); err != nil {
		return "", mapNotFound(err)
	}
	if state == StateExpired {
		return leaseID, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_sessions SET state='expired', active_request_id='', updated_at=?
		WHERE chat_id=?`, now.Unix(), chatID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_snapshots SET ref_count=ref_count-1, updated_at=?
		WHERE snapshot_id=? AND ref_count>0`, now.Unix(), snapshotID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return leaseID, nil
}

func (s *SQLStore) MarkBroken(ctx context.Context, chatID string, cause error) error {
	now := s.now().UTC()
	message := "snapshot binding failed"
	if cause != nil {
		message = cause.Error()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE review_sessions SET state='broken', active_request_id='', updated_at=?
		WHERE chat_id=? AND state!='expired'`, now.Unix(), chatID)
	if err != nil {
		return fmt.Errorf("review session: mark broken (%s): %w", message, err)
	}
	return nil
}

func (s *SQLStore) ListActive(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chat_id FROM review_sessions WHERE state='active' ORDER BY chat_id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	sessions := make([]Session, 0, len(ids))
	for _, id := range ids {
		session, err := loadSession(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

func (s *SQLStore) BindLease(ctx context.Context, chatID, leaseID string) error {
	if strings.TrimSpace(leaseID) == "" {
		return fmt.Errorf("review session: lease ID is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE review_sessions SET lease_id=?, updated_at=?
		WHERE chat_id=? AND state='active'`, leaseID, s.now().UTC().Unix(), chatID)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return ErrNotFound
	}
	return nil
}

type sqlQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadSession(ctx context.Context, q sqlQuerier, chatID string) (Session, error) {
	var session Session
	var target string
	var snapshotExpires, expires, created, updated int64
	err := q.QueryRowContext(ctx, `SELECT s.chat_id, s.owner_kind, s.owner_id, s.mode, s.state,
		s.snapshot_id, sn.kind, sn.storage_path, sn.manifest_sha256, sn.diff_sha256, sn.expires_at,
		s.target_envelope_json, s.bundle_sha256, s.version, s.active_request_id, s.lease_id,
		s.expires_at, s.created_at, s.updated_at
		FROM review_sessions s JOIN review_snapshots sn ON sn.snapshot_id=s.snapshot_id
		WHERE s.chat_id=?`, chatID).Scan(
		&session.ChatID, &session.Owner.Kind, &session.Owner.ID, &session.Mode, &session.State,
		&session.Snapshot.ID, &session.Snapshot.Kind, &session.Snapshot.StoragePath,
		&session.Snapshot.ManifestHash, &session.Snapshot.DiffHash, &snapshotExpires,
		&target, &session.BundleHash, &session.Version, &session.ActiveRequest, &session.LeaseID,
		&expires, &created, &updated)
	if err != nil {
		return Session{}, mapNotFound(err)
	}
	if !json.Valid([]byte(target)) {
		return Session{}, fmt.Errorf("review session: invalid stored target envelope")
	}
	session.TargetEnvelope = json.RawMessage(target)
	session.Snapshot.ExpiresAt = time.Unix(snapshotExpires, 0).UTC()
	session.ExpiresAt = time.Unix(expires, 0).UTC()
	session.CreatedAt = time.Unix(created, 0).UTC()
	session.UpdatedAt = time.Unix(updated, 0).UTC()
	return session, nil
}

func insertMessage(ctx context.Context, tx *sql.Tx, chatID string, turnNo, seq int, message Message, now time.Time) error {
	if err := validateMessage(message); err != nil {
		return err
	}
	toolCalls, err := json.Marshal(message.ToolCalls)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO review_session_messages
		(chat_id, turn_no, seq, role, name, tool_call_id, content, tool_calls_json,
		 prompt_tokens, completion_tokens, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		chatID, turnNo, seq, message.Role, message.Name, message.ToolCallID, message.Content,
		string(toolCalls), message.PromptTokens, message.CompletionTokens, now.Unix())
	if err != nil {
		return fmt.Errorf("review session: insert message: %w", err)
	}
	return nil
}

func validateMessage(message Message) error {
	switch message.Role {
	case "system", "user", "assistant", "tool":
	default:
		return fmt.Errorf("%w: unsupported role %q", ErrInvalidTranscript, message.Role)
	}
	if message.PromptTokens < 0 || message.CompletionTokens < 0 {
		return fmt.Errorf("%w: negative token count", ErrInvalidTranscript)
	}
	if message.Role == "tool" && strings.TrimSpace(message.ToolCallID) == "" {
		return fmt.Errorf("%w: tool message missing tool_call_id", ErrInvalidTranscript)
	}
	return nil
}

func turnRequestHash(expected int, text string) string {
	payload, _ := json.Marshal(struct {
		Expected int    `json:"expected_version"`
		Text     string `json:"message"`
	}{expected, text})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func mapNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// unixScanner exists only for compact duplicate-request scans.
func unixScanner(target *time.Time) sql.Scanner {
	return &timeScanner{target: target}
}

type timeScanner struct{ target *time.Time }

func (s *timeScanner) Scan(src any) error {
	var value int64
	switch v := src.(type) {
	case int64:
		value = v
	case int:
		value = int64(v)
	case nil:
		return nil
	default:
		return fmt.Errorf("review session: unexpected timestamp type %T", src)
	}
	if value > 0 {
		*s.target = time.Unix(value, 0).UTC()
	}
	return nil
}
