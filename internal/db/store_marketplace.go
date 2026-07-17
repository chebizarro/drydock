package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ReviewerProfile holds reviewer information for marketplace operations.
type ReviewerProfile struct {
	Pubkey         string   `json:"pubkey"`
	DisplayName    string   `json:"display_name,omitempty"`
	Languages      []string `json:"languages"`
	Domains        []string `json:"domains"`
	Availability   string   `json:"availability"`
	PricePerReview int64    `json:"price_per_review,omitempty"`
	MaxConcurrent  int      `json:"max_concurrent"`
	EventID        string   `json:"event_id"`
	CreatedAt      int64    `json:"created_at"`
	UpdatedAt      int64    `json:"updated_at"`
}

// ReputationScore holds a reviewer's computed reputation.
type ReputationScore struct {
	Pubkey         string  `json:"pubkey"`
	OverallScore   float64 `json:"overall_score"`
	TotalReviews   int     `json:"total_reviews"`
	AcceptedCount  int     `json:"accepted_count"`
	RejectedCount  int     `json:"rejected_count"`
	AverageRating  float64 `json:"average_rating"`
	AcceptanceRate float64 `json:"acceptance_rate"`
	LastReviewAt   int64   `json:"last_review_at"`
	UpdatedAt      int64   `json:"updated_at"`
}

// ReviewAssignment represents a review task assigned to a reviewer.
type ReviewAssignment struct {
	ID                int    `json:"id"`
	PatchEventID      string `json:"patch_event_id"`
	RepoID            string `json:"repo_id"`
	ReviewerPubkey    string `json:"reviewer_pubkey"`
	RequesterPubkey   string `json:"requester_pubkey"`
	Status            string `json:"status"`
	Priority          int    `json:"priority"`
	PriceSats         int64  `json:"price_sats"`
	AssignmentEventID string `json:"assignment_event_id"`
	AcceptanceEventID string `json:"acceptance_event_id,omitempty"`
	CompletionEventID string `json:"completion_event_id,omitempty"`
	ExpiresAt         int64  `json:"expires_at"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

// ReviewFeedback represents feedback on a completed review.
type ReviewFeedback struct {
	ID             int    `json:"id"`
	AssignmentID   int    `json:"assignment_id"`
	ReviewerPubkey string `json:"reviewer_pubkey"`
	RaterPubkey    string `json:"rater_pubkey"`
	Rating         int    `json:"rating"`
	Comment        string `json:"comment"`
	EventID        string `json:"event_id"`
	CreatedAt      int64  `json:"created_at"`
}

// ReviewerStats holds aggregated stats for reputation calculation.
type ReviewerStats struct {
	TotalAssignments    int
	AcceptedAssignments int
	RejectedAssignments int
	CompletedReviews    int
	TotalFeedback       int
	TotalRatingSum      float64
	LastReviewAt        int64
}

// UpsertReviewerProfile inserts or updates a reviewer profile.
func (s *Store) UpsertReviewerProfile(ctx context.Context, profile ReviewerProfile, eventID string) error {
	languagesJSON, _ := json.Marshal(profile.Languages)
	domainsJSON, _ := json.Marshal(profile.Domains)
	now := time.Now().Unix()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO reviewer_profiles (
			pubkey, display_name, languages, domains,
			availability, price_per_review, max_concurrent,
			event_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pubkey) DO UPDATE SET
			display_name = excluded.display_name,
			languages = excluded.languages,
			domains = excluded.domains,
			availability = excluded.availability,
			price_per_review = excluded.price_per_review,
			max_concurrent = excluded.max_concurrent,
			event_id = excluded.event_id,
			updated_at = excluded.updated_at
	`,
		profile.Pubkey, profile.DisplayName, string(languagesJSON), string(domainsJSON),
		profile.Availability, profile.PricePerReview, profile.MaxConcurrent,
		eventID, now, now,
	)
	return err
}

// GetReviewerProfile retrieves a reviewer profile by pubkey.
func (s *Store) GetReviewerProfile(ctx context.Context, pubkey string) (*ReviewerProfile, error) {
	var p ReviewerProfile
	var languagesJSON, domainsJSON string

	err := s.db.QueryRowContext(ctx, `
		SELECT pubkey, display_name, languages, domains,
				availability, price_per_review, max_concurrent,
				event_id, created_at, updated_at
		FROM reviewer_profiles WHERE pubkey = ?
	`, pubkey).Scan(
		&p.Pubkey, &p.DisplayName, &languagesJSON, &domainsJSON,
		&p.Availability, &p.PricePerReview, &p.MaxConcurrent,
		&p.EventID, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("reviewer not found: %s", pubkey)
	}
	if err != nil {
		return nil, err
	}

	_ = json.Unmarshal([]byte(languagesJSON), &p.Languages)
	_ = json.Unmarshal([]byte(domainsJSON), &p.Domains)

	return &p, nil
}

// ListAvailableReviewers returns all reviewers who are not unavailable.
func (s *Store) ListAvailableReviewers(ctx context.Context) ([]ReviewerProfile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pubkey, display_name, languages, domains,
				availability, price_per_review, max_concurrent,
				event_id, created_at, updated_at
		FROM reviewer_profiles
		WHERE availability != 'unavailable'
		ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []ReviewerProfile
	for rows.Next() {
		var p ReviewerProfile
		var languagesJSON, domainsJSON string

		if err := rows.Scan(
			&p.Pubkey, &p.DisplayName, &languagesJSON, &domainsJSON,
			&p.Availability, &p.PricePerReview, &p.MaxConcurrent,
			&p.EventID, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}

		_ = json.Unmarshal([]byte(languagesJSON), &p.Languages)
		_ = json.Unmarshal([]byte(domainsJSON), &p.Domains)

		profiles = append(profiles, p)
	}

	return profiles, rows.Err()
}

// UpdateReviewerAvailability updates a reviewer's availability status.
func (s *Store) UpdateReviewerAvailability(ctx context.Context, pubkey, availability string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE reviewer_profiles SET availability = ?, updated_at = ?
		WHERE pubkey = ?
	`, availability, time.Now().Unix(), pubkey)
	return err
}

// CountActiveAssignments returns how many pending/accepted assignments a reviewer has.
func (s *Store) CountActiveAssignments(ctx context.Context, pubkey string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM review_assignments
		WHERE reviewer_pubkey = ? AND status IN ('pending', 'accepted')
	`, pubkey).Scan(&count)
	return count, err
}

// GetReviewerReputation retrieves a reviewer's reputation score.
func (s *Store) GetReviewerReputation(ctx context.Context, pubkey string) (*ReputationScore, error) {
	var r ReputationScore
	err := s.db.QueryRowContext(ctx, `
		SELECT pubkey, overall_score, total_reviews, accepted_reviews, rejected_reviews,
				average_rating, acceptance_rate, last_review_at, updated_at
		FROM reviewer_reputations WHERE pubkey = ?
	`, pubkey).Scan(
		&r.Pubkey, &r.OverallScore, &r.TotalReviews, &r.AcceptedCount, &r.RejectedCount,
		&r.AverageRating, &r.AcceptanceRate, &r.LastReviewAt, &r.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// Return default reputation for new reviewers
		return &ReputationScore{
			Pubkey:       pubkey,
			OverallScore: 0.5,
		}, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// UpsertReviewerReputation inserts or updates a reviewer's reputation.
func (s *Store) UpsertReviewerReputation(ctx context.Context, rep ReputationScore) error {
	now := time.Now().Unix()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO reviewer_reputations (
			pubkey, overall_score, total_reviews, accepted_reviews, rejected_reviews,
			average_rating, acceptance_rate, last_review_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pubkey) DO UPDATE SET
			overall_score = excluded.overall_score,
			total_reviews = excluded.total_reviews,
			accepted_reviews = excluded.accepted_reviews,
			rejected_reviews = excluded.rejected_reviews,
			average_rating = excluded.average_rating,
			acceptance_rate = excluded.acceptance_rate,
			last_review_at = excluded.last_review_at,
			updated_at = excluded.updated_at
	`,
		rep.Pubkey, rep.OverallScore, rep.TotalReviews, rep.AcceptedCount, rep.RejectedCount,
		rep.AverageRating, rep.AcceptanceRate, rep.LastReviewAt, now,
	)
	return err
}

// CreateAssignment inserts a new review assignment.
func (s *Store) CreateAssignment(ctx context.Context, a ReviewAssignment) error {
	now := time.Now().Unix()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO review_assignments (
			patch_event_id, repo_id, reviewer_pubkey, requester_pubkey,
			status, priority, price_sats, assignment_event_id,
			expires_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		a.PatchEventID, a.RepoID, a.ReviewerPubkey, a.RequesterPubkey,
		a.Status, a.Priority, a.PriceSats, a.AssignmentEventID,
		a.ExpiresAt, now, now,
	)
	return err
}

// UpsertAssignmentReceipt stores an assignment delivered over ContextVM idempotently.
// Existing transition state is preserved when the assignment was already recorded
// by the router before delivery.
func (s *Store) UpsertAssignmentReceipt(ctx context.Context, a ReviewAssignment) error {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO review_assignments (
			patch_event_id, repo_id, reviewer_pubkey, requester_pubkey,
			status, priority, price_sats, assignment_event_id,
			expires_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(assignment_event_id) DO UPDATE SET
			requester_pubkey = CASE
				WHEN review_assignments.requester_pubkey = '' THEN excluded.requester_pubkey
				ELSE review_assignments.requester_pubkey
			END,
			price_sats = excluded.price_sats,
			expires_at = excluded.expires_at,
			updated_at = excluded.updated_at
		WHERE review_assignments.patch_event_id = excluded.patch_event_id
			AND review_assignments.repo_id = excluded.repo_id
			AND review_assignments.reviewer_pubkey = excluded.reviewer_pubkey
	`,
		a.PatchEventID, a.RepoID, a.ReviewerPubkey, a.RequesterPubkey,
		a.Status, a.Priority, a.PriceSats, a.AssignmentEventID,
		a.ExpiresAt, now, now,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("assignment receipt rows affected: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("assignment receipt conflicts with existing assignment %s", a.AssignmentEventID)
	}
	return nil
}

// GetAssignmentByID retrieves an assignment by its database ID.
func (s *Store) GetAssignmentByID(ctx context.Context, id int) (*ReviewAssignment, error) {
	var a ReviewAssignment
	var acceptanceEventID, completionEventID sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, patch_event_id, repo_id, reviewer_pubkey, requester_pubkey,
				status, priority, price_sats, assignment_event_id,
				acceptance_event_id, completion_event_id, expires_at, created_at, updated_at
		FROM review_assignments WHERE id = ?
	`, id).Scan(
		&a.ID, &a.PatchEventID, &a.RepoID, &a.ReviewerPubkey, &a.RequesterPubkey,
		&a.Status, &a.Priority, &a.PriceSats, &a.AssignmentEventID,
		&acceptanceEventID, &completionEventID, &a.ExpiresAt, &a.CreatedAt, &a.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("assignment not found: %d", id)
	}
	if err != nil {
		return nil, err
	}

	if acceptanceEventID.Valid {
		a.AcceptanceEventID = acceptanceEventID.String
	}
	if completionEventID.Valid {
		a.CompletionEventID = completionEventID.String
	}

	return &a, nil
}

// GetAssignmentByEventID retrieves an assignment by its Nostr event ID.
func (s *Store) GetAssignmentByEventID(ctx context.Context, eventID string) (*ReviewAssignment, error) {
	return s.getAssignmentByColumn(ctx, "assignment_event_id", eventID)
}

// GetAssignmentByCompletionEventID retrieves an assignment by its completed review event ID.
func (s *Store) GetAssignmentByCompletionEventID(ctx context.Context, eventID string) (*ReviewAssignment, error) {
	return s.getAssignmentByColumn(ctx, "completion_event_id", eventID)
}

func (s *Store) getAssignmentByColumn(ctx context.Context, column, eventID string) (*ReviewAssignment, error) {
	var a ReviewAssignment
	var acceptanceEventID, completionEventID sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, patch_event_id, repo_id, reviewer_pubkey, requester_pubkey,
				status, priority, price_sats, assignment_event_id,
				acceptance_event_id, completion_event_id, expires_at, created_at, updated_at
		FROM review_assignments WHERE `+column+` = ?
	`, eventID).Scan(
		&a.ID, &a.PatchEventID, &a.RepoID, &a.ReviewerPubkey, &a.RequesterPubkey,
		&a.Status, &a.Priority, &a.PriceSats, &a.AssignmentEventID,
		&acceptanceEventID, &completionEventID, &a.ExpiresAt, &a.CreatedAt, &a.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("assignment not found: %s", eventID)
	}
	if err != nil {
		return nil, err
	}

	if acceptanceEventID.Valid {
		a.AcceptanceEventID = acceptanceEventID.String
	}
	if completionEventID.Valid {
		a.CompletionEventID = completionEventID.String
	}

	return &a, nil
}

// UpdateAssignmentStatus updates the status of an assignment.
func (s *Store) UpdateAssignmentStatus(ctx context.Context, id int, status string, eventID string) error {
	now := time.Now().Unix()

	var query string
	var args []interface{}

	switch status {
	case "accepted":
		query = `UPDATE review_assignments SET status = ?, acceptance_event_id = ?, updated_at = ? WHERE id = ?`
		args = []interface{}{status, eventID, now, id}
	case "completed":
		query = `UPDATE review_assignments SET status = ?, completion_event_id = ?, updated_at = ? WHERE id = ?`
		args = []interface{}{status, eventID, now, id}
	default:
		query = `UPDATE review_assignments SET status = ?, updated_at = ? WHERE id = ?`
		args = []interface{}{status, now, id}
	}

	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

// TransitionPendingAssignment atomically accepts or rejects a pending,
// unexpired assignment. Re-delivery of the same transition event is idempotent.
func (s *Store) TransitionPendingAssignment(ctx context.Context, id int, reviewerPubkey, status, eventID string, now int64) error {
	if status != "accepted" && status != "rejected" {
		return fmt.Errorf("unsupported assignment transition status %q", status)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE review_assignments
		SET status = ?, acceptance_event_id = ?, updated_at = ?
		WHERE id = ? AND reviewer_pubkey = ? AND status = 'pending' AND expires_at >= ?
	`, status, eventID, now, id, reviewerPubkey, now)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("assignment transition rows affected: %w", err)
	}
	if affected == 1 {
		return nil
	}

	var assignmentEventID, storedReviewer, storedStatus, storedEventID string
	var expiresAt int64
	err = s.db.QueryRowContext(ctx, `
		SELECT assignment_event_id, reviewer_pubkey, status,
			COALESCE(acceptance_event_id, ''), expires_at
		FROM review_assignments WHERE id = ?
	`, id).Scan(&assignmentEventID, &storedReviewer, &storedStatus, &storedEventID, &expiresAt)
	if err != nil {
		return fmt.Errorf("lookup failed assignment transition: %w", err)
	}
	if storedReviewer != reviewerPubkey {
		return fmt.Errorf("unauthorized reviewer: sender %s is not assigned reviewer %s", reviewerPubkey, storedReviewer)
	}
	if storedStatus == status && storedEventID == eventID {
		return nil
	}
	if storedStatus != "pending" {
		return fmt.Errorf("assignment %s is not pending: %s", assignmentEventID, storedStatus)
	}
	if expiresAt > 0 && expiresAt < now {
		return fmt.Errorf("assignment %s expired", assignmentEventID)
	}
	return fmt.Errorf("assignment %s transition did not apply", assignmentEventID)
}

// ListPendingAssignments returns all pending assignments for a reviewer.
func (s *Store) ListPendingAssignments(ctx context.Context, pubkey string) ([]ReviewAssignment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, patch_event_id, repo_id, reviewer_pubkey, requester_pubkey,
				status, priority, price_sats, assignment_event_id,
				expires_at, created_at, updated_at
		FROM review_assignments
		WHERE reviewer_pubkey = ? AND status = 'pending'
		ORDER BY priority ASC, created_at ASC
	`, pubkey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var assignments []ReviewAssignment
	for rows.Next() {
		var a ReviewAssignment
		if err := rows.Scan(
			&a.ID, &a.PatchEventID, &a.RepoID, &a.ReviewerPubkey, &a.RequesterPubkey,
			&a.Status, &a.Priority, &a.PriceSats, &a.AssignmentEventID,
			&a.ExpiresAt, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, err
		}
		assignments = append(assignments, a)
	}

	return assignments, rows.Err()
}

// ListAssignmentsForPatch returns all assignments for a given patch.
func (s *Store) ListAssignmentsForPatch(ctx context.Context, patchEventID string) ([]ReviewAssignment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, patch_event_id, repo_id, reviewer_pubkey, requester_pubkey,
			status, priority, price_sats, assignment_event_id,
			expires_at, created_at, updated_at
		FROM review_assignments
		WHERE patch_event_id = ?
		ORDER BY created_at DESC
	`, patchEventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var assignments []ReviewAssignment
	for rows.Next() {
		var a ReviewAssignment
		if err := rows.Scan(
			&a.ID, &a.PatchEventID, &a.RepoID, &a.ReviewerPubkey, &a.RequesterPubkey,
			&a.Status, &a.Priority, &a.PriceSats, &a.AssignmentEventID,
			&a.ExpiresAt, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, err
		}
		assignments = append(assignments, a)
	}

	return assignments, rows.Err()
}

// ExpireStaleAssignments marks assignments past their expiry as expired.
func (s *Store) ExpireStaleAssignments(ctx context.Context) (int64, error) {
	now := time.Now().Unix()

	result, err := s.db.ExecContext(ctx, `
		UPDATE review_assignments
		SET status = 'expired', updated_at = ?
		WHERE status = 'pending' AND expires_at < ?
	`, now, now)
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

// RecordFeedback stores feedback on a completed review.
func (s *Store) RecordFeedback(ctx context.Context, fb ReviewFeedback) error {
	now := time.Now().Unix()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO review_feedback (
			assignment_id, reviewer_pubkey, rater_pubkey,
			rating, comment, event_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(assignment_id, rater_pubkey) DO NOTHING
	`,
		fb.AssignmentID, fb.ReviewerPubkey, fb.RaterPubkey,
		fb.Rating, fb.Comment, fb.EventID, now,
	)
	return err
}

// GetReviewerStats retrieves aggregated stats for reputation calculation.
func (s *Store) GetReviewerStats(ctx context.Context, pubkey string) (*ReviewerStats, error) {
	var stats ReviewerStats

	// Count assignments by status
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			SUM(CASE WHEN status = 'accepted' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status = 'rejected' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END),
			COALESCE(MAX(updated_at), 0)
		FROM review_assignments
		WHERE reviewer_pubkey = ?
	`, pubkey).Scan(
		&stats.TotalAssignments,
		&stats.AcceptedAssignments,
		&stats.RejectedAssignments,
		&stats.CompletedReviews,
		&stats.LastReviewAt,
	)
	if err != nil {
		return nil, err
	}

	// Count feedback and total rating
	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(rating), 0)
		FROM review_feedback
		WHERE reviewer_pubkey = ?
	`, pubkey).Scan(&stats.TotalFeedback, &stats.TotalRatingSum)
	if err != nil {
		return nil, err
	}

	return &stats, nil
}

// CountAvailableReviewers returns the number of reviewers with available status.
func (s *Store) CountAvailableReviewers(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reviewer_profiles
		WHERE availability = 'available'
	`).Scan(&count)
	return count, err
}
