package promptrefine

import (
	"context"
	"fmt"
	"log/slog"
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
	Triggered       bool
	GapsProcessed   int
	NewVersionID    int64
	Activated       bool
	RolledBack      bool
	RollbackReason  string
}

// Service implements the automated prompt refinement loop.
type Service struct {
	cfg    Config
	store  *db.Store
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
	}
	// If err != nil (no active version), we use the default.

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
		// No active version — nothing to roll back.
		return RefineResult{}, nil
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

	parent, err := s.store.GetActivePromptVersion(ctx, PromptNameReviewerSystem)
	// Re-fetch won't work since active is still active. Instead look up the parent directly.
	_ = parent
	parentScore, err := s.getParentEvalScore(ctx, active.PromptName, active.ParentVersion)
	if err != nil || parentScore == 0 {
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
You will receive the current code review system prompt and a batch of identified prompt gaps.
Each gap describes a category of issues that the current prompt fails to catch.

Your task:
1. Analyze the gaps and identify which sections of the prompt need modification.
2. Rewrite the prompt to address the gaps while preserving all existing capabilities.
3. Return ONLY the complete revised prompt text — no explanation, no markdown fences, no commentary.

Rules:
- Keep the JSON output format specification unchanged.
- Do not remove existing instructions unless they directly conflict with a gap fix.
- Be specific and actionable in new instructions.
- Keep the prompt concise — do not bloat it with redundant text.`
}

func refinementUserPrompt(currentPrompt string, gaps []string) string {
	var b strings.Builder
	b.WriteString("CURRENT PROMPT:\n")
	b.WriteString(currentPrompt)
	b.WriteString("\n\nIDENTIFIED GAPS:\n")
	for i, gap := range gaps {
		b.WriteString(fmt.Sprintf("%d. %s\n", i+1, gap))
	}
	return b.String()
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
	return `You are a code review agent.
Return JSON ONLY with keys:
summary, findings, needs_more_context.
Each finding must include:
severity, category, file, line, evidence, explanation, suggestion, confidence.
If confidence < 0.6, add required items to needs_more_context instead of asserting uncertain findings.`
}
