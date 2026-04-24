// Package marketplace implements a Nostr-native review marketplace where
// community members can register as specialized reviewers with reputation
// scores. Patches are routed to appropriate reviewers based on language,
// domain, and expertise.
//
// # Nostr Event Kinds
//
//   - kind 30620: Reviewer profile (addressable, d=pubkey)
//   - kind 1660:  Review assignment (Drydock assigns patch to reviewer)
//   - kind 1661:  Review acceptance (reviewer accepts assignment)
//   - kind 1662:  Review rejection (reviewer declines assignment)
//   - kind 1663:  Review feedback (patch author rates the review)
//
// # Reputation Model
//
// Reputation is calculated from:
//   - Review acceptance rate (accepted / assigned)
//   - Review feedback scores (from patch authors)
//   - Web-of-trust endorsements (kind 3 follow lists)
//   - Review volume (diminishing returns)
package marketplace

import (
	"encoding/json"
	"time"
)

// Event kinds for marketplace.
const (
	KindReviewerProfile   = 30620 // Addressable reviewer profile
	KindReviewAssignment  = 1660  // Drydock assigns patch to reviewer
	KindReviewAcceptance  = 1661  // Reviewer accepts assignment
	KindReviewRejection   = 1662  // Reviewer declines assignment
	KindReviewFeedback    = 1663  // Patch author rates the review
)

// ReviewerProfile represents a community reviewer's registration.
// Published as kind 30620 with "d" tag set to the reviewer's pubkey.
type ReviewerProfile struct {
	Pubkey        string            `json:"pubkey"`
	DisplayName   string            `json:"display_name,omitempty"`
	About         string            `json:"about,omitempty"`
	Languages     []string          `json:"languages"`      // e.g., ["go", "rust", "python"]
	Domains       []string          `json:"domains"`        // e.g., ["security", "performance", "api-design"]
	Availability  AvailabilityLevel `json:"availability"`   // available, busy, unavailable
	PricePerReview int64            `json:"price_per_review,omitempty"` // sats, 0 = free
	MaxConcurrent int              `json:"max_concurrent"` // max simultaneous reviews
	ResponseTime  string            `json:"response_time,omitempty"` // e.g., "24h", "1h"
	CreatedAt     int64             `json:"created_at"`
	UpdatedAt     int64             `json:"updated_at"`
}

// AvailabilityLevel indicates reviewer availability.
type AvailabilityLevel string

const (
	AvailabilityAvailable   AvailabilityLevel = "available"
	AvailabilityBusy        AvailabilityLevel = "busy"
	AvailabilityUnavailable AvailabilityLevel = "unavailable"
)

// ReviewAssignment represents Drydock assigning a patch to a reviewer.
// Published as kind 1660.
type ReviewAssignment struct {
	AssignmentID  string   `json:"assignment_id"`
	PatchEventID  string   `json:"patch_event_id"`
	RepoID        string   `json:"repo_id"`
	ReviewerPubkey string  `json:"reviewer_pubkey"`
	Languages     []string `json:"languages"`    // Languages in the patch
	PriceSats     int64    `json:"price_sats"`   // Offered price
	Deadline      int64    `json:"deadline"`     // Unix timestamp
	CreatedAt     int64    `json:"created_at"`
}

// ReviewAcceptance represents a reviewer accepting an assignment.
// Published as kind 1661 in reply to the assignment.
type ReviewAcceptance struct {
	AssignmentID   string `json:"assignment_id"`
	ReviewerPubkey string `json:"reviewer_pubkey"`
	EstimatedTime  string `json:"estimated_time,omitempty"` // e.g., "2h"
	CreatedAt      int64  `json:"created_at"`
}

// ReviewRejection represents a reviewer declining an assignment.
// Published as kind 1662 in reply to the assignment.
type ReviewRejection struct {
	AssignmentID   string `json:"assignment_id"`
	ReviewerPubkey string `json:"reviewer_pubkey"`
	Reason         string `json:"reason,omitempty"` // e.g., "busy", "not my expertise"
	CreatedAt      int64  `json:"created_at"`
}

// ReviewFeedback represents a patch author's rating of a review.
// Published as kind 1663 in reply to the review.
type ReviewFeedback struct {
	ReviewEventID  string `json:"review_event_id"`
	ReviewerPubkey string `json:"reviewer_pubkey"`
	Rating         int    `json:"rating"`        // 1-5 stars
	Helpful        bool   `json:"helpful"`       // Was the review helpful?
	Accurate       bool   `json:"accurate"`      // Were findings accurate?
	Comment        string `json:"comment,omitempty"`
	CreatedAt      int64  `json:"created_at"`
}

// ReputationScore holds a reviewer's calculated reputation.
type ReputationScore struct {
	Pubkey           string  `json:"pubkey"`
	OverallScore     float64 `json:"overall_score"`     // 0-100
	AcceptanceRate   float64 `json:"acceptance_rate"`   // 0-1
	AverageRating    float64 `json:"average_rating"`    // 1-5
	TotalReviews     int     `json:"total_reviews"`
	TotalAssignments int     `json:"total_assignments"`
	TrustScore       float64 `json:"trust_score"`       // Web-of-trust score
	LastActive       int64   `json:"last_active"`
}

// RoutingCriteria specifies what kind of reviewer is needed for a patch.
type RoutingCriteria struct {
	Languages       []string `json:"languages"`
	Domains         []string `json:"domains,omitempty"`
	MinReputation   float64  `json:"min_reputation,omitempty"`   // Minimum overall score
	MaxPriceSats    int64    `json:"max_price_sats,omitempty"`   // Max price willing to pay
	RequireFast     bool     `json:"require_fast,omitempty"`     // Prefer fast response time
	PreferredPubkeys []string `json:"preferred_pubkeys,omitempty"` // Specific reviewers
}

// MatchedReviewer represents a reviewer matched to criteria with a score.
type MatchedReviewer struct {
	Profile    ReviewerProfile `json:"profile"`
	Reputation ReputationScore `json:"reputation"`
	MatchScore float64         `json:"match_score"` // How well they match criteria
}

// ParseReviewerProfile parses a ReviewerProfile from event content.
func ParseReviewerProfile(content string) (ReviewerProfile, error) {
	var profile ReviewerProfile
	err := json.Unmarshal([]byte(content), &profile)
	return profile, err
}

// ParseReviewAssignment parses a ReviewAssignment from event content.
func ParseReviewAssignment(content string) (ReviewAssignment, error) {
	var assignment ReviewAssignment
	err := json.Unmarshal([]byte(content), &assignment)
	return assignment, err
}

// ParseReviewFeedback parses a ReviewFeedback from event content.
func ParseReviewFeedback(content string) (ReviewFeedback, error) {
	var feedback ReviewFeedback
	err := json.Unmarshal([]byte(content), &feedback)
	return feedback, err
}

// IsValidRating checks if a rating is in the valid range.
func IsValidRating(rating int) bool {
	return rating >= 1 && rating <= 5
}

// DefaultResponseTimeout is how long to wait for a reviewer to respond.
const DefaultResponseTimeout = 2 * time.Hour

// DefaultAssignmentDeadline is the default deadline for completing a review.
const DefaultAssignmentDeadline = 24 * time.Hour
