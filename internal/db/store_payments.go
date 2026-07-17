package db

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

	"fiatjaf.com/nostr"
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

// MarketplacePayoutRecord is the durable reviewer payout allocated to one assignment.
type MarketplacePayoutRecord struct {
	AssignmentID      int
	IdempotencyKey    string
	AmountSats        int64
	Destination       string
	Status            string
	PaymentHash       string
	Preimage          string
	FailureReason     string
	CompletionEventID string
	ReviewEventID     string
	SubmittedAt       int64
	SettledAt         int64
	CreatedAt         int64
	UpdatedAt         int64
}

// MarketplacePayoutAuditRecord is an immutable payout transition record.
type MarketplacePayoutAuditRecord struct {
	ID                int64
	AssignmentID      int
	FromStatus        string
	ToStatus          string
	CompletionEventID string
	PaymentHash       string
	Detail            string
	CreatedAt         int64
}

// CompleteAssignmentAndAllocatePayout atomically authenticates the completion
// against the assigned reviewer and stored published review, transitions
// accepted->completed, and allocates exactly one payout for paid assignments.
func (s *Store) CompleteAssignmentAndAllocatePayout(ctx context.Context, assignmentEventID, reviewerPubkey, completionEventID, reviewEventID string, now int64) (MarketplacePayoutRecord, bool, error) {
	if assignmentEventID == "" || reviewerPubkey == "" || completionEventID == "" || reviewEventID == "" {
		return MarketplacePayoutRecord{}, false, errors.New("assignment, reviewer, completion event, and review event are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MarketplacePayoutRecord{}, false, err
	}
	defer tx.Rollback()

	var assignmentID int
	var patchEventID, repoID, storedReviewer, status, storedCompletion, storedReview, destination string
	var priceSats int64
	err = tx.QueryRowContext(ctx, `
		SELECT a.id, a.patch_event_id, a.repo_id, a.reviewer_pubkey, a.status,
		       COALESCE(a.completion_event_id, ''), COALESCE(a.review_event_id, ''),
		       a.price_sats, COALESCE(r.payout_destination, '')
		FROM review_assignments a
		LEFT JOIN reviewer_profiles r ON r.pubkey = a.reviewer_pubkey
		WHERE a.assignment_event_id = ?
	`, assignmentEventID).Scan(&assignmentID, &patchEventID, &repoID, &storedReviewer, &status,
		&storedCompletion, &storedReview, &priceSats, &destination)
	if err != nil {
		return MarketplacePayoutRecord{}, false, fmt.Errorf("lookup completion assignment: %w", err)
	}
	if storedReviewer != reviewerPubkey {
		return MarketplacePayoutRecord{}, false, fmt.Errorf("unauthorized reviewer: sender %s is not assigned reviewer %s", reviewerPubkey, storedReviewer)
	}

	var reviewPatch, reviewRepo, rawReview string
	if err := tx.QueryRowContext(ctx, `
		SELECT patch_event_id, repo_id, raw_event_json FROM review_events WHERE event_id = ?
	`, reviewEventID).Scan(&reviewPatch, &reviewRepo, &rawReview); err != nil {
		return MarketplacePayoutRecord{}, false, fmt.Errorf("published review event %s not found: %w", reviewEventID, err)
	}
	var reviewEvent nostr.Event
	if err := json.Unmarshal([]byte(rawReview), &reviewEvent); err != nil {
		return MarketplacePayoutRecord{}, false, fmt.Errorf("decode published review event: %w", err)
	}
	if reviewEvent.ID.Hex() != reviewEventID || !reviewEvent.CheckID() || !reviewEvent.VerifySignature() {
		return MarketplacePayoutRecord{}, false, errors.New("published review event failed integrity verification")
	}
	if reviewEvent.PubKey.Hex() != reviewerPubkey {
		return MarketplacePayoutRecord{}, false, fmt.Errorf("published review author %s is not assigned reviewer %s", reviewEvent.PubKey.Hex(), reviewerPubkey)
	}
	if reviewPatch != patchEventID || reviewRepo != repoID {
		return MarketplacePayoutRecord{}, false, fmt.Errorf("published review correlation mismatch for assignment %s", assignmentEventID)
	}

	completedNow := false
	if status == "accepted" {
		result, err := tx.ExecContext(ctx, `
			UPDATE review_assignments
			SET status = 'completed', completion_event_id = ?, review_event_id = ?, updated_at = ?
			WHERE id = ? AND reviewer_pubkey = ? AND status = 'accepted'
		`, completionEventID, reviewEventID, now, assignmentID, reviewerPubkey)
		if err != nil {
			return MarketplacePayoutRecord{}, false, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return MarketplacePayoutRecord{}, false, err
		}
		if rows != 1 {
			return MarketplacePayoutRecord{}, false, fmt.Errorf("completion transition affected %d rows", rows)
		}
		completedNow = true
	} else if status != "completed" || storedCompletion != completionEventID || storedReview != reviewEventID {
		return MarketplacePayoutRecord{}, false, fmt.Errorf("assignment %s is not accepted: %s", assignmentEventID, status)
	}

	if priceSats <= 0 {
		if err := tx.Commit(); err != nil {
			return MarketplacePayoutRecord{}, false, err
		}
		return MarketplacePayoutRecord{}, false, nil
	}
	destination = strings.TrimSpace(destination)
	if destination == "" {
		return MarketplacePayoutRecord{}, false, errors.New("paid assignment reviewer has no payout destination")
	}
	idempotencyKey := "marketplace-payout:" + assignmentEventID
	result, err := tx.ExecContext(ctx, `
		INSERT INTO marketplace_payouts (
			assignment_id, idempotency_key, amount_sats, destination, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, 'pending', ?, ?)
		ON CONFLICT(assignment_id) DO NOTHING
	`, assignmentID, idempotencyKey, priceSats, destination, now, now)
	if err != nil {
		return MarketplacePayoutRecord{}, false, fmt.Errorf("allocate marketplace payout: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return MarketplacePayoutRecord{}, false, err
	}
	if inserted == 1 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO marketplace_payout_audit
				(assignment_id, from_status, to_status, completion_event_id, detail, created_at)
			VALUES (?, '', 'pending', ?, 'completion allocated payout', ?)
		`, assignmentID, completionEventID, now); err != nil {
			return MarketplacePayoutRecord{}, false, fmt.Errorf("audit payout allocation: %w", err)
		}
	} else if completedNow {
		return MarketplacePayoutRecord{}, false, errors.New("completed assignment unexpectedly already had a payout")
	}

	rec, err := getMarketplacePayoutTx(ctx, tx, assignmentID)
	if err != nil {
		return MarketplacePayoutRecord{}, false, err
	}
	if rec.IdempotencyKey != idempotencyKey || rec.AmountSats != priceSats || rec.Destination != destination {
		return MarketplacePayoutRecord{}, false, errors.New("existing payout does not match assignment allocation")
	}
	rec.CompletionEventID, rec.ReviewEventID = completionEventID, reviewEventID
	if err := tx.Commit(); err != nil {
		return MarketplacePayoutRecord{}, false, err
	}
	return rec, true, nil
}

func getMarketplacePayoutTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, assignmentID int) (MarketplacePayoutRecord, error) {
	var rec MarketplacePayoutRecord
	err := q.QueryRowContext(ctx, `
		SELECT assignment_id, idempotency_key, amount_sats, destination, status,
		       payment_hash, preimage, failure_reason, submitted_at, settled_at, created_at, updated_at
		FROM marketplace_payouts WHERE assignment_id = ?
	`, assignmentID).Scan(&rec.AssignmentID, &rec.IdempotencyKey, &rec.AmountSats, &rec.Destination,
		&rec.Status, &rec.PaymentHash, &rec.Preimage, &rec.FailureReason, &rec.SubmittedAt,
		&rec.SettledAt, &rec.CreatedAt, &rec.UpdatedAt)
	return rec, err
}

// GetMarketplacePayout returns the payout for an assignment.
func (s *Store) GetMarketplacePayout(ctx context.Context, assignmentID int) (MarketplacePayoutRecord, error) {
	return getMarketplacePayoutTx(ctx, s.db, assignmentID)
}

// MarkMarketplacePayoutSubmitted durably records submission intent and audit
// before any external wallet call. claimed is true only for the caller that may
// submit externally; concurrent callers must reconcile instead.
func (s *Store) MarkMarketplacePayoutSubmitted(ctx context.Context, assignmentID int, now int64) (claimed bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, "SELECT status FROM marketplace_payouts WHERE assignment_id=?", assignmentID).Scan(&status); err != nil {
		return false, err
	}
	if status != "pending" {
		if status == "submitted" || status == "settled" || status == "failed" {
			return false, tx.Commit()
		}
		return false, fmt.Errorf("payout %d has invalid state %s", assignmentID, status)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE marketplace_payouts SET status='submitted', submitted_at=?, updated_at=?
		WHERE assignment_id=? AND status='pending'
	`, now, now, assignmentID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return false, fmt.Errorf("claim payout submission affected %d rows (err=%v)", rows, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO marketplace_payout_audit
			(assignment_id, from_status, to_status, detail, created_at)
		VALUES (?, 'pending', 'submitted', 'wallet submission claimed', ?)
	`, assignmentID, now); err != nil {
		return false, fmt.Errorf("audit payout submission: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// MarkMarketplacePayoutFailed records a definitive failure. Ambiguous outcomes
// must not call this method and remain submitted for reconciliation.
func (s *Store) MarkMarketplacePayoutFailed(ctx context.Context, assignmentID int, reason string, now int64) error {
	if strings.TrimSpace(reason) == "" {
		return errors.New("payout failure reason is required")
	}
	return s.transitionMarketplacePayout(ctx, assignmentID, "submitted", "failed", "", reason, now, 0)
}

// MarkMarketplacePayoutSettled records settlement only with hash+preimage evidence.
func (s *Store) MarkMarketplacePayoutSettled(ctx context.Context, assignmentID int, paymentHash, preimage string, settledAt, now int64) error {
	preimageBytes, preimageErr := hex.DecodeString(preimage)
	hashBytes, hashErr := hex.DecodeString(paymentHash)
	if preimageErr != nil || hashErr != nil || len(preimageBytes) != 32 || len(hashBytes) != 32 || settledAt <= 0 || settledAt > now+300 {
		return errors.New("payout settlement evidence is incomplete or invalid")
	}
	derived := sha256.Sum256(preimageBytes)
	if !strings.EqualFold(hex.EncodeToString(derived[:]), paymentHash) {
		return errors.New("payout preimage does not match payment hash")
	}
	return s.transitionMarketplacePayout(ctx, assignmentID, "submitted", "settled", paymentHash, preimage, now, settledAt)
}

func (s *Store) transitionMarketplacePayout(ctx context.Context, assignmentID int, from, to, paymentHash, detail string, now, settledAt int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, storedHash, storedPreimage string
	if err := tx.QueryRowContext(ctx, `
		SELECT status, payment_hash, preimage FROM marketplace_payouts WHERE assignment_id = ?
	`, assignmentID).Scan(&status, &storedHash, &storedPreimage); err != nil {
		return err
	}
	if status == to {
		if to != "settled" || (storedHash == paymentHash && storedPreimage == detail) {
			return tx.Commit()
		}
		return errors.New("payout already settled with different evidence")
	}
	if status != from {
		return fmt.Errorf("payout %d cannot transition %s -> %s", assignmentID, status, to)
	}

	var result sql.Result
	switch to {
	case "submitted":
		result, err = tx.ExecContext(ctx, `
			UPDATE marketplace_payouts SET status='submitted', submitted_at=?, updated_at=?
			WHERE assignment_id=? AND status='pending'
		`, now, now, assignmentID)
	case "failed":
		result, err = tx.ExecContext(ctx, `
			UPDATE marketplace_payouts SET status='failed', failure_reason=?, updated_at=?
			WHERE assignment_id=? AND status='submitted'
		`, detail, now, assignmentID)
	case "settled":
		result, err = tx.ExecContext(ctx, `
			UPDATE marketplace_payouts
			SET status='settled', payment_hash=?, preimage=?, settled_at=?, updated_at=?
			WHERE assignment_id=? AND status='submitted'
		`, paymentHash, detail, settledAt, now, assignmentID)
	default:
		return fmt.Errorf("unsupported payout state %q", to)
	}
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read payout transition result: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("payout transition %s -> %s affected %d rows", from, to, rows)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO marketplace_payout_audit
			(assignment_id, from_status, to_status, payment_hash, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, assignmentID, from, to, paymentHash, detail, now); err != nil {
		return fmt.Errorf("audit payout transition: %w", err)
	}
	return tx.Commit()
}

// ListMarketplacePayoutAudit returns the immutable transition history.
func (s *Store) ListMarketplacePayoutAudit(ctx context.Context, assignmentID int) ([]MarketplacePayoutAuditRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, assignment_id, from_status, to_status, completion_event_id,
		       payment_hash, detail, created_at
		FROM marketplace_payout_audit WHERE assignment_id = ? ORDER BY id
	`, assignmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MarketplacePayoutAuditRecord
	for rows.Next() {
		var rec MarketplacePayoutAuditRecord
		if err := rows.Scan(&rec.ID, &rec.AssignmentID, &rec.FromStatus, &rec.ToStatus,
			&rec.CompletionEventID, &rec.PaymentHash, &rec.Detail, &rec.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func isSQLiteUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}
