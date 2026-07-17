package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"drydock/internal/db"
	"drydock/internal/metrics"
	"drydock/internal/payment"

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

// ContextVMTransport publishes ContextVM intents to Nostr relays.
type PayoutExecutor interface {
	SubmitPayout(ctx context.Context, destination string, amountSats int64, idempotencyKey string) (payment.PayoutEvidence, error)
	ReconcilePayout(ctx context.Context, destination string, amountSats int64) (payment.PayoutEvidence, error)
}

type ContextVMTransport interface {
	SendWithID(ctx context.Context, id, method string, params any, recipients ...nostr.PubKey) (string, error)
}

// RouterConfig holds router configuration.
type RouterConfig struct {
	DefaultRelays        []string
	MaxReviewersPerPatch int           // Max reviewers to assign per patch
	AssignmentTimeout    time.Duration // How long to wait for acceptance
	DefaultDeadline      time.Duration // Default review deadline
	MinReputation        float64       // Minimum reputation to be assigned
}

// Router assigns patches to appropriate community reviewers.
type Router struct {
	cfg                RouterConfig
	registry           *Registry
	store              *db.Store
	signer             Signer
	publisher          RelayPublisher
	contextVMTransport ContextVMTransport
	payoutExecutor     PayoutExecutor
	logger             *slog.Logger
}

// NewRouter creates a patch router.
func NewRouter(
	cfg RouterConfig,
	registry *Registry,
	store *db.Store,
	signer Signer,
	publisher RelayPublisher,
	args ...any,
) *Router {
	var contextVMTransport ContextVMTransport
	var payoutExecutor PayoutExecutor
	var logger *slog.Logger
	for _, arg := range args {
		switch v := arg.(type) {
		case ContextVMTransport:
			contextVMTransport = v
		case PayoutExecutor:
			payoutExecutor = v
		case *slog.Logger:
			logger = v
		case nil:
			// ignore
		}
	}
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
		cfg:                cfg,
		registry:           registry,
		store:              store,
		signer:             signer,
		publisher:          publisher,
		contextVMTransport: contextVMTransport,
		payoutExecutor:     payoutExecutor,
		logger:             logger,
	}
}

// AuthorityPubkey returns the configured Drydock service/router authority pubkey.
func (r *Router) AuthorityPubkey(ctx context.Context) (string, error) {
	if r == nil || r.signer == nil {
		return "", fmt.Errorf("marketplace router authority signer is not configured")
	}
	pubkey, err := r.signer.GetPublicKey(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve marketplace router authority pubkey: %w", err)
	}
	return pubkey.Hex(), nil
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
	Assignments   []ReviewAssignment
	MatchedCount  int
	AssignedCount int
	NoMatchReason string
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

	for _, match := range matches {
		if result.AssignedCount >= assignCount {
			break
		}
		if match.Profile.PricePerReview > patch.PriceSats {
			r.logger.Warn("reviewer price exceeds patch price cap",
				"reviewer", match.Profile.Pubkey,
				"reviewer_price_sats", match.Profile.PricePerReview,
				"patch_price_sats", patch.PriceSats,
			)
			continue
		}

		assignment := ReviewAssignment{
			AssignmentID:    generateAssignmentID(patch.PatchEventID, match.Profile.Pubkey),
			PatchEventID:    patch.PatchEventID,
			RepoID:          patch.RepoID,
			ReviewerPubkey:  match.Profile.Pubkey,
			RequesterPubkey: patch.AuthorPubkey,
			Languages:       languages,
			PriceSats:       match.Profile.PricePerReview,
			Deadline:        deadline.Unix(),
			CreatedAt:       now.Unix(),
		}

		// Record durably before publishing/acking success. The DB row is the
		// source of truth for later acceptance, rejection, expiry, and payout
		// tracking, so a persistence failure is an assignment failure.
		if err := r.registry.RecordAssignment(ctx, assignment); err != nil {
			r.logger.Warn("failed to record assignment",
				"assignment_id", assignment.AssignmentID,
				"error", err)
			return result, fmt.Errorf("record assignment %s: %w", assignment.AssignmentID, err)
		}

		// Publish assignment intent after the durable record exists.
		if err := r.publishAssignment(ctx, assignment); err != nil {
			r.logger.Warn("failed to publish assignment",
				"reviewer", match.Profile.Pubkey,
				"assignment_id", assignment.AssignmentID,
				"error", err)
			continue
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

// publishAssignment publishes a review assignment ContextVM intent.
func (r *Router) publishAssignment(ctx context.Context, assignment ReviewAssignment) error {
	if r.contextVMTransport == nil {
		return fmt.Errorf("contextvm transport not configured")
	}
	reviewerPubkey, err := nostr.PubKeyFromHex(assignment.ReviewerPubkey)
	if err != nil {
		return fmt.Errorf("parse reviewer pubkey: %w", err)
	}

	if _, err := r.contextVMTransport.SendWithID(ctx, assignment.AssignmentID, MethodAssign, assignment, reviewerPubkey); err != nil {
		return fmt.Errorf("publish assignment intent: %w", err)
	}

	return nil
}

// generateAssignmentID creates a deterministic assignment ID.
func generateAssignmentID(patchEventID, reviewerPubkey string) string {
	// Use safe substring with min length
	patchPrefix := patchEventID
	if len(patchPrefix) > 16 {
		patchPrefix = patchPrefix[:16]
	}
	reviewerPrefix := reviewerPubkey
	if len(reviewerPrefix) > 16 {
		reviewerPrefix = reviewerPrefix[:16]
	}
	return fmt.Sprintf("%s-%s", patchPrefix, reviewerPrefix)
}

// HandleAcceptance processes a reviewer accepting an assignment.
func (r *Router) HandleAcceptance(ctx context.Context, event nostr.Event) error {
	var acceptance ReviewAcceptance
	if err := json.Unmarshal([]byte(event.Content), &acceptance); err != nil {
		return fmt.Errorf("parse acceptance: %w", err)
	}

	if event.PubKey != nostr.ZeroPK {
		acceptance.ReviewerPubkey = event.PubKey.Hex()
	}
	acceptance.CreatedAt = int64(event.CreatedAt)
	acceptance.EventID = event.ID.Hex()

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

// HandleCompletion processes a signed completion event from the assigned reviewer.
func (r *Router) HandleCompletion(ctx context.Context, event nostr.Event) error {
	if event.PubKey == nostr.ZeroPK || !event.CheckID() || !event.VerifySignature() {
		return fmt.Errorf("completion event failed signature verification")
	}
	var completion ReviewCompletion
	if err := json.Unmarshal([]byte(event.Content), &completion); err != nil {
		return fmt.Errorf("parse completion: %w", err)
	}
	return r.complete(ctx, completion, event.PubKey.Hex(), event.ID.Hex())
}

func (r *Router) complete(ctx context.Context, completion ReviewCompletion, reviewerPubkey, completionEventID string) error {
	rec, hasPayout, err := r.store.CompleteAssignmentAndAllocatePayout(
		ctx, completion.AssignmentID, reviewerPubkey, completionEventID,
		completion.ReviewEventID, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("record completion: %w", err)
	}
	if !hasPayout {
		return nil
	}
	if r.payoutExecutor == nil {
		return errors.New("review completed but payout executor is not configured")
	}
	return r.executePayout(ctx, rec)
}

func (r *Router) executePayout(ctx context.Context, rec db.MarketplacePayoutRecord) error {
	if rec.Status == "settled" || rec.Status == "failed" {
		return nil
	}

	submit := false
	if rec.Status == "pending" {
		claimed, err := r.store.MarkMarketplacePayoutSubmitted(ctx, rec.AssignmentID, time.Now().Unix())
		if err != nil {
			return fmt.Errorf("persist payout submission: %w", err)
		}
		submit = claimed
		rec, err = r.store.GetMarketplacePayout(ctx, rec.AssignmentID)
		if err != nil {
			return fmt.Errorf("reload payout: %w", err)
		}
		if rec.Status == "settled" || rec.Status == "failed" {
			return nil
		}
	}
	var evidence payment.PayoutEvidence
	var err error
	if submit {
		evidence, err = r.payoutExecutor.SubmitPayout(ctx, rec.Destination, rec.AmountSats, rec.IdempotencyKey)
		if err != nil && payment.PayoutMayHaveSubmitted(err) {
			return fmt.Errorf("payout submission outcome is ambiguous: %w", err)
		}
	} else {
		evidence, err = r.payoutExecutor.ReconcilePayout(ctx, rec.Destination, rec.AmountSats)
		if err != nil {
			// Lookup errors are not definitive payment failures: the original
			// submission may still be in flight or the wallet may be unavailable.
			return fmt.Errorf("payout reconciliation is inconclusive: %w", err)
		}
	}
	if err != nil {
		if markErr := r.store.MarkMarketplacePayoutFailed(ctx, rec.AssignmentID, err.Error(), time.Now().Unix()); markErr != nil {
			return errors.Join(err, fmt.Errorf("persist payout failure: %w", markErr))
		}
		return fmt.Errorf("payout failed: %w", err)
	}
	if evidence.Failed {
		return r.store.MarkMarketplacePayoutFailed(ctx, rec.AssignmentID, "wallet reports payment failed", time.Now().Unix())
	}
	if !evidence.Settled {
		return nil
	}
	if evidence.PaymentHash == "" || evidence.Preimage == "" || evidence.SettledAt <= 0 {
		return errors.New("wallet returned incomplete payout settlement evidence")
	}
	if err := r.store.MarkMarketplacePayoutSettled(ctx, rec.AssignmentID, evidence.PaymentHash, evidence.Preimage, evidence.SettledAt, time.Now().Unix()); err != nil {
		return fmt.Errorf("persist payout settlement: %w", err)
	}
	return nil
}

// HandleRejection processes a reviewer rejecting an assignment.
func (r *Router) HandleRejection(ctx context.Context, event nostr.Event) error {
	var rejection ReviewRejection
	if err := json.Unmarshal([]byte(event.Content), &rejection); err != nil {
		return fmt.Errorf("parse rejection: %w", err)
	}

	if event.PubKey != nostr.ZeroPK {
		rejection.ReviewerPubkey = event.PubKey.Hex()
	}
	rejection.CreatedAt = int64(event.CreatedAt)
	rejection.EventID = event.ID.Hex()

	if err := r.registry.RecordRejection(ctx, rejection); err != nil {
		return fmt.Errorf("record rejection: %w", err)
	}

	metrics.MarketplaceAssignmentsRejected.Inc()
	r.logger.Info("reviewer rejected assignment",
		"assignment_id", rejection.AssignmentID,
		"reviewer", rejection.ReviewerPubkey,
		"reason", rejection.Reason,
	)

	// Attempt to reassign to another reviewer
	if err := r.attemptReassignment(ctx, rejection.AssignmentID); err != nil {
		r.logger.Warn("reassignment failed",
			"assignment_id", rejection.AssignmentID,
			"error", err,
		)
		// Don't return error - rejection was already recorded successfully
	}

	return nil
}

// attemptReassignment tries to assign a patch to another reviewer after rejection.
func (r *Router) attemptReassignment(ctx context.Context, originalAssignmentID string) error {
	if r.store == nil {
		return fmt.Errorf("store not configured for reassignment")
	}

	// Look up the original assignment
	original, err := r.store.GetAssignmentByEventID(ctx, originalAssignmentID)
	if err != nil {
		return fmt.Errorf("lookup original assignment: %w", err)
	}

	// Get all existing assignments for this patch to build exclusion list
	existingAssignments, err := r.store.ListAssignmentsForPatch(ctx, original.PatchEventID)
	if err != nil {
		return fmt.Errorf("list existing assignments: %w", err)
	}

	// Build set of reviewers to exclude (already assigned, rejected, or completed)
	excludePubkeys := make(map[string]bool)
	for _, a := range existingAssignments {
		excludePubkeys[a.ReviewerPubkey] = true
	}

	// Find new candidate reviewers
	// We need to detect languages from the patch - but we don't have changed files here
	// So we'll use a broad search and rely on prior assignment info
	criteria := RoutingCriteria{
		MinReputation: r.cfg.MinReputation,
		MaxPriceSats:  original.PriceSats,
	}

	matches, err := r.registry.FindReviewers(ctx, criteria, 10)
	if err != nil {
		return fmt.Errorf("find reviewers: %w", err)
	}

	// Filter out excluded reviewers and patch author
	var candidates []MatchedReviewer
	for _, m := range matches {
		if !excludePubkeys[m.Profile.Pubkey] && m.Profile.Pubkey != original.RequesterPubkey {
			candidates = append(candidates, m)
		}
	}

	if len(candidates) == 0 {
		r.logger.Info("no alternative reviewers available for reassignment",
			"patch_event_id", original.PatchEventID,
			"excluded_count", len(excludePubkeys),
		)
		return nil // Not an error, just no alternatives
	}

	// Pick the best candidate
	candidate := candidates[0]

	// Create new assignment
	now := time.Now()
	deadline := now.Add(r.cfg.DefaultDeadline)

	newAssignment := ReviewAssignment{
		AssignmentID:    generateAssignmentID(original.PatchEventID, candidate.Profile.Pubkey),
		PatchEventID:    original.PatchEventID,
		RepoID:          original.RepoID,
		ReviewerPubkey:  candidate.Profile.Pubkey,
		RequesterPubkey: original.RequesterPubkey,
		PriceSats:       candidate.Profile.PricePerReview,
		Deadline:        deadline.Unix(),
		CreatedAt:       now.Unix(),
	}

	// Publish assignment event
	if err := r.publishAssignment(ctx, newAssignment); err != nil {
		return fmt.Errorf("publish reassignment: %w", err)
	}

	// Record in database
	if err := r.registry.RecordAssignment(ctx, newAssignment); err != nil {
		return fmt.Errorf("record reassignment: %w", err)
	}

	metrics.MarketplaceAssignmentsCreated.Inc()
	r.logger.Info("patch reassigned to new reviewer",
		"patch_event_id", original.PatchEventID,
		"new_reviewer", candidate.Profile.Pubkey,
		"rejected_by", original.ReviewerPubkey,
	)

	return nil
}

// HandleFeedback processes feedback on a completed review.
func (r *Router) HandleFeedback(ctx context.Context, event nostr.Event) error {
	feedback, err := ParseReviewFeedbackEvent(event)
	if err != nil {
		return fmt.Errorf("parse feedback: %w", err)
	}

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
