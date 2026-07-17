package promptrefine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"drydock/internal/db"
	"drydock/internal/reviewengine"
)

const (
	// DefaultThreshold is the number of unconsumed prompt gaps that must
	// accumulate before an automated refinement cycle is triggered.
	DefaultThreshold = 20

	// PromptNameReviewerSystem identifies the reviewer system prompt.
	PromptNameReviewerSystem = "reviewer_system"
)

// LLMClient abstracts the LLM call needed for prompt refinement.
type LLMClient interface {
	ChatCompletion(ctx context.Context, req reviewengine.ChatRequest) (string, error)
}

// Config controls the prompt refinement service.
type Config struct {
	// Threshold is the minimum number of unconsumed prompt gaps before
	// triggering a refinement cycle. Defaults to DefaultThreshold.
	Threshold int

	// Endpoint is the model endpoint used for the refinement LLM call.
	Endpoint reviewengine.ModelEndpoint

	// EvalScoreFloor is the minimum eval recall required to keep a new
	// prompt version active. If the eval score after activation falls
	// below the previous version's score minus this tolerance, the
	// version is rolled back. A value of 0 means any regression triggers
	// rollback.
	EvalScoreTolerance float64
}

// RefineResult describes the outcome of a refinement cycle.
type RefineResult struct {
	Triggered      bool
	GapsProcessed  int
	NewVersionID   int64
	Activated      bool
	RolledBack     bool
	RollbackReason string
}

type promptStore interface {
	CountUnconsumedPromptGaps(context.Context) (int64, error)
	FetchUnconsumedPromptGaps(context.Context, int) ([]db.PromptGapRecord, error)
	GetActivePromptVersion(context.Context, string) (db.PromptVersion, error)
	InsertPromptVersion(context.Context, string, string, int, string) (int64, error)
	MarkPromptGapsConsumed(context.Context, []int64) error
	ActivatePromptVersion(context.Context, int64) error
	GetLatestEvalRecall(context.Context) (float64, error)
	SetPromptVersionEvalScore(context.Context, int64, float64) error
	RollbackPromptVersion(context.Context, int64) error
	GetPromptVersionByNumber(context.Context, string, int) (db.PromptVersion, error)
}

// Service implements the automated prompt refinement loop.
type Service struct {
	cfg    Config
	store  promptStore
	client LLMClient
	logger *slog.Logger
}

// New creates a prompt refinement service.
func New(cfg Config, store *db.Store, client LLMClient, logger *slog.Logger) *Service {
	if cfg.Threshold <= 0 {
		cfg.Threshold = DefaultThreshold
	}
	return &Service{
		cfg:    cfg,
		store:  store,
		client: client,
		logger: logger,
	}
}

// CheckAndRefine examines the prompt gap queue. If the number of unconsumed
// gaps has reached the configured threshold, it batches them, asks the LLM to
// produce a refined reviewer system prompt, stores the new version, and
// activates it.
func (s *Service) CheckAndRefine(ctx context.Context) (RefineResult, error) {
	count, err := s.store.CountUnconsumedPromptGaps(ctx)
	if err != nil {
		return RefineResult{}, fmt.Errorf("count prompt gaps: %w", err)
	}
	if count < int64(s.cfg.Threshold) {
		return RefineResult{Triggered: false}, nil
	}

	gaps, err := s.store.FetchUnconsumedPromptGaps(ctx, s.cfg.Threshold)
	if err != nil {
		return RefineResult{}, fmt.Errorf("fetch prompt gaps: %w", err)
	}
	if len(gaps) == 0 {
		return RefineResult{Triggered: false}, nil
	}

	// Determine the current active prompt content and version number.
	currentContent := defaultReviewerPrompt()
	parentVersion := 0
	active, err := s.store.GetActivePromptVersion(ctx, PromptNameReviewerSystem)
	if err == nil {
		currentContent = active.Content
		parentVersion = active.Version
	} else if !errors.Is(err, sql.ErrNoRows) {
		return RefineResult{}, fmt.Errorf("get active prompt version: %w", err)
	}
	// If no active version exists, use the default.

	// Build the refinement request.
	gapTexts := make([]string, len(gaps))
	gapIDs := make([]int64, len(gaps))
	for i, g := range gaps {
		gapTexts[i] = g.GapText
		gapIDs[i] = g.ID
	}

	refined, err := s.callRefinementLLM(ctx, currentContent, gapTexts)
	if err != nil {
		return RefineResult{}, fmt.Errorf("refinement LLM call: %w", err)
	}

	// Store the gap IDs as CSV for traceability.
	idStrs := make([]string, len(gapIDs))
	for i, id := range gapIDs {
		idStrs[i] = fmt.Sprintf("%d", id)
	}
	sourceGapCSV := strings.Join(idStrs, ",")

	versionID, err := s.store.InsertPromptVersion(ctx, PromptNameReviewerSystem, refined, parentVersion, sourceGapCSV)
	if err != nil {
		return RefineResult{}, fmt.Errorf("insert prompt version: %w", err)
	}

	if err := s.store.MarkPromptGapsConsumed(ctx, gapIDs); err != nil {
		return RefineResult{}, fmt.Errorf("mark gaps consumed: %w", err)
	}

	// Activate the new version immediately. The eval-and-rollback check
	// runs after the next eval cycle to decide whether to keep it.
	if err := s.store.ActivatePromptVersion(ctx, versionID); err != nil {
		return RefineResult{}, fmt.Errorf("activate prompt version: %w", err)
	}

	s.logger.Info("prompt refinement completed",
		"gaps_processed", len(gaps),
		"new_version_id", versionID,
		"parent_version", parentVersion,
	)

	return RefineResult{
		Triggered:     true,
		GapsProcessed: len(gaps),
		NewVersionID:  versionID,
		Activated:     true,
	}, nil
}

// EvalAndMaybeRollback checks whether the latest eval score regressed after a
// prompt version change. If so, it rolls back to the parent version.
//
// Call this after each eval run completes.
func (s *Service) EvalAndMaybeRollback(ctx context.Context) (RefineResult, error) {
	active, err := s.store.GetActivePromptVersion(ctx, PromptNameReviewerSystem)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No active version — nothing to roll back.
			return RefineResult{}, nil
		}
		return RefineResult{}, fmt.Errorf("get active prompt version: %w", err)
	}
	if active.EvalScore != nil {
		// Already evaluated — no action needed.
		return RefineResult{}, nil
	}

	latestRecall, err := s.store.GetLatestEvalRecall(ctx)
	if err != nil {
		return RefineResult{}, fmt.Errorf("get latest eval recall: %w", err)
	}
	if latestRecall == 0 {
		// No eval runs yet — cannot judge.
		return RefineResult{}, nil
	}

	// Record the score on this version.
	if err := s.store.SetPromptVersionEvalScore(ctx, active.ID, latestRecall); err != nil {
		return RefineResult{}, fmt.Errorf("set eval score: %w", err)
	}

	// Compare against the parent version's score.
	if active.ParentVersion == 0 {
		// First version — no parent to compare against.
		return RefineResult{}, nil
	}

	parentScore, err := s.getParentEvalScore(ctx, active.PromptName, active.ParentVersion)
	if err != nil {
		return RefineResult{}, fmt.Errorf("get parent eval score: %w", err)
	}
	if parentScore == 0 {
		// No parent score to compare — keep the new version.
		return RefineResult{}, nil
	}

	if latestRecall < parentScore-s.cfg.EvalScoreTolerance {
		s.logger.Warn("prompt version eval regression detected, rolling back",
			"version_id", active.ID,
			"version", active.Version,
			"recall", latestRecall,
			"parent_recall", parentScore,
		)
		if err := s.store.RollbackPromptVersion(ctx, active.ID); err != nil {
			return RefineResult{}, fmt.Errorf("rollback prompt version: %w", err)
		}
		return RefineResult{
			RolledBack:     true,
			RollbackReason: fmt.Sprintf("eval recall %.3f < parent %.3f (tolerance %.3f)", latestRecall, parentScore, s.cfg.EvalScoreTolerance),
		}, nil
	}

	return RefineResult{}, nil
}

// ActiveReviewerPrompt returns the content of the active reviewer system prompt
// version, or empty string if none is active (meaning the default should be used).
func (s *Service) ActiveReviewerPrompt(ctx context.Context) string {
	active, err := s.store.GetActivePromptVersion(ctx, PromptNameReviewerSystem)
	if err != nil {
		return ""
	}
	return active.Content
}

func (s *Service) getParentEvalScore(ctx context.Context, promptName string, parentVersion int) (float64, error) {
	// Look up the parent version row directly by name + version number.
	pv, err := s.getVersionByNumber(ctx, promptName, parentVersion)
	if err != nil {
		return 0, err
	}
	if pv.EvalScore == nil {
		return 0, nil
	}
	return *pv.EvalScore, nil
}

func (s *Service) getVersionByNumber(ctx context.Context, promptName string, version int) (db.PromptVersion, error) {
	// Delegate to a store method that doesn't exist yet — use a simple query.
	// This is a read-only convenience; we can add a proper store method later.
	active, err := s.store.GetPromptVersionByNumber(ctx, promptName, version)
	return active, err
}

func (s *Service) callRefinementLLM(ctx context.Context, currentPrompt string, gaps []string) (string, error) {
	system := refinementSystemPrompt()
	user := refinementUserPrompt(currentPrompt, gaps)

	raw, err := s.client.ChatCompletion(ctx, reviewengine.ChatRequest{
		BaseURL:     s.cfg.Endpoint.BaseURL,
		APIKey:      s.cfg.Endpoint.APIKey,
		Model:       s.cfg.Endpoint.Model,
		Temperature: 0.2,
		System:      system,
		User:        user,
	})
	if err != nil {
		return "", fmt.Errorf("refinement completion: %w", err)
	}

	// The LLM should return the full revised prompt text.
	// Strip any markdown fences if present.
	refined := strings.TrimSpace(raw)
	refined = stripCodeFences(refined)
	if refined == "" {
		return "", fmt.Errorf("refinement LLM returned empty response")
	}
	return refined, nil
}

func refinementSystemPrompt() string {
	return `You are a prompt engineering specialist.
You will receive the current code review system prompt and a batch of identified prompt gaps
clustered by category (security, correctness, concurrency, performance, style, other).
Each gap describes an issue category that the current prompt fails to catch.

Your task:
1. Process each cluster in order — gaps within the same cluster often reveal a pattern.
2. For each cluster, identify which prompt sections need modification to address the pattern.
3. Rewrite the prompt to address ALL gaps while preserving existing capabilities.
4. Return ONLY the complete revised prompt text — no explanation, no markdown fences, no commentary.

Rules:
- Keep the JSON output format specification unchanged.
- Do not remove existing instructions unless they directly conflict with a gap fix.
- Be specific and actionable in new instructions.
- Address cluster patterns holistically — one well-written instruction can cover multiple related gaps.
- Keep the prompt concise — do not bloat it with redundant text.`
}

func refinementUserPrompt(currentPrompt string, gaps []string) string {
	var b strings.Builder
	b.WriteString("CURRENT PROMPT:\n")
	b.WriteString(currentPrompt)
	b.WriteString("\n\nIDENTIFIED GAPS (clustered by category):\n\n")

	clusters := clusterGaps(gaps)
	for _, cl := range clusters {
		b.WriteString(fmt.Sprintf("### %s (%d gap", strings.ToUpper(cl.category), len(cl.gaps)))
		if len(cl.gaps) != 1 {
			b.WriteString("s")
		}
		b.WriteString(")\n")
		for i, gap := range cl.gaps {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, gap))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// gapCluster groups prompt gaps by inferred category.
type gapCluster struct {
	category string
	gaps     []string
}

// clusterGaps groups gap texts by keyword-based category inference.
// Categories are sorted alphabetically for deterministic output.
func clusterGaps(gaps []string) []gapCluster {
	buckets := make(map[string][]string)
	for _, gap := range gaps {
		cat := inferGapCategory(gap)
		buckets[cat] = append(buckets[cat], gap)
	}

	cats := make([]string, 0, len(buckets))
	for cat := range buckets {
		cats = append(cats, cat)
	}
	sort.Strings(cats)

	clusters := make([]gapCluster, len(cats))
	for i, cat := range cats {
		clusters[i] = gapCluster{category: cat, gaps: buckets[cat]}
	}
	return clusters
}

// categoryKeywords maps categories to keywords that indicate membership.
var categoryKeywords = []struct {
	category string
	keywords []string
}{
	{"security", []string{
		"security", "injection", "xss", "ssrf", "csrf", "auth", "credential",
		"password", "token", "encrypt", "tls", "ssl", "certificate",
		"vulnerability", "sanitize", "escape", "traversal", "redirect",
		"secret", "hardcoded", "insecure", "permission", "privilege",
	}},
	{"correctness", []string{
		"null", "nil", "error handling", "panic", "bounds", "overflow",
		"off-by-one", "validation", "incorrect", "wrong", "bug", "crash",
		"undefined", "missing check", "return value", "type", "cast",
	}},
	{"concurrency", []string{
		"race", "deadlock", "mutex", "goroutine", "concurrent", "atomic",
		"sync", "lock", "thread", "channel", "data race", "parallel",
	}},
	{"performance", []string{
		"performance", "memory", "leak", "allocation", "cache", "complexity",
		"O(n", "slow", "efficient", "optimize", "latency", "throughput",
		"resource", "unbounded",
	}},
	{"style", []string{
		"style", "naming", "format", "convention", "readability",
		"documentation", "comment", "lint", "idiomatic", "consistency",
		"spelling", "whitespace",
	}},
}

// inferGapCategory classifies a gap text into a category by keyword matching.
func inferGapCategory(gap string) string {
	lower := strings.ToLower(gap)
	for _, entry := range categoryKeywords {
		for _, kw := range entry.keywords {
			if strings.Contains(lower, kw) {
				return entry.category
			}
		}
	}
	return "other"
}

func stripCodeFences(s string) string {
	// Remove leading ```...  and trailing ```
	if strings.HasPrefix(s, "```") {
		// Find end of first line (the opening fence).
		idx := strings.Index(s, "\n")
		if idx >= 0 {
			s = s[idx+1:]
		}
	}
	if strings.HasSuffix(s, "```") {
		s = s[:len(s)-3]
	}
	return strings.TrimSpace(s)
}

func defaultReviewerPrompt() string {
	return reviewengine.DefaultReviewerSystemPrompt()
}
