package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"drydock/internal/metrics"

	"fiatjaf.com/nostr"
)

// Signer signs Nostr events.
type Signer interface {
	GetPublicKey(ctx context.Context) (nostr.PubKey, error)
	SignEvent(ctx context.Context, evt *nostr.Event) error
}

// RelayPublisher publishes events to Nostr relays.
type RelayPublisher interface {
	Publish(ctx context.Context, relays []string, event nostr.Event) error
}

// RouterConfig holds router configuration.
type RouterConfig struct {
	DefaultRelays      []string
	MaxReviewersPerPatch int           // Max reviewers to assign per patch
	AssignmentTimeout  time.Duration // How long to wait for acceptance
	DefaultDeadline    time.Duration // Default review deadline
	MinReputation      float64       // Minimum reputation to be assigned
}

// Router assigns patches to appropriate community reviewers.
type Router struct {
	cfg       RouterConfig
	registry  *Registry
	signer    Signer
	publisher RelayPublisher
	logger    *slog.Logger
}

// NewRouter creates a patch router.
func NewRouter(
	cfg RouterConfig,
	registry *Registry,
	signer Signer,
	publisher RelayPublisher,
	logger *slog.Logger,
) *Router {
	if cfg.MaxReviewersPerPatch <= 0 {
		cfg.MaxReviewersPerPatch = 2
	}
	if cfg.AssignmentTimeout == 0 {
		cfg.AssignmentTimeout = DefaultResponseTimeout
	}
	if cfg.DefaultDeadline == 0 {
		cfg.DefaultDeadline = DefaultAssignmentDeadline
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &Router{
		cfg:       cfg,
		registry:  registry,
		signer:    signer,
		publisher: publisher,
		logger:    logger,
	}
}

// PatchInfo contains information about a patch to be routed.
type PatchInfo struct {
	PatchEventID string
	RepoID       string
	AuthorPubkey string
	Diff         string
	ChangedFiles []string
	PriceSats    int64 // Price author is willing to pay
}

// RoutingResult contains the result of routing a patch.
type RoutingResult struct {
	Assignments    []ReviewAssignment
	MatchedCount   int
	AssignedCount  int
	NoMatchReason  string
}

// RoutePatch finds appropriate reviewers and assigns the patch to them.
func (r *Router) RoutePatch(ctx context.Context, patch PatchInfo) (*RoutingResult, error) {
	metrics.MarketplaceRoutingAttempts.Inc()

	// Detect languages from changed files
	languages := r.detectLanguages(patch.ChangedFiles)
	if len(languages) == 0 {
		return &RoutingResult{
			NoMatchReason: "no supported languages detected",
		}, nil
	}

	// Build routing criteria
	criteria := RoutingCriteria{
		Languages:     languages,
		MinReputation: r.cfg.MinReputation,
		MaxPriceSats:  patch.PriceSats,
	}

	// Find matching reviewers
	matches, err := r.registry.FindReviewers(ctx, criteria, r.cfg.MaxReviewersPerPatch*2)
	if err != nil {
		return nil, fmt.Errorf("find reviewers: %w", err)
	}

	if len(matches) == 0 {
		metrics.MarketplaceNoReviewersFound.Inc()
		return &RoutingResult{
			NoMatchReason: fmt.Sprintf("no reviewers available for languages: %v", languages),
		}, nil
	}

	// Filter out the patch author (can't review own patch)
	filtered := make([]MatchedReviewer, 0, len(matches))
	for _, m := range matches {
		if m.Profile.Pubkey != patch.AuthorPubkey {
			filtered = append(filtered, m)
		}
	}
	matches = filtered

	if len(matches) == 0 {
		return &RoutingResult{
			NoMatchReason: "only matching reviewer is patch author",
		}, nil
	}

	// Create assignments for top matches
	result := &RoutingResult{
		MatchedCount: len(matches),
	}

	assignCount := r.cfg.MaxReviewersPerPatch
	if assignCount > len(matches) {
		assignCount = len(matches)
	}

	now := time.Now()
	deadline := now.Add(r.cfg.DefaultDeadline)

	for i := 0; i < assignCount; i++ {
		match := matches[i]

		assignment := ReviewAssignment{
			AssignmentID:   generateAssignmentID(patch.PatchEventID, match.Profile.Pubkey),
			PatchEventID:   patch.PatchEventID,
			RepoID:         patch.RepoID,
			ReviewerPubkey: match.Profile.Pubkey,
			Languages:      languages,
			PriceSats:      match.Profile.PricePerReview,
			Deadline:       deadline.Unix(),
			CreatedAt:      now.Unix(),
		}

		// Publish assignment event
		if err := r.publishAssignment(ctx, assignment); err != nil {
			r.logger.Warn("failed to publish assignment",
				"reviewer", match.Profile.Pubkey,
				"error", err)
			continue
		}

		// Record in database
		if err := r.registry.RecordAssignment(ctx, assignment); err != nil {
			r.logger.Warn("failed to record assignment",
				"assignment_id", assignment.AssignmentID,
				"error", err)
		}

		result.Assignments = append(result.Assignments, assignment)
		result.AssignedCount++
	}

	if result.AssignedCount > 0 {
		metrics.MarketplaceAssignmentsCreated.Add(int64(result.AssignedCount))
	}

	r.logger.Info("patch routed to reviewers",
		"patch_event_id", patch.PatchEventID,
		"languages", languages,
		"matched", len(matches),
		"assigned", result.AssignedCount,
	)

	return result, nil
}

// detectLanguages extracts programming languages from file paths.
func (r *Router) detectLanguages(files []string) []string {
	langSet := make(map[string]bool)

	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		lang := extToLanguage(ext)
		if lang != "" {
			langSet[lang] = true
		}
	}

	languages := make([]string, 0, len(langSet))
	for lang := range langSet {
		languages = append(languages, lang)
	}

	return languages
}

// extToLanguage maps file extensions to language identifiers.
func extToLanguage(ext string) string {
	switch ext {
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".py", ".pyi":
		return "python"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	case ".c", ".h":
		return "c"
	case ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx":
		return "cpp"
	case ".cs":
		return "csharp"
	case ".swift":
		return "swift"
	case ".kt", ".kts":
		return "kotlin"
	case ".scala":
		return "scala"
	case ".php":
		return "php"
	case ".sol":
		return "solidity"
	case ".zig":
		return "zig"
	case ".ex", ".exs":
		return "elixir"
	default:
		return ""
	}
}

// publishAssignment publishes a review assignment event.
func (r *Router) publishAssignment(ctx context.Context, assignment ReviewAssignment) error {
	content, err := json.Marshal(assignment)
	if err != nil {
		return fmt.Errorf("marshal assignment: %w", err)
	}

	event := nostr.Event{
		Kind:      nostr.Kind(KindReviewAssignment),
		CreatedAt: nostr.Now(),
		Content:   string(content),
		Tags: nostr.Tags{
			{"p", assignment.ReviewerPubkey},           // Tag the reviewer
			{"e", assignment.PatchEventID},             // Reference the patch
			{"a", "30617:" + assignment.RepoID},        // Reference the repo
			{"assignment", assignment.AssignmentID},    // Assignment ID
			{"expiration", fmt.Sprintf("%d", assignment.Deadline)},
		},
	}

	if err := r.signer.SignEvent(ctx, &event); err != nil {
		return fmt.Errorf("sign assignment: %w", err)
	}

	if err := r.publisher.Publish(ctx, r.cfg.DefaultRelays, event); err != nil {
		return fmt.Errorf("publish assignment: %w", err)
	}

	return nil
}

// generateAssignmentID creates a deterministic assignment ID.
func generateAssignmentID(patchEventID, reviewerPubkey string) string {
	// Simple combination for now - could be a hash
	return fmt.Sprintf("%s-%s", patchEventID[:16], reviewerPubkey[:16])
}

// HandleAcceptance processes a reviewer accepting an assignment.
func (r *Router) HandleAcceptance(ctx context.Context, event nostr.Event) error {
	var acceptance ReviewAcceptance
	if err := json.Unmarshal([]byte(event.Content), &acceptance); err != nil {
		return fmt.Errorf("parse acceptance: %w", err)
	}

	acceptance.ReviewerPubkey = event.PubKey.Hex()
	acceptance.CreatedAt = int64(event.CreatedAt)

	if err := r.registry.RecordAcceptance(ctx, acceptance); err != nil {
		return fmt.Errorf("record acceptance: %w", err)
	}

	metrics.MarketplaceAssignmentsAccepted.Inc()
	r.logger.Info("reviewer accepted assignment",
		"assignment_id", acceptance.AssignmentID,
		"reviewer", acceptance.ReviewerPubkey,
	)

	return nil
}

// HandleRejection processes a reviewer rejecting an assignment.
func (r *Router) HandleRejection(ctx context.Context, event nostr.Event) error {
	var rejection ReviewRejection
	if err := json.Unmarshal([]byte(event.Content), &rejection); err != nil {
		return fmt.Errorf("parse rejection: %w", err)
	}

	rejection.ReviewerPubkey = event.PubKey.Hex()
	rejection.CreatedAt = int64(event.CreatedAt)

	if err := r.registry.RecordRejection(ctx, rejection); err != nil {
		return fmt.Errorf("record rejection: %w", err)
	}

	metrics.MarketplaceAssignmentsRejected.Inc()
	r.logger.Info("reviewer rejected assignment",
		"assignment_id", rejection.AssignmentID,
		"reviewer", rejection.ReviewerPubkey,
		"reason", rejection.Reason,
	)

	// TODO: Reassign to another reviewer if available

	return nil
}

// HandleFeedback processes feedback on a completed review.
func (r *Router) HandleFeedback(ctx context.Context, event nostr.Event) error {
	var feedback ReviewFeedback
	if err := json.Unmarshal([]byte(event.Content), &feedback); err != nil {
		return fmt.Errorf("parse feedback: %w", err)
	}

	feedback.CreatedAt = int64(event.CreatedAt)

	if err := r.registry.RecordFeedback(ctx, feedback); err != nil {
		return fmt.Errorf("record feedback: %w", err)
	}

	metrics.MarketplaceFeedbackReceived.Inc()
	r.logger.Info("review feedback received",
		"review_event_id", feedback.ReviewEventID,
		"reviewer", feedback.ReviewerPubkey,
		"rating", feedback.Rating,
	)

	return nil
}
