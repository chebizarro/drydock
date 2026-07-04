package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IDEGatewayFix is a durable suggested-fix patch for IDE review/apply-fix flows.
type IDEGatewayFix struct {
	FixID        string
	SessionID    string
	AuthorPubKey string
	File         string
	Diff         string
	CreatedAt    int64
}

// UpsertIDEGatewayFix stores or replaces a suggested fix for an IDE session.
func (s *Store) UpsertIDEGatewayFix(ctx context.Context, fix IDEGatewayFix) error {
	fix.FixID = strings.TrimSpace(fix.FixID)
	fix.SessionID = strings.TrimSpace(fix.SessionID)
	if fix.FixID == "" {
		return fmt.Errorf("ide gateway fix_id is required")
	}
	if fix.SessionID == "" {
		return fmt.Errorf("ide gateway session_id is required")
	}
	if fix.CreatedAt == 0 {
		fix.CreatedAt = time.Now().Unix()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ide_gateway_fixes(fix_id, session_id, author_pubkey, file, diff, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(fix_id, session_id) DO UPDATE SET
		  author_pubkey=excluded.author_pubkey,
		  file=excluded.file,
		  diff=excluded.diff,
		  created_at=excluded.created_at`,
		fix.FixID, fix.SessionID, fix.AuthorPubKey, fix.File, fix.Diff, fix.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert ide gateway fix: %w", err)
	}
	return nil
}

// GetIDEGatewayFix fetches a suggested fix by fix/session key.
func (s *Store) GetIDEGatewayFix(ctx context.Context, fixID, sessionID string) (IDEGatewayFix, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT fix_id, session_id, author_pubkey, file, diff, created_at
		FROM ide_gateway_fixes
		WHERE fix_id=? AND session_id=?`, strings.TrimSpace(fixID), strings.TrimSpace(sessionID))

	var fix IDEGatewayFix
	if err := row.Scan(&fix.FixID, &fix.SessionID, &fix.AuthorPubKey, &fix.File, &fix.Diff, &fix.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return IDEGatewayFix{}, false, nil
		}
		return IDEGatewayFix{}, false, fmt.Errorf("get ide gateway fix: %w", err)
	}
	return fix, true, nil
}

// DeleteExpiredIDEGatewayFixes removes suggested fixes created before cutoffUnix.
func (s *Store) DeleteExpiredIDEGatewayFixes(ctx context.Context, cutoffUnix int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ide_gateway_fixes WHERE created_at < ?`, cutoffUnix)
	if err != nil {
		return fmt.Errorf("delete expired ide gateway fixes: %w", err)
	}
	return nil
}
