package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ZapReceiptRecord is a validated NIP-57 zap receipt normalized for authorization.
type ZapReceiptRecord struct {
	ID            int64
	EventID       string
	PatchEventID  string
	PayerPubkey   string
	ReceiptAuthor string
	AmountMSat    int64
	CreatedAt     int64
}

// InsertZapReceiptAndClaimBlockedReviews stores a receipt idempotently and
// atomically claims reviews that were permanently blocked waiting for payment.
func (s *Store) InsertZapReceiptAndClaimBlockedReviews(ctx context.Context, receipt ZapReceiptRecord) (bool, []ReviewTask, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, fmt.Errorf("begin zap receipt transaction: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `INSERT INTO zap_receipts (
		event_id, patch_event_id, payer_pubkey, receipt_author, amount_msat, created_at, seen_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(event_id) DO NOTHING`,
		receipt.EventID, receipt.PatchEventID, receipt.PayerPubkey, receipt.ReceiptAuthor,
		receipt.AmountMSat, receipt.CreatedAt, time.Now().Unix())
	if err != nil {
		return false, nil, fmt.Errorf("insert zap receipt: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, nil, fmt.Errorf("zap receipt rows affected: %w", err)
	}
	if inserted == 0 {
		if err := tx.Commit(); err != nil {
			return false, nil, fmt.Errorf("commit duplicate zap receipt: %w", err)
		}
		return false, nil, nil
	}

	rows, err := tx.QueryContext(ctx, `SELECT patch_event_id, repo_id, force
		FROM review_log
		WHERE patch_event_id=? AND status='failed' AND failure_reason LIKE 'payment_blocked:%'`,
		receipt.PatchEventID)
	if err != nil {
		return false, nil, fmt.Errorf("query payment-blocked reviews: %w", err)
	}
	var candidates []ReviewTask
	for rows.Next() {
		var task ReviewTask
		if err := rows.Scan(&task.PatchEventID, &task.RepoID, &task.Force); err != nil {
			rows.Close()
			return false, nil, fmt.Errorf("scan payment-blocked review: %w", err)
		}
		candidates = append(candidates, task)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, nil, fmt.Errorf("iterate payment-blocked reviews: %w", err)
	}
	if err := rows.Close(); err != nil {
		return false, nil, fmt.Errorf("close payment-blocked reviews: %w", err)
	}

	now := time.Now().Unix()
	tasks := make([]ReviewTask, 0, len(candidates))
	for _, task := range candidates {
		result, err := tx.ExecContext(ctx, `UPDATE review_log
			SET status='reviewing', failure_reason=NULL, updated_at=?
			WHERE patch_event_id=? AND repo_id=? AND status='failed'
			  AND failure_reason LIKE 'payment_blocked:%'`,
			now, task.PatchEventID, task.RepoID)
		if err != nil {
			return false, nil, fmt.Errorf("claim payment-blocked review: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, nil, fmt.Errorf("claimed review rows affected: %w", err)
		}
		if affected == 1 {
			tasks = append(tasks, task)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, nil, fmt.Errorf("commit zap receipt: %w", err)
	}
	return true, tasks, nil
}

// FindZapReceiptAtLeast returns the oldest receipt for a patch that covers the
// requested millisatoshi price.
func (s *Store) FindZapReceiptAtLeast(ctx context.Context, patchEventID string, minimumMSat int64) (ZapReceiptRecord, bool, int64, error) {
	var cursor int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM zap_receipts WHERE patch_event_id=?`, patchEventID).Scan(&cursor); err != nil {
		return ZapReceiptRecord{}, false, 0, fmt.Errorf("read zap receipt cursor: %w", err)
	}

	var receipt ZapReceiptRecord
	err := s.db.QueryRowContext(ctx, `SELECT id, event_id, patch_event_id, payer_pubkey,
		receipt_author, amount_msat, created_at
		FROM zap_receipts
		WHERE patch_event_id=? AND amount_msat>=?
		ORDER BY seen_at ASC, event_id ASC
		LIMIT 1`, patchEventID, minimumMSat).Scan(
		&receipt.ID, &receipt.EventID, &receipt.PatchEventID, &receipt.PayerPubkey,
		&receipt.ReceiptAuthor, &receipt.AmountMSat, &receipt.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ZapReceiptRecord{}, false, cursor, nil
		}
		return ZapReceiptRecord{}, false, cursor, fmt.Errorf("find zap receipt: %w", err)
	}
	return receipt, true, cursor, nil
}

// MarkReviewPaymentBlocked records a permanent payment denial only when no
// receipt arrived after the authorization attempt observed its cursor.
// advanced is true when the caller must retry authorization instead.
func (s *Store) MarkReviewPaymentBlocked(ctx context.Context, patchEventID, repoID, reason string, observedCursor int64) (advanced bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin payment block transaction: %w", err)
	}
	defer tx.Rollback()

	var currentCursor int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM zap_receipts WHERE patch_event_id=?`, patchEventID).Scan(&currentCursor); err != nil {
		return false, fmt.Errorf("read payment block zap cursor: %w", err)
	}
	if currentCursor > observedCursor {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit advanced zap cursor: %w", err)
		}
		return true, nil
	}

	result, err := tx.ExecContext(ctx, `UPDATE review_log
		SET status='failed', failure_reason=?, updated_at=?
		WHERE patch_event_id=? AND repo_id=? AND status='reviewing'`,
		"payment_blocked:"+reason, time.Now().Unix(), patchEventID, repoID)
	if err != nil {
		return false, fmt.Errorf("mark review payment blocked: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("payment block rows affected: %w", err)
	}
	if affected != 1 {
		return false, fmt.Errorf("mark review payment blocked: expected reviewing row, changed %d", affected)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit review payment block: %w", err)
	}
	return false, nil
}
