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
	Trace  ReviewerTrace
	Err    error
}

// RunEnsemble preserves legacy single-shot review through a fresh executor per
// member.
func (e *Engine) RunEnsemble(ctx context.Context, in RunInput, cfg EnsembleConfig) (RunOutput, error) {
	return e.RunEnsembleWithExecutors(ctx, in, cfg, func(ModelRoute) ReviewerExecutor {
		return e.singleShotExecutor()
	})
}

// RunEnsembleWithExecutors runs one shared planner/prompt preparation, creates
// an isolated executor for every member, drops failed members, and merges all
// successful reviews before running one post-consensus walkthrough.
func (e *Engine) RunEnsembleWithExecutors(ctx context.Context, in RunInput, cfg EnsembleConfig, factory ReviewerExecutorFactory) (RunOutput, error) {
	if factory == nil {
		return RunOutput{}, fmt.Errorf("review engine: reviewer executor factory is required")
	}
	prepared, err := e.prepareReviewer(ctx, in)
	if err != nil {
		return RunOutput{}, err
	}
	models := append([]ModelRoute(nil), cfg.Models...)
	if len(models) == 0 {
		models = []ModelRoute{RouteCoder32B, RouteLLM70B}
	}

	var wg sync.WaitGroup
	results := make(chan modelResult, len(models))
	for _, route := range models {
		executor := factory(route)
		if executor == nil {
			results <- modelResult{Route: route, Err: fmt.Errorf("review engine: factory returned nil executor")}
			continue
		}
		wg.Add(1)
		go func(r ModelRoute, member ReviewerExecutor) {
			defer wg.Done()
			endpoint, routeErr := e.routeEndpoint(r)
			if routeErr != nil {
				results <- modelResult{Route: r, Err: routeErr}
				return
			}
			executed, executeErr := member.ExecuteReviewer(ctx, ReviewerExecutionRequest{
				Route: r, Endpoint: endpoint, Temperature: e.cfg.ReviewerTemp,
				System: prepared.system, User: prepared.user,
				Label: fmt.Sprintf("reviewer %s", r),
			})
			if executeErr == nil {
				executed, executeErr = normalizeReviewerExecution(executed)
			}
			if executeErr != nil {
				results <- modelResult{Route: r, Err: executeErr}
				return
			}
			results <- modelResult{
				Route: r, Review: executed.Review, Served: executed.ServedModel,
				Trace: executed.Trace,
			}
		}(route, executor)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var reviews []modelResult
	var errs []error
	var failures []ModelFailure
	var succeeded []ModelRoute
	var traces []EnsembleReviewerTrace
	for result := range results {
		if result.Err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.Route, result.Err))
			failures = append(failures, ModelFailure{Route: result.Route, Error: result.Err.Error()})
			if e.logger != nil {
				e.logger.Warn("ensemble model failed", "route", result.Route, "error", result.Err)
			}
			continue
		}
		reviews = append(reviews, result)
		succeeded = append(succeeded, result.Route)
		traces = append(traces, EnsembleReviewerTrace{Route: result.Route, Trace: result.Trace})
		if e.logger != nil {
			e.logger.Info("ensemble model completed", "route", result.Route, "findings", len(result.Review.Findings))
		}
	}
	sort.Slice(reviews, func(i, j int) bool { return reviews[i].Route < reviews[j].Route })
	sort.Slice(succeeded, func(i, j int) bool { return succeeded[i] < succeeded[j] })
	sort.Slice(failures, func(i, j int) bool { return failures[i].Route < failures[j].Route })
	sort.Slice(traces, func(i, j int) bool { return traces[i].Route < traces[j].Route })

	status := EnsembleStatus{
		RequiredReviewers: len(models), SucceededReviewers: succeeded,
		FailedReviewers: failures, ReviewerTraces: traces, Degraded: len(failures) > 0,
	}
	if len(reviews) == 0 {
		return RunOutput{}, fmt.Errorf("all %d ensemble reviewer(s) failed: %s", len(models), joinErrors(errs))
	}

	merged := mergeFindings(reviews, cfg, e.logger)
	merged, err = filterFindingsToChangedFiles(
		merged,
		in.ChangedFiles,
		in.TargetEnvelope,
		in.PatchDiff,
		in.ContextBundle,
		e.logger,
		"ensemble",
	)
	if err != nil {
		return RunOutput{}, err
	}

	summary := reviews[0].Review.Summary
	maxFindings := len(reviews[0].Review.Findings)
	for _, review := range reviews[1:] {
		if len(review.Review.Findings) > maxFindings {
			maxFindings = len(review.Review.Findings)
			summary = review.Review.Summary
		}
	}
	review := ReviewerOutput{
		Summary: summary, Findings: merged,
		NeedsMoreContext: collectNeedsMoreContext(reviews),
	}

	// This is deliberately after consensus: ensemble members never generate
	// their own walkthroughs.
	walkthrough, walkthroughStatus := e.generateWalkthrough(ctx, in)

	metrics.EnsembleReviewsRun.Inc()
	for _, member := range reviews {
		metrics.EnsembleModelsUsed.With(string(member.Route)).Inc()
	}
	metrics.EnsembleFindingsMerged.Add(int64(len(merged)))
	if e.logger != nil {
		e.logger.Info("ensemble review completed",
			"models", len(reviews),
			"failed_models", len(failures),
			"findings_merged", len(merged),
			"checklist_items", len(prepared.checklist),
			"walkthrough_status", walkthroughStatus.State,
			"has_walkthrough", walkthrough.Walkthrough != "",
		)
	}

	primary := reviews[0]
	for _, member := range reviews {
		if member.Route == prepared.planner.ModelRoute {
			primary = member
			break
		}
	}
	return RunOutput{
		Planner: prepared.planner, Review: review, Route: prepared.planner.ModelRoute,
		ServedModel: primary.Served, Checklist: prepared.checklist,
		Walkthrough: walkthrough, WalkthroughStatus: walkthroughStatus,
		ReviewerTrace: primary.Trace, EnsembleStatus: status,
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

	// Sort by canonical priority (desc), then confidence (desc), then file/line.
	// FindingPriorityRank accepts both P0/P1/P2 and every legacy severity, so
	// a canonical priority can never silently fall through a lookup map.
	sort.Slice(result, func(i, j int) bool {
		si := FindingPriorityRank(result[i])
		sj := FindingPriorityRank(result[j])
		if si != sj {
			return si > sj
		}
		legacyI, legacyJ := FindingLegacySeverityRank(result[i]), FindingLegacySeverityRank(result[j])
		if legacyI != legacyJ {
			return legacyI > legacyJ
		}
		if result[i].Confidence != result[j].Confidence {
			return result[i].Confidence > result[j].Confidence
		}
		if result[i].File != result[j].File {
			return result[i].File < result[j].File
		}
		return result[i].Line < result[j].Line
	})
	if normalized, err := NormalizeFindings(result); err == nil {
		return normalized
	}
	return result
}

// DeduplicateFindings merges findings in the same file and category whose
// locations are within two lines. The highest-confidence representative is
// retained and the result uses the ensemble severity/confidence ordering.
func DeduplicateFindings(findings []Finding) []Finding {
	ordered := append([]Finding(nil), findings...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if strings.ToLower(ordered[i].File) != strings.ToLower(ordered[j].File) {
			return strings.ToLower(ordered[i].File) < strings.ToLower(ordered[j].File)
		}
		if strings.ToLower(ordered[i].Category) != strings.ToLower(ordered[j].Category) {
			return strings.ToLower(ordered[i].Category) < strings.ToLower(ordered[j].Category)
		}
		return ordered[i].Line < ordered[j].Line
	})

	result := make([]Finding, 0, len(ordered))
	for _, finding := range ordered {
		merged := false
		for i := len(result) - 1; i >= 0; i-- {
			existing := result[i]
			if !strings.EqualFold(existing.File, finding.File) || !strings.EqualFold(existing.Category, finding.Category) {
				continue
			}
			if existing.Line-finding.Line > 2 || finding.Line-existing.Line > 2 {
				continue
			}
			if finding.Confidence > existing.Confidence {
				result[i] = finding
			}
			merged = true
			break
		}
		if !merged {
			result = append(result, finding)
		}
	}

	sort.SliceStable(result, func(i, j int) bool {
		if FindingPriorityRank(result[i]) != FindingPriorityRank(result[j]) {
			return FindingPriorityRank(result[i]) > FindingPriorityRank(result[j])
		}
		if FindingLegacySeverityRank(result[i]) != FindingLegacySeverityRank(result[j]) {
			return FindingLegacySeverityRank(result[i]) > FindingLegacySeverityRank(result[j])
		}
		if result[i].Confidence != result[j].Confidence {
			return result[i].Confidence > result[j].Confidence
		}
		if result[i].File != result[j].File {
			return result[i].File < result[j].File
		}
		return result[i].Line < result[j].Line
	})
	if normalized, err := NormalizeFindings(result); err == nil {
		return normalized
	}
	return result
}

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
