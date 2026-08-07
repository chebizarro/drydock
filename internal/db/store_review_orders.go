package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ReviewOrderReceipt is the immutable durable identity of an accepted order.
type ReviewOrderReceipt struct {
	RequesterPubkey   string
	OrderID           string
	RequestEventID    string
	PatchEventID      string
	RepositoryID      string
	RepositoryAddress string
	Force             bool
	AcceptedAt        int64
}

type ReviewOrderDisposition string

const (
	ReviewOrderAcquired   ReviewOrderDisposition = "acquired"
	ReviewOrderIdempotent ReviewOrderDisposition = "idempotent"
	ReviewOrderConflict   ReviewOrderDisposition = "conflict"
)

type ReviewOrderResult struct {
	Disposition ReviewOrderDisposition
	Receipt     ReviewOrderReceipt
}

// AcceptReviewOrder atomically stores an order receipt and claims its review.
func (s *Store) AcceptReviewOrder(ctx context.Context, receipt ReviewOrderReceipt, claim ReviewClaim) (ReviewOrderResult, error) {
	receipt = normalizeReviewOrderReceipt(receipt)
	if receipt.RequesterPubkey == "" || receipt.OrderID == "" || receipt.RequestEventID == "" ||
		receipt.PatchEventID == "" || receipt.RepositoryID == "" || receipt.RepositoryAddress == "" {
		return ReviewOrderResult{}, errors.New("review order receipt is incomplete")
	}
	claim.Force = receipt.Force
	claim.Invocation = ReviewInvocationContextVM
	claim.RequesterPubkey = receipt.RequesterPubkey
	claim.OrderID = receipt.OrderID
	var err error
	claim, err = claim.normalized()
	if err != nil {
		return ReviewOrderResult{}, err
	}
	if receipt.AcceptedAt <= 0 {
		receipt.AcceptedAt = time.Now().Unix()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewOrderResult{}, fmt.Errorf("begin review order transaction: %w", err)
	}
	defer tx.Rollback()

	if existing, ok, err := getReviewOrderTx(ctx, tx, receipt.RequesterPubkey, receipt.OrderID); err != nil {
		return ReviewOrderResult{}, err
	} else if ok {
		if sameReviewOrder(existing, receipt) {
			return ReviewOrderResult{Disposition: ReviewOrderIdempotent, Receipt: existing}, nil
		}
		return ReviewOrderResult{Disposition: ReviewOrderConflict, Receipt: existing}, nil
	}

	force := 0
	if receipt.Force {
		force = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_orders(
		requester_pubkey, order_id, request_event_id, patch_event_id,
		repository_id, repository_addr, force, accepted_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		receipt.RequesterPubkey, receipt.OrderID, receipt.RequestEventID,
		receipt.PatchEventID, receipt.RepositoryID, receipt.RepositoryAddress,
		force, receipt.AcceptedAt,
	); err != nil {
		if existing, ok, _ := getReviewOrderByEventTx(ctx, tx, receipt.RequestEventID); ok {
			return ReviewOrderResult{Disposition: ReviewOrderConflict, Receipt: existing}, nil
		}
		return ReviewOrderResult{}, fmt.Errorf("insert review order: %w", err)
	}

	acquired, err := beginReviewTx(ctx, tx, receipt.PatchEventID, receipt.RepositoryID, claim, receipt.AcceptedAt)
	if errors.Is(err, ErrReviewAlreadyPublished) {
		return ReviewOrderResult{Disposition: ReviewOrderConflict}, nil
	}
	if err != nil {
		return ReviewOrderResult{}, err
	}
	if !acquired {
		return ReviewOrderResult{Disposition: ReviewOrderConflict}, nil
	}
	if err := tx.Commit(); err != nil {
		return ReviewOrderResult{}, fmt.Errorf("commit review order transaction: %w", err)
	}
	return ReviewOrderResult{Disposition: ReviewOrderAcquired, Receipt: receipt}, nil
}

func (s *Store) GetReviewOrder(ctx context.Context, requesterPubkey, orderID string) (ReviewOrderReceipt, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT requester_pubkey, order_id, request_event_id,
		patch_event_id, repository_id, repository_addr, force, accepted_at
		FROM review_orders WHERE requester_pubkey=? AND order_id=?`,
		strings.TrimSpace(requesterPubkey), strings.TrimSpace(orderID))
	receipt, err := scanReviewOrder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewOrderReceipt{}, false, nil
	}
	if err != nil {
		return ReviewOrderReceipt{}, false, fmt.Errorf("get review order: %w", err)
	}
	return receipt, true, nil
}

// MarkReviewSkipped durably records a permanent skip using the existing failed
// status plus a status_skipped reason, preserving compatibility with old
// binaries while keeping structured skip state in review_skips.
func (s *Store) MarkReviewSkipped(ctx context.Context, patchEventID, repoID, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("review skip reason is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review skip transaction: %w", err)
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM review_log
		WHERE patch_event_id=? AND repo_id=?`, patchEventID, repoID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReviewNotFound
		}
		return fmt.Errorf("load review for skip: %w", err)
	}
	if status == "published" {
		return ErrReviewAlreadyPublished
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_skips(
		patch_event_id, repo_id, reason, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?)
	ON CONFLICT(patch_event_id, repo_id) DO UPDATE SET
		reason=excluded.reason, updated_at=excluded.updated_at`,
		patchEventID, repoID, reason, now, now,
	); err != nil {
		return fmt.Errorf("store review skip: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_log
		SET status='failed', failure_reason=?, updated_at=?
		WHERE patch_event_id=? AND repo_id=?`,
		"status_skipped:"+reason, now, patchEventID, repoID,
	); err != nil {
		return fmt.Errorf("mark review skipped: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review skip: %w", err)
	}
	return nil
}

func (s *Store) GetReviewSkip(ctx context.Context, patchEventID, repoID string) (string, bool, error) {
	var reason string
	err := s.db.QueryRowContext(ctx, `SELECT reason FROM review_skips
		WHERE patch_event_id=? AND repo_id=?`, patchEventID, repoID).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get review skip: %w", err)
	}
	return reason, true, nil
}

func normalizeReviewOrderReceipt(receipt ReviewOrderReceipt) ReviewOrderReceipt {
	receipt.RequesterPubkey = strings.TrimSpace(receipt.RequesterPubkey)
	receipt.OrderID = strings.TrimSpace(receipt.OrderID)
	receipt.RequestEventID = strings.TrimSpace(receipt.RequestEventID)
	receipt.PatchEventID = strings.TrimSpace(receipt.PatchEventID)
	receipt.RepositoryID = strings.TrimSpace(receipt.RepositoryID)
	receipt.RepositoryAddress = strings.TrimSpace(receipt.RepositoryAddress)
	return receipt
}

func sameReviewOrder(a, b ReviewOrderReceipt) bool {
	return a.RequesterPubkey == b.RequesterPubkey &&
		a.OrderID == b.OrderID &&
		a.PatchEventID == b.PatchEventID &&
		a.RepositoryID == b.RepositoryID &&
		a.RepositoryAddress == b.RepositoryAddress &&
		a.Force == b.Force
}

type reviewOrderScanner interface {
	Scan(...any) error
}

func scanReviewOrder(scanner reviewOrderScanner) (ReviewOrderReceipt, error) {
	var receipt ReviewOrderReceipt
	var force int
	err := scanner.Scan(
		&receipt.RequesterPubkey, &receipt.OrderID, &receipt.RequestEventID,
		&receipt.PatchEventID, &receipt.RepositoryID, &receipt.RepositoryAddress,
		&force, &receipt.AcceptedAt,
	)
	receipt.Force = force != 0
	return receipt, err
}

func getReviewOrderTx(ctx context.Context, tx *sql.Tx, requesterPubkey, orderID string) (ReviewOrderReceipt, bool, error) {
	receipt, err := scanReviewOrder(tx.QueryRowContext(ctx, `SELECT requester_pubkey, order_id,
		request_event_id, patch_event_id, repository_id, repository_addr, force, accepted_at
		FROM review_orders WHERE requester_pubkey=? AND order_id=?`, requesterPubkey, orderID))
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewOrderReceipt{}, false, nil
	}
	if err != nil {
		return ReviewOrderReceipt{}, false, fmt.Errorf("get existing review order: %w", err)
	}
	return receipt, true, nil
}

func getReviewOrderByEventTx(ctx context.Context, tx *sql.Tx, eventID string) (ReviewOrderReceipt, bool, error) {
	receipt, err := scanReviewOrder(tx.QueryRowContext(ctx, `SELECT requester_pubkey, order_id,
		request_event_id, patch_event_id, repository_id, repository_addr, force, accepted_at
		FROM review_orders WHERE request_event_id=?`, eventID))
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewOrderReceipt{}, false, nil
	}
	if err != nil {
		return ReviewOrderReceipt{}, false, fmt.Errorf("get review order by event: %w", err)
	}
	return receipt, true, nil
}
