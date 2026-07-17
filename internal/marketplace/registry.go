package marketplace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"drydock/internal/db"
	"drydock/internal/metrics"

	"fiatjaf.com/nostr"
)

// Registry tracks registered reviewers and their profiles.
type Registry struct {
	store  *db.Store
	logger *slog.Logger

	// In-memory cache of active reviewers
	mu        sync.RWMutex
	reviewers map[string]*cachedReviewer // keyed by pubkey
}

type cachedReviewer struct {
	Profile    ReviewerProfile
	Reputation ReputationScore
	CachedAt   time.Time
}

const cacheExpiry = 5 * time.Minute

// ReviewerProfileQueryFilter scopes Nostr queries to Drydock reviewer NIP-89 app handlers.
func ReviewerProfileQueryFilter() nostr.Filter {
	return nostr.Filter{
		Kinds: []nostr.Kind{nostr.Kind(KindReviewerProfile)},
		Tags: nostr.TagMap{
			"d": []string{ReviewerProfileDTag},
			"k": []string{ReviewerProfileHandledKind},
		},
	}
}

// NewRegistry creates a reviewer registry.
func NewRegistry(store *db.Store, logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{
		store:     store,
		logger:    logger,
		reviewers: make(map[string]*cachedReviewer),
	}
}

// RegisterReviewer adds or updates a reviewer profile.
func (r *Registry) RegisterReviewer(ctx context.Context, profile ReviewerProfile, eventID string) error {
	if profile.Pubkey == "" {
		return fmt.Errorf("reviewer pubkey is required")
	}

	// Validate languages
	for i, lang := range profile.Languages {
		profile.Languages[i] = strings.ToLower(strings.TrimSpace(lang))
	}

	// Validate domains
	for i, domain := range profile.Domains {
		profile.Domains[i] = strings.ToLower(strings.TrimSpace(domain))
	}

	// Set timestamps
	now := time.Now().Unix()
	if profile.CreatedAt == 0 {
		profile.CreatedAt = now
	}
	profile.UpdatedAt = now

	// Default availability
	if profile.Availability == "" {
		profile.Availability = AvailabilityAvailable
	}

	// Default max concurrent
	if profile.MaxConcurrent == 0 {
		profile.MaxConcurrent = 3
	}

	// Persist to database (convert to DB type)
	dbProfile := db.ReviewerProfile{
		Pubkey:            profile.Pubkey,
		DisplayName:       profile.DisplayName,
		Languages:         profile.Languages,
		Domains:           profile.Domains,
		Availability:      string(profile.Availability),
		PricePerReview:    profile.PricePerReview,
		MaxConcurrent:     profile.MaxConcurrent,
		PayoutDestination: profile.PayoutDestination,
		EventID:           eventID,
		CreatedAt:         profile.CreatedAt,
		UpdatedAt:         profile.UpdatedAt,
	}
	if err := r.store.UpsertReviewerProfile(ctx, dbProfile, eventID); err != nil {
		return fmt.Errorf("upsert reviewer profile: %w", err)
	}

	// Update cache
	r.mu.Lock()
	r.reviewers[profile.Pubkey] = &cachedReviewer{
		Profile:  profile,
		CachedAt: time.Now(),
	}
	r.mu.Unlock()

	r.logger.Info("reviewer registered",
		"pubkey", profile.Pubkey,
		"languages", profile.Languages,
		"domains", profile.Domains,
	)

	return nil
}

// GetReviewer returns a reviewer's profile and reputation.
func (r *Registry) GetReviewer(ctx context.Context, pubkey string) (*ReviewerProfile, *ReputationScore, error) {
	// Check cache first
	r.mu.RLock()
	cached, ok := r.reviewers[pubkey]
	r.mu.RUnlock()

	if ok && time.Since(cached.CachedAt) < cacheExpiry {
		return &cached.Profile, &cached.Reputation, nil
	}

	// Load from database
	dbProfile, err := r.store.GetReviewerProfile(ctx, pubkey)
	if err != nil {
		return nil, nil, err
	}

	dbReputation, err := r.store.GetReviewerReputation(ctx, pubkey)
	if err != nil {
		// Reputation may not exist yet
		dbReputation = &db.ReputationScore{Pubkey: pubkey, OverallScore: 0.5}
	}

	// Convert to marketplace types
	profile := ReviewerProfile{
		Pubkey:            dbProfile.Pubkey,
		DisplayName:       dbProfile.DisplayName,
		Languages:         dbProfile.Languages,
		Domains:           dbProfile.Domains,
		Availability:      AvailabilityLevel(dbProfile.Availability),
		PricePerReview:    dbProfile.PricePerReview,
		MaxConcurrent:     dbProfile.MaxConcurrent,
		PayoutDestination: dbProfile.PayoutDestination,
		CreatedAt:         dbProfile.CreatedAt,
		UpdatedAt:         dbProfile.UpdatedAt,
	}

	reputation := ReputationScore{
		Pubkey:         dbReputation.Pubkey,
		OverallScore:   dbReputation.OverallScore,
		AcceptanceRate: dbReputation.AcceptanceRate,
		AverageRating:  dbReputation.AverageRating,
		TotalReviews:   dbReputation.TotalReviews,
	}

	// Update cache
	r.mu.Lock()
	r.reviewers[pubkey] = &cachedReviewer{
		Profile:    profile,
		Reputation: reputation,
		CachedAt:   time.Now(),
	}
	r.mu.Unlock()

	return &profile, &reputation, nil
}

// FindReviewers finds reviewers matching the given criteria.
func (r *Registry) FindReviewers(ctx context.Context, criteria RoutingCriteria, limit int) ([]MatchedReviewer, error) {
	if limit <= 0 {
		limit = 10
	}

	// Get all available reviewers from database
	dbProfiles, err := r.store.ListAvailableReviewers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list reviewers: %w", err)
	}

	var matches []MatchedReviewer

	for _, dbProfile := range dbProfiles {
		// Convert to marketplace type
		profile := ReviewerProfile{
			Pubkey:            dbProfile.Pubkey,
			DisplayName:       dbProfile.DisplayName,
			Languages:         dbProfile.Languages,
			Domains:           dbProfile.Domains,
			Availability:      AvailabilityLevel(dbProfile.Availability),
			PricePerReview:    dbProfile.PricePerReview,
			MaxConcurrent:     dbProfile.MaxConcurrent,
			PayoutDestination: dbProfile.PayoutDestination,
			CreatedAt:         dbProfile.CreatedAt,
			UpdatedAt:         dbProfile.UpdatedAt,
		}
		// Skip unavailable reviewers
		if profile.Availability == AvailabilityUnavailable {
			continue
		}

		// Check price constraint
		if criteria.MaxPriceSats > 0 && profile.PricePerReview > criteria.MaxPriceSats {
			continue
		}

		// Check preferred pubkeys
		if len(criteria.PreferredPubkeys) > 0 {
			found := false
			for _, pk := range criteria.PreferredPubkeys {
				if pk == profile.Pubkey {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		// Calculate match score
		score := r.calculateMatchScore(profile, criteria)
		if score < 0.1 {
			continue // Too low a match
		}

		// Get reputation
		dbReputation, err := r.store.GetReviewerReputation(ctx, profile.Pubkey)
		if err != nil {
			dbReputation = &db.ReputationScore{Pubkey: profile.Pubkey}
		}
		reputation := ReputationScore{
			Pubkey:         dbReputation.Pubkey,
			OverallScore:   dbReputation.OverallScore,
			AcceptanceRate: dbReputation.AcceptanceRate,
			AverageRating:  dbReputation.AverageRating,
			TotalReviews:   dbReputation.TotalReviews,
		}

		// Check minimum reputation
		if criteria.MinReputation > 0 && reputation.OverallScore < criteria.MinReputation {
			continue
		}

		matches = append(matches, MatchedReviewer{
			Profile:    profile,
			Reputation: reputation,
			MatchScore: score,
		})
	}

	// Sort by match score (descending)
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].MatchScore > matches[j].MatchScore
	})

	// Limit results
	if len(matches) > limit {
		matches = matches[:limit]
	}

	return matches, nil
}

// calculateMatchScore calculates how well a reviewer matches the criteria.
func (r *Registry) calculateMatchScore(profile ReviewerProfile, criteria RoutingCriteria) float64 {
	var score float64

	// Language match (most important)
	langMatch := r.calculateSetOverlap(profile.Languages, criteria.Languages)
	score += langMatch * 0.5

	// Domain match
	if len(criteria.Domains) > 0 {
		domainMatch := r.calculateSetOverlap(profile.Domains, criteria.Domains)
		score += domainMatch * 0.3
	} else {
		score += 0.3 // No domain requirement = full points
	}

	// Availability bonus
	if profile.Availability == AvailabilityAvailable {
		score += 0.1
	} else if profile.Availability == AvailabilityBusy {
		score += 0.05
	}

	// Fast response bonus
	if criteria.RequireFast && profile.ResponseTime != "" {
		if strings.Contains(profile.ResponseTime, "h") {
			// Parse hours
			var hours int
			fmt.Sscanf(profile.ResponseTime, "%dh", &hours)
			if hours <= 1 {
				score += 0.1
			} else if hours <= 4 {
				score += 0.05
			}
		}
	}

	return score
}

// calculateSetOverlap returns the Jaccard index of two string sets.
func (r *Registry) calculateSetOverlap(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}

	setA := make(map[string]bool, len(a))
	for _, s := range a {
		setA[strings.ToLower(s)] = true
	}

	setB := make(map[string]bool, len(b))
	for _, s := range b {
		setB[strings.ToLower(s)] = true
	}

	// Count intersection
	var intersection int
	for s := range setA {
		if setB[s] {
			intersection++
		}
	}

	// Union size
	union := len(setA)
	for s := range setB {
		if !setA[s] {
			union++
		}
	}

	if union == 0 {
		return 0
	}

	return float64(intersection) / float64(union)
}

// UpdateAvailability updates a reviewer's availability status.
func (r *Registry) UpdateAvailability(ctx context.Context, pubkey string, availability AvailabilityLevel) error {
	if err := r.store.UpdateReviewerAvailability(ctx, pubkey, string(availability)); err != nil {
		return err
	}

	// Invalidate cache
	r.mu.Lock()
	delete(r.reviewers, pubkey)
	r.mu.Unlock()

	return nil
}

// RecordAssignment records that a reviewer was assigned a patch.
func (r *Registry) RecordAssignment(ctx context.Context, assignment ReviewAssignment) error {
	dbAssignment := db.ReviewAssignment{
		PatchEventID:      assignment.PatchEventID,
		RepoID:            assignment.RepoID,
		ReviewerPubkey:    assignment.ReviewerPubkey,
		RequesterPubkey:   assignment.RequesterPubkey,
		Status:            "pending",
		Priority:          2,
		PriceSats:         assignment.PriceSats,
		AssignmentEventID: assignment.AssignmentID,
		ExpiresAt:         assignment.Deadline,
	}
	return r.store.CreateAssignment(ctx, dbAssignment)
}

// RecordAcceptance records that a reviewer accepted an assignment.
func (r *Registry) RecordAcceptance(ctx context.Context, acceptance ReviewAcceptance) error {
	assignment, err := r.store.GetAssignmentByEventID(ctx, acceptance.AssignmentID)
	if err != nil {
		return fmt.Errorf("find assignment: %w", err)
	}
	return r.store.TransitionPendingAssignment(ctx, assignment.ID, acceptance.ReviewerPubkey, "accepted", acceptance.EventID, time.Now().Unix())
}

// RecordRejection records that a reviewer rejected an assignment.
func (r *Registry) RecordRejection(ctx context.Context, rejection ReviewRejection) error {
	assignment, err := r.store.GetAssignmentByEventID(ctx, rejection.AssignmentID)
	if err != nil {
		return fmt.Errorf("find assignment: %w", err)
	}
	return r.store.TransitionPendingAssignment(ctx, assignment.ID, rejection.ReviewerPubkey, "rejected", rejection.EventID, time.Now().Unix())
}

// RecordFeedback records feedback on a review and updates reputation.
func (r *Registry) RecordFeedback(ctx context.Context, feedback ReviewFeedback) error {
	if !IsValidRating(feedback.Rating) {
		return fmt.Errorf("invalid rating: %d", feedback.Rating)
	}

	assignment, err := r.feedbackAssignment(ctx, feedback)
	if err != nil {
		return err
	}
	if feedback.ReviewerPubkey == "" {
		feedback.ReviewerPubkey = assignment.ReviewerPubkey
	}
	if err := r.authorizeFeedbackRater(ctx, assignment, feedback.RaterPubkey); err != nil {
		return err
	}
	dbFeedback := db.ReviewFeedback{
		AssignmentID:   assignment.ID,
		ReviewerPubkey: assignment.ReviewerPubkey,
		RaterPubkey:    feedback.RaterPubkey,
		Rating:         feedback.Rating,
		Comment:        feedback.Comment,
		EventID:        feedback.EventID,
	}
	if err := r.store.RecordFeedback(ctx, dbFeedback); err != nil {
		return err
	}

	return r.recalculateReputation(ctx, assignment.ReviewerPubkey)
}

func (r *Registry) feedbackAssignment(ctx context.Context, feedback ReviewFeedback) (*db.ReviewAssignment, error) {
	if feedback.AssignmentID > 0 {
		assignment, err := r.store.GetAssignmentByID(ctx, feedback.AssignmentID)
		if err != nil {
			return nil, fmt.Errorf("find feedback assignment: %w", err)
		}
		return assignment, nil
	}
	if feedback.ReviewEventID == "" {
		return nil, fmt.Errorf("feedback assignment_id or review_event_id is required")
	}
	assignment, err := r.store.GetAssignmentByCompletionEventID(ctx, feedback.ReviewEventID)
	if err != nil {
		return nil, fmt.Errorf("find assignment by review event: %w", err)
	}
	return assignment, nil
}

func (r *Registry) authorizeFeedbackRater(ctx context.Context, assignment *db.ReviewAssignment, raterPubkey string) error {
	if raterPubkey == assignment.RequesterPubkey && raterPubkey != "" {
		return nil
	}
	patchAuthor, err := r.store.GetPatchAuthorPubKey(ctx, assignment.PatchEventID)
	if err == nil && patchAuthor == raterPubkey {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) && assignment.RequesterPubkey == "" {
		return fmt.Errorf("resolve assignment requester: %w", err)
	}
	return fmt.Errorf("unauthorized feedback rater: sender %s is not requester/patch author", raterPubkey)
}

// RecalculateReputation triggers reputation recalculation for a reviewer.
func (r *Registry) RecalculateReputation(ctx context.Context, pubkey string) error {
	return r.recalculateReputation(ctx, pubkey)
}

// recalculateReputation recalculates a reviewer's reputation score.
func (r *Registry) recalculateReputation(ctx context.Context, pubkey string) error {
	stats, err := r.store.GetReviewerStats(ctx, pubkey)
	if err != nil {
		return fmt.Errorf("get reviewer stats: %w", err)
	}

	var acceptanceRate, avgRating float64

	// Calculate acceptance rate
	if stats.TotalAssignments > 0 {
		acceptanceRate = float64(stats.AcceptedAssignments) / float64(stats.TotalAssignments)
	}

	// Calculate average rating
	if stats.TotalFeedback > 0 {
		avgRating = stats.TotalRatingSum / float64(stats.TotalFeedback)
	}

	// Calculate overall score (weighted combination)
	// - 40% acceptance rate
	// - 40% average rating (normalized to 0-1)
	// - 20% volume bonus (diminishing returns)
	volumeBonus := 1.0 - (1.0 / (1.0 + float64(stats.CompletedReviews)/10.0))

	overallScore := (acceptanceRate*0.4 + (avgRating/5.0)*0.4 + volumeBonus*0.2)

	// Save reputation
	dbReputation := db.ReputationScore{
		Pubkey:         pubkey,
		OverallScore:   overallScore,
		TotalReviews:   stats.CompletedReviews,
		AcceptedCount:  stats.AcceptedAssignments,
		RejectedCount:  stats.RejectedAssignments,
		AverageRating:  avgRating,
		AcceptanceRate: acceptanceRate,
		LastReviewAt:   stats.LastReviewAt,
	}
	if err := r.store.UpsertReviewerReputation(ctx, dbReputation); err != nil {
		return fmt.Errorf("upsert reputation: %w", err)
	}

	metrics.MarketplaceReputationUpdates.Inc()

	// Invalidate cache
	r.mu.Lock()
	delete(r.reviewers, pubkey)
	r.mu.Unlock()

	return nil
}

// GetTopReviewers returns the top N reviewers by reputation.
func (r *Registry) GetTopReviewers(ctx context.Context, limit int) ([]MatchedReviewer, error) {
	return r.FindReviewers(ctx, RoutingCriteria{}, limit)
}
