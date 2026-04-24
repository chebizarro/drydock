package db

import (
	"context"
	"database/sql"
	"time"
)

// ReviewPaymentRecord represents a review payment authorization record.
type ReviewPaymentRecord struct {
	PatchEventID     string
	RepoID           string
	AuthorPubkey     string
	Status           string // pending, authorized
	AccessKind       string // free_tier, subscription, cashu_review, cashu_subscription
	RequestedMode    string // review, subscription
	TokenHash        string
	MintURL          string
	TokenAmountSats  int64
	InvoiceID        string
	InvoiceRequest   string
	InvoiceExpiresAt int64
	CreatedAt        int64
	UpdatedAt        int64
}

// SubscriptionRecord represents an active payment subscription.
type SubscriptionRecord struct {
	AuthorPubkey       string
	RepoID             string
	SourcePatchEventID string
	SourceTokenHash    string
	PaidAmountSats     int64
	ExpiresAt          int64
	CreatedAt          int64
	UpdatedAt          int64
}

// GetReviewPayment retrieves a review payment record by patch event ID.
func (s *Store) GetReviewPayment(ctx context.Context, patchEventID string) (ReviewPaymentRecord, error) {
	var rec ReviewPaymentRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT patch_event_id, repo_id, author_pubkey, status, access_kind, requested_mode,
		       token_hash, mint_url, token_amount_sats, invoice_id, invoice_request,
		       invoice_expires_at, created_at, updated_at
		FROM review_payments
		WHERE patch_event_id = ?
	`, patchEventID).Scan(
		&rec.PatchEventID, &rec.RepoID, &rec.AuthorPubkey, &rec.Status, &rec.AccessKind,
		&rec.RequestedMode, &rec.TokenHash, &rec.MintURL, &rec.TokenAmountSats,
		&rec.InvoiceID, &rec.InvoiceRequest, &rec.InvoiceExpiresAt, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		return ReviewPaymentRecord{}, err
	}
	return rec, nil
}

// UpsertPendingReviewPayment inserts or updates a pending review payment record.
func (s *Store) UpsertPendingReviewPayment(ctx context.Context, rec ReviewPaymentRecord) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO review_payments (
			patch_event_id, repo_id, author_pubkey, status, access_kind, requested_mode,
			token_hash, mint_url, token_amount_sats, invoice_id, invoice_request,
			invoice_expires_at, created_at, updated_at
		) VALUES (?, ?, ?, 'pending', '', ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(patch_event_id) DO UPDATE SET
			token_hash = excluded.token_hash,
			mint_url = excluded.mint_url,
			token_amount_sats = excluded.token_amount_sats,
			invoice_id = excluded.invoice_id,
			invoice_request = excluded.invoice_request,
			invoice_expires_at = excluded.invoice_expires_at,
			updated_at = excluded.updated_at
	`, rec.PatchEventID, rec.RepoID, rec.AuthorPubkey, rec.RequestedMode,
		rec.TokenHash, rec.MintURL, rec.TokenAmountSats, rec.InvoiceID, rec.InvoiceRequest,
		rec.InvoiceExpiresAt, now, now)
	return err
}

// DeletePendingReviewPayment removes a pending review payment record.
func (s *Store) DeletePendingReviewPayment(ctx context.Context, patchEventID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM review_payments WHERE patch_event_id = ? AND status = 'pending'
	`, patchEventID)
	return err
}

// MarkReviewPaymentAuthorized marks a review payment as authorized.
func (s *Store) MarkReviewPaymentAuthorized(ctx context.Context, patchEventID, accessKind string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		UPDATE review_payments
		SET status = 'authorized', access_kind = ?, updated_at = ?
		WHERE patch_event_id = ?
	`, accessKind, now, patchEventID)
	return err
}

// GetActiveSubscription returns an active subscription for the author+repo if one exists.
func (s *Store) GetActiveSubscription(ctx context.Context, authorPubkey, repoID string, now int64) (SubscriptionRecord, bool, error) {
	var rec SubscriptionRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT author_pubkey, repo_id, source_patch_event_id, source_token_hash,
		       paid_amount_sats, expires_at, created_at, updated_at
		FROM payment_subscriptions
		WHERE author_pubkey = ? AND repo_id = ? AND expires_at > ?
	`, authorPubkey, repoID, now).Scan(
		&rec.AuthorPubkey, &rec.RepoID, &rec.SourcePatchEventID, &rec.SourceTokenHash,
		&rec.PaidAmountSats, &rec.ExpiresAt, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return SubscriptionRecord{}, false, nil
	}
	if err != nil {
		return SubscriptionRecord{}, false, err
	}
	return rec, true, nil
}

// UpsertSubscription creates or extends a subscription. Extension starts from
// max(now, current_expires_at) so renewals stack.
func (s *Store) UpsertSubscription(ctx context.Context, authorPubkey, repoID, sourcePatchEventID, sourceTokenHash string, paidAmountSats int64, extendDays int) error {
	now := time.Now().Unix()
	extendSecs := int64(extendDays * 24 * 60 * 60)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO payment_subscriptions (
			author_pubkey, repo_id, source_patch_event_id, source_token_hash,
			paid_amount_sats, expires_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(author_pubkey, repo_id) DO UPDATE SET
			source_patch_event_id = excluded.source_patch_event_id,
			source_token_hash = excluded.source_token_hash,
			paid_amount_sats = excluded.paid_amount_sats,
			expires_at = MAX(expires_at, ?) + ?,
			updated_at = ?
	`, authorPubkey, repoID, sourcePatchEventID, sourceTokenHash,
		paidAmountSats, now+extendSecs, now, now,
		now, extendSecs, now)
	return err
}

// TryAuthorizeFreeReview attempts to authorize a review using free-tier quota.
// Returns true if authorized, false if quota exhausted.
func (s *Store) TryAuthorizeFreeReview(ctx context.Context, patchEventID, repoID, authorPubkey string, dailyLimit int, usageDay string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// Check if already authorized
	var existingStatus string
	err = tx.QueryRowContext(ctx, `
		SELECT status FROM review_payments WHERE patch_event_id = ?
	`, patchEventID).Scan(&existingStatus)
	if err == nil && existingStatus == "authorized" {
		return true, nil
	} else if err != nil && err != sql.ErrNoRows {
		return false, err
	}

	// Check current usage
	var usedCount int
	err = tx.QueryRowContext(ctx, `
		SELECT used_count FROM free_review_usage
		WHERE author_pubkey = ? AND repo_id = ? AND usage_day = ?
	`, authorPubkey, repoID, usageDay).Scan(&usedCount)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}

	if usedCount >= dailyLimit {
		return false, nil
	}

	now := time.Now().Unix()

	// Increment usage
	_, err = tx.ExecContext(ctx, `
		INSERT INTO free_review_usage (author_pubkey, repo_id, usage_day, used_count, updated_at)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT(author_pubkey, repo_id, usage_day) DO UPDATE SET
			used_count = used_count + 1,
			updated_at = ?
	`, authorPubkey, repoID, usageDay, now, now)
	if err != nil {
		return false, err
	}

	// Insert authorized payment record
	_, err = tx.ExecContext(ctx, `
		INSERT INTO review_payments (
			patch_event_id, repo_id, author_pubkey, status, access_kind, requested_mode,
			token_hash, mint_url, token_amount_sats, invoice_id, invoice_request,
			invoice_expires_at, created_at, updated_at
		) VALUES (?, ?, ?, 'authorized', 'free_tier', 'review', NULL, '', 0, '', '', 0, ?, ?)
		ON CONFLICT(patch_event_id) DO UPDATE SET
			status = 'authorized',
			access_kind = 'free_tier',
			updated_at = ?
	`, patchEventID, repoID, authorPubkey, now, now, now)
	if err != nil {
		return false, err
	}

	return tx.Commit() == nil, nil
}

// AuthorizeReviewFromSubscription creates an authorized payment record
// for a review covered by an existing subscription.
func (s *Store) AuthorizeReviewFromSubscription(ctx context.Context, patchEventID, repoID, authorPubkey string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO review_payments (
			patch_event_id, repo_id, author_pubkey, status, access_kind, requested_mode,
			token_hash, mint_url, token_amount_sats, invoice_id, invoice_request,
			invoice_expires_at, created_at, updated_at
		) VALUES (?, ?, ?, 'authorized', 'subscription', 'review', NULL, '', 0, '', '', 0, ?, ?)
		ON CONFLICT(patch_event_id) DO NOTHING
	`, patchEventID, repoID, authorPubkey, now, now)
	return err
}

// IsTokenHashUsed checks if a token hash has already been used for payment.
func (s *Store) IsTokenHashUsed(ctx context.Context, tokenHash string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM review_payments WHERE token_hash = ? AND status = 'authorized'
		UNION ALL
		SELECT 1 FROM payment_subscriptions WHERE source_token_hash = ?
		LIMIT 1
	`, tokenHash, tokenHash).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
