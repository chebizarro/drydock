package reviewengine

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"drydock/internal/metrics"
)

// EnsembleConfig controls multi-model ensemble review behavior.
type EnsembleConfig struct {
	// Enabled turns on ensemble mode for reviews.
	Enabled bool
	// Models specifies which model routes to include in the ensemble.
	// If empty when enabled, defaults to [Coder32B, LLM70B].
	Models []ModelRoute
	// ConsensusBoost is the confidence boost per additional model that
	// reports the same finding (default 0.1).
	ConsensusBoost float64
	// RequireConsensus if true, only includes findings reported by 2+ models.
	RequireConsensus bool
}

// DefaultEnsembleConfig returns sensible defaults for ensemble mode.
func DefaultEnsembleConfig() EnsembleConfig {
	return EnsembleConfig{
		Enabled:          false,
		Models:           []ModelRoute{RouteCoder32B, RouteLLM70B},
		ConsensusBoost:   0.10,
		RequireConsensus: false,
	}
}

// modelResult holds the output from a single model in the ensemble.
type modelResult struct {
	Route  ModelRoute
	Review ReviewerOutput
	Served string // model identifier the endpoint reported serving
	Err    error
}

// RunEnsemble runs the review engine with multiple models in parallel and
// merges their findings using consensus scoring.
func (e *Engine) RunEnsemble(ctx context.Context, in RunInput, cfg EnsembleConfig) (RunOutput, error) {
	// Run planner first (single model)
	planner, err := e.completeStructured(ctx, ChatRequest{
		BaseURL:     e.cfg.Planner.BaseURL,
		APIKey:      e.cfg.Planner.APIKey,
		Model:       e.cfg.Planner.Model,
		Temperature: e.cfg.PlannerTemp,
		System:      plannerSystemPrompt(),
		User:        plannerUserPrompt(in.ContextBundle, in.ChangedFiles),
	}, "planner", ParsePlannerOutput)
	if err != nil {
		return RunOutput{}, err
	}

	// Build shared reviewer prompt
	checklist := BuildChecklist(in.ChangedFiles)
	if len(in.TestCoverageGaps) > 0 {
		checklist = append(checklist,
			fmt.Sprintf("Missing test coverage: symbols %s have no test references — consider flagging as a finding",
				strings.Join(in.TestCoverageGaps, ", ")))
	}
	system := reviewerSystemPrompt(in.ReviewerSystemPromptOverride, in.AdditionalInstructions, checklist, IsSecuritySensitive(in.ChangedFiles), in.FewShot)
	user := reviewerUserPrompt(in.ContextBundle, planner)

	// Determine which models to use
	models := cfg.Models
	if len(models) == 0 {
		models = []ModelRoute{RouteCoder32B, RouteLLM70B}
	}

	// Run all models in parallel
	var wg sync.WaitGroup
	results := make(chan modelResult, len(models))

	for _, route := range models {
		wg.Add(1)
		go func(r ModelRoute) {
			defer wg.Done()
			endpoint, err := e.routeEndpoint(r)
			if err != nil {
				results <- modelResult{Route: r, Err: err}
				return
			}
			review, served, err := e.completeStructuredReviewer(ctx, ChatRequest{
				BaseURL:     endpoint.BaseURL,
				APIKey:      endpoint.APIKey,
				Model:       endpoint.Model,
				Temperature: e.cfg.ReviewerTemp,
				System:      system,
				User:        user,
			}, fmt.Sprintf("reviewer %s", r))
			if err != nil {
				results <- modelResult{Route: r, Err: err}
				return
			}
			results <- modelResult{Route: r, Review: review, Served: served}
		}(route)
	}

	// Close results channel when all goroutines complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	var reviews []modelResult
	var errs []error
	var failures []ModelFailure
	var succeeded []ModelRoute
	for res := range results {
		if res.Err != nil {
			errs = append(errs, res.Err)
			failures = append(failures, ModelFailure{Route: res.Route, Error: res.Err.Error()})
			e.logger.Warn("ensemble model failed", "route", res.Route, "error", res.Err)
		} else {
			reviews = append(reviews, res)
			succeeded = append(succeeded, res.Route)
			e.logger.Info("ensemble model completed", "route", res.Route, "findings", len(res.Review.Findings))
		}
	}

	status := EnsembleStatus{
		RequiredReviewers:  len(models),
		SucceededReviewers: succeeded,
		FailedReviewers:    failures,
		Degraded:           len(failures) > 0,
	}

	// Fail closed: every configured ensemble reviewer is required. Publishing a
	// single-model success as an ensemble review hides reviewer failures.
	if len(errs) > 0 {
		return RunOutput{}, fmt.Errorf("ensemble failed closed: %d of %d required reviewer(s) failed: %s", len(errs), len(models), joinErrors(errs))
	}
	if len(reviews) == 0 {
		return RunOutput{}, fmt.Errorf("no models configured for ensemble")
	}

	// Merge findings with consensus scoring
	merged := mergeFindings(reviews, cfg, e.logger)

	// Use first successful review's summary (or synthesize one)
	summary := reviews[0].Review.Summary
	if len(reviews) > 1 {
		// Prefer the model with the most findings for the summary
		maxFindings := 0
		for _, r := range reviews {
			if len(r.Review.Findings) > maxFindings {
				maxFindings = len(r.Review.Findings)
				summary = r.Review.Summary
			}
		}
	}

	// Collect unique needs_more_context from all models
	needsCtx := collectNeedsMoreContext(reviews)

	review := ReviewerOutput{
		Summary:          summary,
		Findings:         merged,
		NeedsMoreContext: needsCtx,
	}

	// Generate walkthrough (using planner model — lightweight 14B)
	walkthrough, walkthroughStatus := e.generateWalkthrough(ctx, in)

	// Record ensemble metrics
	metrics.EnsembleReviewsRun.Inc()
	for _, r := range reviews {
		metrics.EnsembleModelsUsed.With(string(r.Route)).Inc()
	}
	metrics.EnsembleFindingsMerged.Add(int64(len(merged)))

	e.logger.Info("ensemble review completed",
		"models", len(reviews),
		"findings_merged", len(merged),
		"checklist_items", len(checklist),
		"walkthrough_status", walkthroughStatus.State,
		"has_walkthrough", walkthrough.Walkthrough != "")

	// Label the output with the served model of the planner's primary route
	// when that reviewer participated, otherwise the first successful one.
	servedModel := reviews[0].Served
	for _, r := range reviews {
		if r.Route == planner.ModelRoute {
			servedModel = r.Served
			break
		}
	}

	return RunOutput{
		Planner:           planner,
		Review:            review,
		Route:             planner.ModelRoute, // Primary route from planner
		ServedModel:       servedModel,
		Checklist:         checklist,
		Walkthrough:       walkthrough,
		WalkthroughStatus: walkthroughStatus,
		EnsembleStatus:    status,
	}, nil
}

func joinErrors(errs []error) string {
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			parts = append(parts, err.Error())
		}
	}
	return strings.Join(parts, "; ")
}

// findingKey generates a deduplication key for a finding.
// Findings are considered the same if they target the same file, line, and category.
func findingKey(f Finding) string {
	normalizedLine := (f.Line / 5) * 5
	return fmt.Sprintf("%s:%d:%s", strings.ToLower(f.File), normalizedLine, strings.ToLower(f.Category))
}

// mergedFinding tracks a finding across multiple models.
type mergedFinding struct {
	Finding    Finding
	Models     []ModelRoute
	Confidence float64
}

// mergeFindings combines findings from multiple models, deduplicates by
// (file, line, category), and applies consensus scoring.
func mergeFindings(reviews []modelResult, cfg EnsembleConfig, logger *slog.Logger) []Finding {
	if len(reviews) == 0 {
		return nil
	}

	// Group findings by key
	byKey := make(map[string]*mergedFinding)

	for _, r := range reviews {
		for _, f := range r.Review.Findings {
			key := findingKey(f)
			if existing, ok := byKey[key]; ok {
				// Finding already reported by another model — boost confidence
				existing.Models = append(existing.Models, r.Route)
				// Keep the higher base confidence
				if f.Confidence > existing.Finding.Confidence {
					existing.Finding = f
				}
			} else {
				byKey[key] = &mergedFinding{
					Finding:    f,
					Models:     []ModelRoute{r.Route},
					Confidence: f.Confidence,
				}
			}
		}
	}

	// Apply consensus boost and filter
	var result []Finding
	consensusBoost := cfg.ConsensusBoost
	if consensusBoost == 0 {
		consensusBoost = 0.10
	}

	for _, mf := range byKey {
		// Skip if consensus required but only one model reported
		if cfg.RequireConsensus && len(mf.Models) < 2 {
			logger.Debug("finding dropped: no consensus",
				"file", mf.Finding.File,
				"line", mf.Finding.Line,
				"category", mf.Finding.Category,
				"models", len(mf.Models))
			continue
		}

		// Apply consensus boost: +boost per additional model
		boostedConfidence := mf.Finding.Confidence
		if len(mf.Models) > 1 {
			boost := consensusBoost * float64(len(mf.Models)-1)
			boostedConfidence = mf.Finding.Confidence + boost
			if boostedConfidence > 1.0 {
				boostedConfidence = 1.0
			}
			metrics.EnsembleConsensusBoost.Inc()
			logger.Debug("finding consensus boost",
				"file", mf.Finding.File,
				"line", mf.Finding.Line,
				"original_confidence", mf.Finding.Confidence,
				"boosted_confidence", boostedConfidence,
				"models", len(mf.Models))
		}

		finding := mf.Finding
		finding.Confidence = boostedConfidence
		result = append(result, finding)
	}

	// Sort by severity (desc), then confidence (desc), then file/line
	sort.Slice(result, func(i, j int) bool {
		sevOrder := map[string]int{
			"critical": 5, "high": 4, "medium": 3, "low": 2, "info": 1,
		}
		si := sevOrder[strings.ToLower(result[i].Severity)]
		sj := sevOrder[strings.ToLower(result[j].Severity)]
		if si != sj {
			return si > sj
		}
		if result[i].Confidence != result[j].Confidence {
			return result[i].Confidence > result[j].Confidence
		}
		if result[i].File != result[j].File {
			return result[i].File < result[j].File
		}
		return result[i].Line < result[j].Line
	})

	return result
}

// collectNeedsMoreContext merges needs_more_context from all models.
func collectNeedsMoreContext(reviews []modelResult) []string {
	seen := make(map[string]bool)
	var result []string
	for _, r := range reviews {
		for _, ctx := range r.Review.NeedsMoreContext {
			ctx = strings.TrimSpace(ctx)
			if ctx != "" && !seen[ctx] {
				seen[ctx] = true
				result = append(result, ctx)
			}
		}
	}
	return result
}
