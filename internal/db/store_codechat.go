package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrCodeChatRateLimited is returned when the user has exceeded their daily chat limit.
var ErrCodeChatRateLimited = errors.New("codechat rate limit exceeded")

// CodeChatTurn represents a single question/answer turn in a codechat conversation.
type CodeChatTurn struct {
	ID           int64
	SenderPubKey string
	EventID      string
	RepoID       string
	Question     string
	Response     string
	Status       string // pending, published, failed
	CreatedAt    int64
}

// BeginCodeChatTurn atomically checks rate limits and inserts a new codechat turn.
// Returns the turn number (1-based) if successful, 0 if duplicate.
// Returns ErrCodeChatRateLimited if the limit is exceeded.
func (s *Store) BeginCodeChatTurn(ctx context.Context, turn CodeChatTurn, maxTurns int) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Check for a duplicate event. Failed attempts may be retried; all other
	// existing states are already in flight, staged, or published.
	var existingStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM codechat_turns WHERE event_id = ?`,
		turn.EventID,
	).Scan(&existingStatus)
	if err == nil {
		if existingStatus != "failed" {
			return 0, nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE codechat_turns SET response = '', status = 'pending' WHERE event_id = ?`,
			turn.EventID,
		); err != nil {
			return 0, fmt.Errorf("reset failed codechat turn: %w", err)
		}
		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM codechat_turns
			WHERE sender_pubkey = ? AND created_at > strftime('%s', 'now', '-1 day')`,
			turn.SenderPubKey,
		).Scan(&count); err != nil {
			return 0, fmt.Errorf("count retry turn: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit retry: %w", err)
		}
		return count, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("check duplicate: %w", err)
	}

	// Count existing turns for this sender today.
	var count int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM codechat_turns 
		 WHERE sender_pubkey = ? AND created_at > strftime('%s', 'now', '-1 day')`,
		turn.SenderPubKey,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count turns: %w", err)
	}

	if count >= maxTurns {
		return 0, ErrCodeChatRateLimited
	}

	// Insert new turn.
	_, err = tx.ExecContext(ctx,
		`INSERT INTO codechat_turns (sender_pubkey, event_id, repo_id, question, status, created_at)
		 VALUES (?, ?, ?, ?, 'pending', ?)`,
		turn.SenderPubKey, turn.EventID, turn.RepoID, turn.Question, turn.CreatedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("insert turn: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}

	return count + 1, nil
}

// SetCodeChatResponse durably stages a generated response before relay publication.
func (s *Store) SetCodeChatResponse(ctx context.Context, eventID, response string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE codechat_turns SET response = ?, status = 'pending' WHERE event_id = ?`,
		response, eventID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("stage codechat response: event %s not found", eventID)
	}
	return nil
}

// MarkCodeChatPublished marks a staged response as published.
func (s *Store) MarkCodeChatPublished(ctx context.Context, eventID string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE codechat_turns SET status = 'published' WHERE event_id = ?`,
		eventID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("mark codechat published: event %s not found", eventID)
	}
	return nil
}

// MarkCodeChatFailed marks a codechat turn as failed.
func (s *Store) MarkCodeChatFailed(ctx context.Context, eventID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE codechat_turns SET status = 'failed' WHERE event_id = ?`,
		eventID,
	)
	return err
}

// GetCodeChatHistory returns the recent chat turns for a user and repo.
func (s *Store) GetCodeChatHistory(ctx context.Context, senderPubKey, repoID string, limit int) ([]CodeChatTurn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, sender_pubkey, event_id, repo_id, question, COALESCE(response, ''), status, created_at
		 FROM codechat_turns
		 WHERE sender_pubkey = ? AND repo_id = ? AND status = 'published'
		 ORDER BY created_at DESC
		 LIMIT ?`,
		senderPubKey, repoID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var turns []CodeChatTurn
	for rows.Next() {
		var t CodeChatTurn
		if err := rows.Scan(&t.ID, &t.SenderPubKey, &t.EventID, &t.RepoID, &t.Question, &t.Response, &t.Status, &t.CreatedAt); err != nil {
			return nil, err
		}
		turns = append(turns, t)
	}

	// Reverse to get chronological order (oldest first).
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}

	return turns, rows.Err()
}

// GetLastCodeChatRepo returns the most recent repo a user chatted about.
func (s *Store) GetLastCodeChatRepo(ctx context.Context, senderPubKey string) (string, error) {
	var repoID string
	err := s.db.QueryRowContext(ctx,
		`SELECT repo_id FROM codechat_turns
		 WHERE sender_pubkey = ? AND repo_id != ''
		 ORDER BY created_at DESC
		 LIMIT 1`,
		senderPubKey,
	).Scan(&repoID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return repoID, err
}
