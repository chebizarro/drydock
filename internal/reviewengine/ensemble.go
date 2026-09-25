package reviewengine

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"git.sharegap.net/cascadia/drydock/internal/metrics"
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
	Route      ModelRoute
	Review     ReviewerOutput
	Served     string // model identifier the endpoint reported serving
	Trace      ReviewerTrace
	Transcript []CompletionMessage
	Err        error
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
	scope := in.FindingScope
	if scope == "" {
		scope = FindingScopePatch
	}
	if scope != FindingScopePatch && scope != FindingScopeSnapshot {
		return RunOutput{}, fmt.Errorf("review engine: unsupported finding scope %q", scope)
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
				Label:         fmt.Sprintf("reviewer %s", r),
				ContextBundle: in.ContextBundle, PatchDiff: in.PatchDiff,
				ChangedFiles:   append([]string(nil), in.ChangedFiles...),
				TargetEnvelope: in.TargetEnvelope, FindingScope: scope,
				Conversation: ReviewerConversation{History: cloneCompletionMessages(in.Conversation.History), Message: in.Conversation.Message, Sink: in.Conversation.Sink},
			})
			if executeErr == nil {
				executed, executeErr = normalizeReviewerExecution(executed, scope)
			}
			if executeErr != nil {
				results <- modelResult{Route: r, Trace: executed.Trace, Transcript: cloneCompletionMessages(executed.Transcript), Err: executeErr}
				return
			}
			results <- modelResult{
				Route: r, Review: executed.Review, Served: executed.ServedModel,
				Trace: executed.Trace, Transcript: cloneCompletionMessages(executed.Transcript),
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
		if result.Trace.StopReason != "" || result.Trace.Turns > 0 || result.Trace.ToolCalls > 0 {
			traces = append(traces, EnsembleReviewerTrace{Route: result.Route, Trace: result.Trace})
		}
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
		if e.logger != nil {
			e.logger.Info("ensemble model completed", "route", result.Route, "findings", len(result.Review.Findings))
		}
	}
	order := make(map[ModelRoute]int, len(models))
	for index, route := range models {
		if _, exists := order[route]; !exists {
			order[route] = index
		}
	}
	sort.SliceStable(reviews, func(i, j int) bool { return order[reviews[i].Route] < order[reviews[j].Route] })
	sort.SliceStable(succeeded, func(i, j int) bool { return order[succeeded[i]] < order[succeeded[j]] })
	sort.SliceStable(failures, func(i, j int) bool { return order[failures[i].Route] < order[failures[j].Route] })
	sort.SliceStable(traces, func(i, j int) bool { return order[traces[i].Route] < order[traces[j].Route] })

	status := EnsembleStatus{
		RequiredReviewers: len(models), SucceededReviewers: succeeded,
		FailedReviewers: failures, ReviewerTraces: traces, Degraded: len(failures) > 0,
	}
	if err := ctx.Err(); err != nil {
		return RunOutput{EnsembleStatus: status}, err
	}
	if len(reviews) == 0 {
		return RunOutput{}, fmt.Errorf("all %d ensemble reviewer(s) failed: %s", len(models), joinErrors(errs))
	}

	merged, err := mergeFindings(reviews, cfg, e.logger)
	if err != nil {
		return RunOutput{}, fmt.Errorf("merge ensemble findings: %w", err)
	}
	if scope == FindingScopePatch {
		merged, err = filterFindingsToChangedFiles(merged, in.ChangedFiles, in.TargetEnvelope,
			in.PatchDiff, in.ContextBundle, e.logger, "ensemble")
	} else {
		err = in.TargetEnvelope.VerifyMaterials(in.PatchDiff, in.ContextBundle)
		if err == nil && in.SnapshotFindingValidator == nil {
			err = fmt.Errorf("review engine: snapshot finding validator is required")
		}
		if err == nil {
			for _, finding := range merged {
				if validateErr := in.SnapshotFindingValidator(ctx, finding); validateErr != nil {
					err = fmt.Errorf("review engine: validate snapshot finding: %w", validateErr)
					break
				}
			}
		}
	}
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
		ReviewerTranscript: cloneCompletionMessages(primary.Transcript),
		ServedModel:        primary.Served, Checklist: prepared.checklist,
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

// SameFindingLocus reports whether two findings describe the same defect:
// same file and category (case-insensitive), within two lines. This is the
// single definition of finding identity, shared by mergeFindings,
// DeduplicateFindings, and securityscan.MergeScannerFindings.
func SameFindingLocus(a, b Finding) bool {
	// Dependency (SCA) findings are identified by the vulnerable package, not by
	// (file, category, line): a single manifest (go.mod, package-lock.json) holds
	// many packages, and scanners report every one against that one file — often
	// with no line at all — so the positional predicate would collapse distinct
	// vulnerable packages into a single finding. Compare package identity
	// whenever either side carries it.
	if a.Package != nil || b.Package != nil {
		return samePackageLocus(a, b)
	}
	if !strings.EqualFold(a.File, b.File) || !strings.EqualFold(a.Category, b.Category) {
		return false
	}
	delta := a.Line - b.Line
	if delta < 0 {
		delta = -delta
	}
	return delta <= 2
}

// samePackageLocus decides identity for dependency findings: same normalized
// ecosystem, package name, installed version, and rule (advisory) id. A finding
// that carries package identity never merges with one that does not, and two
// findings that differ in package, version, or advisory stay distinct so each
// vulnerable dependency yields its own finding — and its own upgrade candidate.
func samePackageLocus(a, b Finding) bool {
	if a.Package == nil || b.Package == nil {
		return false
	}
	return strings.EqualFold(a.Package.Ecosystem, b.Package.Ecosystem) &&
		strings.EqualFold(a.Package.Name, b.Package.Name) &&
		strings.EqualFold(a.Package.InstalledVersion, b.Package.InstalledVersion) &&
		strings.EqualFold(a.RuleID, b.RuleID)
}

// mergedFinding tracks a finding across multiple models.
type mergedFinding struct {
	// Anchor is the first finding seen in the cluster. Membership is decided
	// against it and it never changes, so whether two findings cluster cannot
	// depend on join order or which finding happened to have higher confidence.
	Anchor Finding
	// Finding is the representative used for explanatory fields; a
	// higher-confidence member may replace it, but that never widens the
	// clustering window because identity is anchored, not representative-based.
	Finding Finding
	// Models holds the distinct routes that contributed to the cluster.
	Models   []ModelRoute
	Priority Priority
}

// mergeFindings combines findings from multiple models, clusters them by the
// shared (file, category, ±2 line) window, and applies consensus scoring.
// Cluster identity is anchored to the first finding seen (stable), while the
// highest-confidence finding becomes the representative used for explanatory
// fields. Every distinct contributing route is tracked so consensus boost and
// RequireConsensus reflect how many models actually agreed.
func mergeFindings(reviews []modelResult, cfg EnsembleConfig, logger *slog.Logger) ([]Finding, error) {
	if len(reviews) == 0 {
		return nil, nil
	}

	// Flatten every finding tagged with its originating route, then sort so the
	// window clustering is deterministic (same ordering DeduplicateFindings uses).
	type routedFinding struct {
		finding Finding
		route   ModelRoute
	}
	var flat []routedFinding
	for _, r := range reviews {
		for _, f := range r.Review.Findings {
			flat = append(flat, routedFinding{finding: f, route: r.Route})
		}
	}
	sort.SliceStable(flat, func(i, j int) bool {
		fi, fj := flat[i].finding, flat[j].finding
		if !strings.EqualFold(fi.File, fj.File) {
			return strings.ToLower(fi.File) < strings.ToLower(fj.File)
		}
		if !strings.EqualFold(fi.Category, fj.Category) {
			return strings.ToLower(fi.Category) < strings.ToLower(fj.Category)
		}
		return fi.Line < fj.Line
	})

	var clusters []*mergedFinding
	for _, rf := range flat {
		var cluster *mergedFinding
		for i := len(clusters) - 1; i >= 0; i-- {
			// Compare against the stable anchor, never the representative, so the
			// ±2 window cannot drift as higher-confidence findings replace the
			// representative and chain otherwise-separate clusters together.
			if SameFindingLocus(clusters[i].Anchor, rf.finding) {
				cluster = clusters[i]
				break
			}
		}
		if cluster == nil {
			clusters = append(clusters, &mergedFinding{
				Anchor: rf.finding, Finding: rf.finding,
				Models: []ModelRoute{rf.route}, Priority: canonicalFindingPriority(rf.finding),
			})
			continue
		}
		// Count each route once: consensus means multiple *distinct* models
		// agreed, not one model reporting two nearby findings.
		if !containsRoute(cluster.Models, rf.route) {
			cluster.Models = append(cluster.Models, rf.route)
		}
		cluster.Priority = higherCanonicalPriority(cluster.Priority, canonicalFindingPriority(rf.finding))
		// Keep the higher-confidence representative for explanatory fields,
		// but never let confidence downgrade the cluster's priority.
		if rf.finding.Confidence > cluster.Finding.Confidence {
			cluster.Finding = rf.finding
		}
	}

	// Apply consensus boost and filter
	var result []Finding
	consensusBoost := cfg.ConsensusBoost
	if consensusBoost == 0 {
		consensusBoost = 0.10
	}

	for _, mf := range clusters {
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
		if mf.Priority != "" && mf.Priority != canonicalFindingPriority(finding) {
			finding.Priority = mf.Priority
			finding.Severity, _ = SeverityFromPriority(mf.Priority)
		}
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
	return NormalizeFindings(result)
}

// DeduplicateFindings merges findings in the same file and category whose
// locations are within two lines. The highest-confidence representative is
// retained and the result uses the ensemble severity/confidence ordering.
func DeduplicateFindings(findings []Finding) ([]Finding, error) {
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
			if !SameFindingLocus(existing, finding) {
				continue
			}
			priority := higherCanonicalPriority(canonicalFindingPriority(existing), canonicalFindingPriority(finding))
			if finding.Confidence > existing.Confidence {
				result[i] = finding
			}
			if priority != "" {
				result[i].Priority = priority
				result[i].Severity, _ = SeverityFromPriority(priority)
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
	return NormalizeFindings(result)
}

func containsRoute(routes []ModelRoute, route ModelRoute) bool {
	for _, r := range routes {
		if r == route {
			return true
		}
	}
	return false
}

func canonicalFindingPriority(f Finding) Priority {
	if priority, ok := NormalizePriority(string(f.Priority)); ok {
		return priority
	}
	priority, _ := PriorityFromSeverity(f.Severity)
	return priority
}

func higherCanonicalPriority(a, b Priority) Priority {
	if FindingPriorityRank(Finding{Priority: b}) > FindingPriorityRank(Finding{Priority: a}) {
		return b
	}
	return a
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
