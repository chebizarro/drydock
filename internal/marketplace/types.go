// Package marketplace implements a Nostr-native review marketplace where
// community members can register as specialized reviewers with reputation
// scores. Patches are routed to appropriate reviewers based on language,
// domain, and expertise.
//
// # Nostr Event Kinds
//
//   - kind 31990: Reviewer NIP-89 application handler (d=drydock-reviewer)
//   - kind 25910: Review assignment ContextVM intent (marketplace/assign)
//   - kind 25910: ContextVM JSON-RPC intents for assignment acceptance/rejection
//   - kind 7000:  NIP-90 review feedback (patch author rates the review)
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
	"fmt"
	"strconv"
	"time"

	"fiatjaf.com/nostr"
)

// Event kinds for marketplace.
const (
	KindReviewerProfile = 31990 // NIP-89 application handler
	KindReviewFeedback  = 7000  // NIP-90 feedback (patch author rates the review)

	// Legacy constants retained for compatibility with older tests/helpers.
	// Live marketplace assignment/accept/reject now uses ContextVM kind 25910.
	KindReviewAssignment = 1660
	KindReviewAcceptance = 1661
	KindReviewRejection  = 1662
)

// NIP-90 feedback tags for marketplace review feedback.
const (
	FeedbackStatusSuccess = "success"
	TagReviewFeedback     = "review-feedback"
)

const (
	ReviewerProfileDTag        = "drydock-reviewer"
	ReviewerProfileHandledKind = "25910"

	MethodAssign   = "marketplace/assign"
	MethodAccept   = "marketplace/accept"
	MethodReject   = "marketplace/reject"
	MethodComplete = "marketplace/complete"
)

// ReviewerProfile represents a community reviewer's registration.
// Published as kind 31990 with "d" tag set to ReviewerProfileDTag.
type ReviewerProfile struct {
	Pubkey            string            `json:"pubkey"`
	DisplayName       string            `json:"display_name,omitempty"`
	About             string            `json:"about,omitempty"`
	Languages         []string          `json:"languages"`                    // e.g., ["go", "rust", "python"]
	Domains           []string          `json:"domains"`                      // e.g., ["security", "performance", "api-design"]
	Availability      AvailabilityLevel `json:"availability"`                 // available, busy, unavailable
	PricePerReview    int64             `json:"price_per_review,omitempty"`   // sats, 0 = free
	PayoutDestination string            `json:"payout_destination,omitempty"` // BOLT11 invoice (or future lightning address)
	MaxConcurrent     int               `json:"max_concurrent"`               // max simultaneous reviews
	ResponseTime      string            `json:"response_time,omitempty"`      // e.g., "24h", "1h"
	CreatedAt         int64             `json:"created_at"`
	UpdatedAt         int64             `json:"updated_at"`
}

// AvailabilityLevel indicates reviewer availability.
type AvailabilityLevel string

const (
	AvailabilityAvailable   AvailabilityLevel = "available"
	AvailabilityBusy        AvailabilityLevel = "busy"
	AvailabilityUnavailable AvailabilityLevel = "unavailable"
)

// ReviewAssignment represents Drydock assigning a patch to a reviewer.
// Published as a ContextVM intent (kind 25910, marketplace/assign).
type ReviewAssignment struct {
	AssignmentID    string   `json:"assignment_id"`
	PatchEventID    string   `json:"patch_event_id"`
	RepoID          string   `json:"repo_id"`
	ReviewerPubkey  string   `json:"reviewer_pubkey"`
	RequesterPubkey string   `json:"requester_pubkey,omitempty"`
	Languages       []string `json:"languages"`  // Languages in the patch
	PriceSats       int64    `json:"price_sats"` // Offered price
	Deadline        int64    `json:"deadline"`   // Unix timestamp
	CreatedAt       int64    `json:"created_at"`
}

// ReviewAcceptance represents a reviewer accepting an assignment.
// Sent as ContextVM method marketplace/accept in reply to the assignment.
type ReviewAcceptance struct {
	AssignmentID   string `json:"assignment_id"`
	ReviewerPubkey string `json:"reviewer_pubkey"`
	EstimatedTime  string `json:"estimated_time,omitempty"` // e.g., "2h"
	CreatedAt      int64  `json:"created_at"`
	EventID        string `json:"-"`
}

// ReviewCompletion authenticates delivery of a published review for an assignment.
// Sent as ContextVM method marketplace/complete by the assigned reviewer.
type ReviewCompletion struct {
	AssignmentID  string `json:"assignment_id"`
	ReviewEventID string `json:"review_event_id"`
	CreatedAt     int64  `json:"created_at"`
	EventID       string `json:"-"`
}

// ReviewRejection represents a reviewer declining an assignment.
// Sent as ContextVM method marketplace/reject in reply to the assignment.
type ReviewRejection struct {
	AssignmentID   string `json:"assignment_id"`
	ReviewerPubkey string `json:"reviewer_pubkey"`
	Reason         string `json:"reason,omitempty"` // e.g., "busy", "not my expertise"
	CreatedAt      int64  `json:"created_at"`
	EventID        string `json:"-"`
}

// ReviewFeedback represents a patch author's rating of a review.
// Published as NIP-90 feedback kind 7000 in reply to the review.
type ReviewFeedback struct {
	AssignmentID   int    `json:"assignment_id,omitempty"` // Legacy compatibility; NIP-90 uses review_event_id/e-tag.
	ReviewEventID  string `json:"review_event_id"`
	ReviewerPubkey string `json:"reviewer_pubkey"`
	RaterPubkey    string `json:"rater_pubkey,omitempty"`
	Rating         int    `json:"rating"`   // 1-5 stars
	Helpful        bool   `json:"helpful"`  // Was the review helpful?
	Accurate       bool   `json:"accurate"` // Were findings accurate?
	Comment        string `json:"comment,omitempty"`
	EventID        string `json:"event_id,omitempty"`
	CreatedAt      int64  `json:"created_at"`
}

// ReputationScore holds a reviewer's calculated reputation.
type ReputationScore struct {
	Pubkey           string  `json:"pubkey"`
	OverallScore     float64 `json:"overall_score"`   // 0-100
	AcceptanceRate   float64 `json:"acceptance_rate"` // 0-1
	AverageRating    float64 `json:"average_rating"`  // 1-5
	TotalReviews     int     `json:"total_reviews"`
	TotalAssignments int     `json:"total_assignments"`
	TrustScore       float64 `json:"trust_score"` // Web-of-trust score
	LastActive       int64   `json:"last_active"`
}

// RoutingCriteria specifies what kind of reviewer is needed for a patch.
type RoutingCriteria struct {
	Languages        []string `json:"languages"`
	Domains          []string `json:"domains,omitempty"`
	MinReputation    float64  `json:"min_reputation,omitempty"`    // Minimum overall score
	MaxPriceSats     int64    `json:"max_price_sats,omitempty"`    // Max price willing to pay
	RequireFast      bool     `json:"require_fast,omitempty"`      // Prefer fast response time
	PreferredPubkeys []string `json:"preferred_pubkeys,omitempty"` // Specific reviewers
}

// MatchedReviewer represents a reviewer matched to criteria with a score.
type MatchedReviewer struct {
	Profile    ReviewerProfile `json:"profile"`
	Reputation ReputationScore `json:"reputation"`
	MatchScore float64         `json:"match_score"` // How well they match criteria
}

// ReviewerProfileAppContent is the JSON content stored in the NIP-89 app handler event.
type ReviewerProfileAppContent struct {
	Pubkey        string `json:"pubkey"`
	MaxConcurrent int    `json:"max_concurrent"`
	ResponseTime  string `json:"response_time,omitempty"`
}

// DrydockTag builds a drydock-specific NIP-89 capability tag.
func DrydockTag(name string, values ...string) nostr.Tag {
	return append(nostr.Tag{"drydock:" + name}, values...)
}

// ReviewerProfileTags builds NIP-89 standard and Drydock capability tags for a reviewer.
func ReviewerProfileTags(profile ReviewerProfile) nostr.Tags {
	tags := nostr.Tags{
		{"d", ReviewerProfileDTag},
		{"k", ReviewerProfileHandledKind},
		{"name", profile.DisplayName},
		{"about", profile.About},
		DrydockTag("languages", profile.Languages...),
		DrydockTag("domains", profile.Domains...),
		DrydockTag("availability", string(profile.Availability)),
		DrydockTag("price", strconv.FormatInt(profile.PricePerReview, 10)),
	}
	if profile.PayoutDestination != "" {
		tags = append(tags, DrydockTag("payout", profile.PayoutDestination))
	}
	tags = append(tags, DrydockTag("methods", MethodAssign, MethodAccept, MethodReject, MethodComplete))
	return tags
}

// ReviewerProfileContent builds the NIP-89 content for a reviewer profile event.
func ReviewerProfileContent(profile ReviewerProfile) (string, error) {
	content, err := json.Marshal(ReviewerProfileAppContent{
		Pubkey:        profile.Pubkey,
		MaxConcurrent: profile.MaxConcurrent,
		ResponseTime:  profile.ResponseTime,
	})
	return string(content), err
}

// ReviewerProfileEvent builds an unsigned NIP-89 app handler event for a reviewer.
func ReviewerProfileEvent(profile ReviewerProfile) (nostr.Event, error) {
	content, err := ReviewerProfileContent(profile)
	if err != nil {
		return nostr.Event{}, err
	}
	return nostr.Event{
		Kind:      nostr.Kind(KindReviewerProfile),
		CreatedAt: nostr.Now(),
		Content:   content,
		Tags:      ReviewerProfileTags(profile),
	}, nil
}

// ParseReviewerProfile parses a ReviewerProfile from event content.
func ParseReviewerProfile(content string) (ReviewerProfile, error) {
	var profile ReviewerProfile
	err := json.Unmarshal([]byte(content), &profile)
	return profile, err
}

// ParseReviewerProfileEvent parses a ReviewerProfile from a Drydock NIP-89 app handler event.
func ParseReviewerProfileEvent(event nostr.Event) (ReviewerProfile, bool, error) {
	if int(event.Kind) != KindReviewerProfile {
		return ReviewerProfile{}, false, nil
	}

	var content ReviewerProfileAppContent
	if err := json.Unmarshal([]byte(event.Content), &content); err != nil {
		return ReviewerProfile{}, false, err
	}

	profile := ReviewerProfile{
		Pubkey:        content.Pubkey,
		MaxConcurrent: content.MaxConcurrent,
		ResponseTime:  content.ResponseTime,
	}

	var hasDTag, hasHandledKind bool
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		switch tag[0] {
		case "d":
			hasDTag = tag[1] == ReviewerProfileDTag
		case "k":
			if tag[1] == ReviewerProfileHandledKind {
				hasHandledKind = true
			}
		case "name":
			profile.DisplayName = tag[1]
		case "about":
			profile.About = tag[1]
		case "drydock:languages":
			profile.Languages = append([]string(nil), tag[1:]...)
		case "drydock:domains":
			profile.Domains = append([]string(nil), tag[1:]...)
		case "drydock:availability":
			profile.Availability = AvailabilityLevel(tag[1])
		case "drydock:payout":
			profile.PayoutDestination = tag[1]
		case "drydock:price":
			price, err := strconv.ParseInt(tag[1], 10, 64)
			if err != nil {
				return ReviewerProfile{}, false, err
			}
			profile.PricePerReview = price
		}
	}

	if !hasDTag || !hasHandledKind {
		return ReviewerProfile{}, false, nil
	}
	return profile, true, nil
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

// ParseReviewFeedbackEvent parses NIP-90 review feedback from a kind 7000 event.
func ParseReviewFeedbackEvent(event nostr.Event) (ReviewFeedback, error) {
	if event.Kind != KindReviewFeedback {
		return ReviewFeedback{}, fmt.Errorf("unexpected feedback kind: %d", event.Kind)
	}
	if tagValue(event.Tags, "t") != TagReviewFeedback {
		return ReviewFeedback{}, fmt.Errorf("missing review feedback t tag")
	}
	if status := tagValue(event.Tags, "status"); status != "" && status != FeedbackStatusSuccess {
		return ReviewFeedback{}, fmt.Errorf("unsupported feedback status: %s", status)
	}

	feedback, err := ParseReviewFeedback(event.Content)
	if err != nil {
		return ReviewFeedback{}, err
	}

	feedback.ReviewEventID = tagValue(event.Tags, "e")
	feedback.ReviewerPubkey = tagValue(event.Tags, "p")
	feedback.RaterPubkey = event.PubKey.Hex()
	feedback.EventID = event.ID.Hex()
	feedback.CreatedAt = int64(event.CreatedAt)

	ratingTag := tagValue(event.Tags, "rating")
	if ratingTag == "" {
		return ReviewFeedback{}, fmt.Errorf("missing rating tag")
	}
	feedback.Rating, err = strconv.Atoi(ratingTag)
	if err != nil {
		return ReviewFeedback{}, fmt.Errorf("parse rating tag: %w", err)
	}
	if !IsValidRating(feedback.Rating) {
		return ReviewFeedback{}, fmt.Errorf("invalid rating: %d", feedback.Rating)
	}

	return feedback, nil
}

func tagValue(tags nostr.Tags, name string) string {
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == name {
			return tag[1]
		}
	}
	return ""
}

// ReviewFeedbackTags builds the required NIP-90 tags for review feedback.
func ReviewFeedbackTags(reviewEventID, reviewerPubkey string, rating int) nostr.Tags {
	return nostr.Tags{
		nostr.Tag{"e", reviewEventID},
		nostr.Tag{"p", reviewerPubkey},
		nostr.Tag{"status", FeedbackStatusSuccess},
		nostr.Tag{"rating", strconv.Itoa(rating)},
		nostr.Tag{"t", TagReviewFeedback},
	}
}

// IsValidRating checks if a rating is in the valid range.
func IsValidRating(rating int) bool {
	return rating >= 1 && rating <= 5
}

// DefaultResponseTimeout is how long to wait for a reviewer to respond.
const DefaultResponseTimeout = 2 * time.Hour

// DefaultAssignmentDeadline is the default deadline for completing a review.
const DefaultAssignmentDeadline = 24 * time.Hour
