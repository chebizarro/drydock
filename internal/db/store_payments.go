package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrTokenHashAlreadyReserved = errors.New("payment token hash already reserved")

// ReviewPaymentRecord represents a review payment authorization record.
type ReviewPaymentRecord struct {
	PatchEventID        string
	RepoID              string
	AuthorPubkey        string
	Status              string // pending, token_spent, authorized
	AccessKind          string // free_tier, subscription, cashu_review, cashu_subscription
	RequestedMode       string // review, subscription
	TokenHash           string
	MintURL             string
	TokenAmountSats     int64
	ExpectedAmountSats  int64
	SubscriptionDays    int
	InvoiceID           string
	InvoiceRequest      string
	InvoiceAmountMSats  int64
	InvoiceExpiresAt    int64
	MeltQuoteID         string
	MeltQuoteAmountSats int64
	MeltFeeReserveSats  int64
	MeltState           string // empty, submitted, paid, unpaid
	CreatedAt           int64
	UpdatedAt           int64
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
		       COALESCE(token_hash, ''), mint_url, token_amount_sats, expected_amount_sats,
		       subscription_days, invoice_id, invoice_request, invoice_amount_msats,
		       invoice_expires_at, melt_quote_id, melt_quote_amount_sats,
		       melt_fee_reserve_sats, melt_state, created_at, updated_at
		FROM review_payments
		WHERE patch_event_id = ?
	`, patchEventID).Scan(
		&rec.PatchEventID, &rec.RepoID, &rec.AuthorPubkey, &rec.Status, &rec.AccessKind,
		&rec.RequestedMode, &rec.TokenHash, &rec.MintURL, &rec.TokenAmountSats,
		&rec.ExpectedAmountSats, &rec.SubscriptionDays, &rec.InvoiceID, &rec.InvoiceRequest,
		&rec.InvoiceAmountMSats, &rec.InvoiceExpiresAt, &rec.MeltQuoteID,
		&rec.MeltQuoteAmountSats, &rec.MeltFeeReserveSats, &rec.MeltState,
		&rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		return ReviewPaymentRecord{}, err
	}
	return rec, nil
}

// ReserveReviewPaymentToken atomically reserves a token hash before any external
// invoice or mint calls. A unique-constraint conflict is reported as
// ErrTokenHashAlreadyReserved so callers can distinguish replay/race denials
// from genuine database failures.
func (s *Store) ReserveReviewPaymentToken(ctx context.Context, rec ReviewPaymentRecord) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO review_payments (
			patch_event_id, repo_id, author_pubkey, status, access_kind, requested_mode,
			token_hash, mint_url, token_amount_sats, expected_amount_sats, subscription_days,
			invoice_id, invoice_request, invoice_amount_msats, invoice_expires_at,
			created_at, updated_at
		) VALUES (?, ?, ?, 'pending', '', ?, ?, ?, ?, ?, ?, '', '', 0, 0, ?, ?)
	`, rec.PatchEventID, rec.RepoID, rec.AuthorPubkey, rec.RequestedMode,
		rec.TokenHash, rec.MintURL, rec.TokenAmountSats, rec.ExpectedAmountSats,
		rec.SubscriptionDays, now, now)
	if err != nil {
		if isSQLiteUniqueConstraint(err) {
			return ErrTokenHashAlreadyReserved
		}
		return fmt.Errorf("reserve review payment token: %w", err)
	}
	return nil
}

// UpsertPendingReviewPayment inserts or updates invoice details for a pending review payment record.
func (s *Store) UpsertPendingReviewPayment(ctx context.Context, rec ReviewPaymentRecord) error {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO review_payments (
			patch_event_id, repo_id, author_pubkey, status, access_kind, requested_mode,
			token_hash, mint_url, token_amount_sats, expected_amount_sats, subscription_days,
			invoice_id, invoice_request, invoice_amount_msats, invoice_expires_at,
			created_at, updated_at
		) VALUES (?, ?, ?, 'pending', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(patch_event_id) DO UPDATE SET
			expected_amount_sats = excluded.expected_amount_sats,
			subscription_days = excluded.subscription_days,
			invoice_id = excluded.invoice_id,
			invoice_request = excluded.invoice_request,
			invoice_amount_msats = excluded.invoice_amount_msats,
			invoice_expires_at = excluded.invoice_expires_at,
			updated_at = excluded.updated_at
		WHERE review_payments.status = 'pending'
		  AND review_payments.token_hash = excluded.token_hash
	`, rec.PatchEventID, rec.RepoID, rec.AuthorPubkey, rec.RequestedMode,
		rec.TokenHash, rec.MintURL, rec.TokenAmountSats, rec.ExpectedAmountSats,
		rec.SubscriptionDays, rec.InvoiceID, rec.InvoiceRequest, rec.InvoiceAmountMSats,
		rec.InvoiceExpiresAt, now, now)
	if err != nil {
		if isSQLiteUniqueConstraint(err) {
			return ErrTokenHashAlreadyReserved
		}
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check pending payment upsert rows affected: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("upsert pending review payment: expected 1 row for patch %q, got %d", rec.PatchEventID, rows)
	}
	return nil
}

// GetReviewPaymentByTokenHash retrieves a review payment record by reserved token hash.
func (s *Store) GetReviewPaymentByTokenHash(ctx context.Context, tokenHash string) (ReviewPaymentRecord, error) {
	var rec ReviewPaymentRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT patch_event_id, repo_id, author_pubkey, status, access_kind, requested_mode,
		       COALESCE(token_hash, ''), mint_url, token_amount_sats, expected_amount_sats,
		       subscription_days, invoice_id, invoice_request, invoice_amount_msats,
		       invoice_expires_at, melt_quote_id, melt_quote_amount_sats,
		       melt_fee_reserve_sats, melt_state, created_at, updated_at
		FROM review_payments
		WHERE token_hash = ?
	`, tokenHash).Scan(
		&rec.PatchEventID, &rec.RepoID, &rec.AuthorPubkey, &rec.Status, &rec.AccessKind,
		&rec.RequestedMode, &rec.TokenHash, &rec.MintURL, &rec.TokenAmountSats,
		&rec.ExpectedAmountSats, &rec.SubscriptionDays, &rec.InvoiceID, &rec.InvoiceRequest,
		&rec.InvoiceAmountMSats, &rec.InvoiceExpiresAt, &rec.MeltQuoteID,
		&rec.MeltQuoteAmountSats, &rec.MeltFeeReserveSats, &rec.MeltState,
		&rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		return ReviewPaymentRecord{}, err
	}
	return rec, nil
}

// DeletePendingReviewPayment removes a pending review payment record.
func (s *Store) DeletePendingReviewPayment(ctx context.Context, patchEventID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM review_payments WHERE patch_event_id = ? AND status = 'pending'
	`, patchEventID)
	return err
}

// MarkReviewPaymentMeltSubmitted durably records the quote and submission intent.
// It must commit before the mint call so a crash or ambiguous transport error can
// be reconciled without ever submitting the proofs a second time.
func (s *Store) MarkReviewPaymentMeltSubmitted(ctx context.Context, patchEventID, tokenHash string, quoteID string, quoteAmount, feeReserve int64) error {
	if quoteID == "" || quoteAmount <= 0 || feeReserve < 0 {
		return errors.New("invalid melt quote evidence")
	}
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE review_payments
		SET melt_quote_id = ?, melt_quote_amount_sats = ?, melt_fee_reserve_sats = ?,
		    melt_state = 'submitted', updated_at = ?
		WHERE patch_event_id = ? AND token_hash = ? AND status = 'pending' AND melt_state = ''
	`, quoteID, quoteAmount, feeReserve, now, patchEventID, tokenHash)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check melt-submitted update: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("mark melt submitted: expected 1 row for patch %q, got %d", patchEventID, rows)
	}
	return nil
}

// MarkReviewPaymentMeltUnpaid records a definitive mint observation without
// releasing the token hash. A submitted quote is never deleted.
func (s *Store) MarkReviewPaymentMeltUnpaid(ctx context.Context, patchEventID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE review_payments SET melt_state = 'unpaid', updated_at = ?
		WHERE patch_event_id = ? AND status = 'pending' AND melt_state = 'submitted'
	`, time.Now().Unix(), patchEventID)
	return err
}

// MarkReviewPaymentTokenSpent records definitive mint evidence that the quote
// was paid. It is idempotent and monotonic.
func (s *Store) MarkReviewPaymentTokenSpent(ctx context.Context, patchEventID string) error {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE review_payments
		SET status = 'token_spent', melt_state = 'paid', updated_at = ?
		WHERE patch_event_id = ? AND status = 'pending' AND melt_state IN ('submitted', 'unpaid')
	`, now, patchEventID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check token-spent update rows affected: %w", err)
	}
	if rows == 1 {
		return nil
	}
	var status, meltState string
	if err := s.db.QueryRowContext(ctx, `SELECT status, melt_state FROM review_payments WHERE patch_event_id = ?`, patchEventID).Scan(&status, &meltState); err != nil {
		return err
	}
	if (status == "token_spent" || status == "authorized") && meltState == "paid" {
		return nil
	}
	return fmt.Errorf("mark review payment token spent: invalid state for patch %q", patchEventID)
}

// MarkReviewPaymentAuthorized marks a review payment as authorized.
// Only definitive token_spent records may be authorized.
func (s *Store) MarkReviewPaymentAuthorized(ctx context.Context, patchEventID, accessKind string) error {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE review_payments
		SET status = 'authorized', access_kind = ?, updated_at = ?
		WHERE patch_event_id = ? AND status = 'token_spent'
	`, accessKind, now, patchEventID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check authorized update rows affected: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("mark review payment authorized: expected 1 row updated for patch %q, got %d", patchEventID, rows)
	}
	return nil
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
			expires_at = CASE
				WHEN payment_subscriptions.source_token_hash = excluded.source_token_hash THEN payment_subscriptions.expires_at
				ELSE MAX(payment_subscriptions.expires_at, ?) + ?
			END,
			updated_at = ?
	`, authorPubkey, repoID, sourcePatchEventID, sourceTokenHash,
		paidAmountSats, now+extendSecs, now, now,
		now, extendSecs, now)
	return err
}

// FinalizePaidReview atomically creates/extends a subscription (when requested)
// and authorizes its source review. All source payment invariants are checked in
// the same transaction, and repeating an already-authorized payment is harmless.
func (s *Store) FinalizePaidReview(ctx context.Context, patchEventID, tokenHash string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var status, accessKind, requestedMode, storedToken, author, repoID, meltState string
	var expectedAmount, invoiceAmountMSats, quoteAmount int64
	var subscriptionDays int
	err = tx.QueryRowContext(ctx, `
		SELECT status, access_kind, requested_mode, COALESCE(token_hash, ''),
				author_pubkey, repo_id, expected_amount_sats, subscription_days, melt_state,
				invoice_amount_msats, melt_quote_amount_sats
		FROM review_payments WHERE patch_event_id = ?
	`, patchEventID).Scan(&status, &accessKind, &requestedMode, &storedToken,
		&author, &repoID, &expectedAmount, &subscriptionDays, &meltState,
		&invoiceAmountMSats, &quoteAmount)
	if err != nil {
		return "", err
	}
	if storedToken == "" || storedToken != tokenHash {
		return "", errors.New("payment token identity mismatch")
	}
	if status == "authorized" {
		if (requestedMode == "review" && accessKind != "cashu_review") ||
			(requestedMode == "subscription" && accessKind != "cashu_subscription") {
			return "", errors.New("authorized payment has inconsistent mode/access kind")
		}
		return accessKind, nil
	}
	if status != "token_spent" || meltState != "paid" || expectedAmount <= 0 ||
		expectedAmount > (1<<63-1)/1000 || invoiceAmountMSats != expectedAmount*1000 || quoteAmount != expectedAmount {
		return "", fmt.Errorf("payment does not have consistent definitive paid evidence (status=%s melt_state=%s)", status, meltState)
	}

	accessKind = "cashu_review"
	if requestedMode == "subscription" {
		if subscriptionDays <= 0 {
			return "", errors.New("subscription payment has invalid duration")
		}
		accessKind = "cashu_subscription"
		now := time.Now().Unix()
		extendSecs := int64(subscriptionDays) * 24 * 60 * 60
		_, err = tx.ExecContext(ctx, `
			INSERT INTO payment_subscriptions (
				author_pubkey, repo_id, source_patch_event_id, source_token_hash,
				paid_amount_sats, expires_at, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(author_pubkey, repo_id) DO UPDATE SET
				source_patch_event_id = excluded.source_patch_event_id,
				source_token_hash = excluded.source_token_hash,
				paid_amount_sats = excluded.paid_amount_sats,
				expires_at = CASE
					WHEN payment_subscriptions.source_token_hash = excluded.source_token_hash THEN payment_subscriptions.expires_at
					ELSE MAX(payment_subscriptions.expires_at, ?) + ?
				END,
				updated_at = ?
		`, author, repoID, patchEventID, tokenHash, expectedAmount,
			now+extendSecs, now, now, now, extendSecs, now)
		if err != nil {
			return "", fmt.Errorf("upsert subscription: %w", err)
		}
	} else if requestedMode != "review" {
		return "", fmt.Errorf("unsupported payment mode %q", requestedMode)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE review_payments SET status = 'authorized', access_kind = ?, updated_at = ?
		WHERE patch_event_id = ? AND token_hash = ? AND status = 'token_spent' AND melt_state = 'paid'
	`, accessKind, time.Now().Unix(), patchEventID, tokenHash)
	if err != nil {
		return "", err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("read conditional payment authorization result: %w", err)
	}
	if rows != 1 {
		return "", fmt.Errorf("conditional payment authorization affected %d rows", rows)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return accessKind, nil
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

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
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
// Considers both 'token_spent' and 'authorized' status to prevent double-spending during recovery.
func (s *Store) IsTokenHashUsed(ctx context.Context, tokenHash string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM review_payments WHERE token_hash = ? AND status IN ('token_spent', 'authorized')
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

func isSQLiteUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}
