package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"drydock/internal/contextbuilder"
	"drydock/internal/db"
	"drydock/internal/metareview"
	"drydock/internal/metrics"
	"drydock/internal/payment"
	"drydock/internal/promptrefine"
	"drydock/internal/publisher"
	"drydock/internal/repo"
	"drydock/internal/repoconfig"
	"drydock/internal/reviewengine"
	"drydock/internal/securityscan"
	"drydock/internal/tracing"

	"fiatjaf.com/nostr"
)

// Runner reads review tasks from a channel and executes the full pipeline:
// repo prepare → context build → LLM review → publish → meta-review.
// PromptRefiner is the subset of promptrefine.Service used by the pipeline.
type PromptRefiner interface {
	ActiveReviewerPrompt(ctx context.Context) string
}

// DocIngester indexes project documentation into the vector store.
// Called after repo preparation so that project docs are searchable
// by the QdrantProvider during context building.
type DocIngester interface {
	IngestRepoDocs(ctx context.Context, repoPath, repoID string) error
}

// CodeIndexer indexes source code symbols into a vector store for
// semantic code search. Called after repo preparation so that the
// related-code provider can retrieve relevant code during context building.
type CodeIndexer interface {
	IndexRepo(ctx context.Context, repoPath, repoID string) error
}

// PaymentAuthorizer gates reviews according to the repository payment policy.
type PaymentAuthorizer interface {
	AuthorizePatch(ctx context.Context, patchEvent nostr.Event, repoID string, policy repoconfig.PaymentsConfig) (payment.AuthorizeResult, error)
}

type Runner struct {
	store            *db.Store
	repoSvc          *repo.Service
	ctxBuilder       *contextbuilder.Builder
	engine           *reviewengine.Engine
	pubSvc           *publisher.Service
	metaSvc          *metareview.Service
	promptRefiner    PromptRefiner
	fewShotRetriever FewShotRetriever
	docIngester      DocIngester
	codeIndexer      CodeIndexer
	secScanner       *securityscan.Scanner
	paymentAuth      PaymentAuthorizer
	queue            <-chan db.ReviewTask
	workers          int
	logger           *slog.Logger
	activityHook     func()

	// Narrow function seams keep failure handling testable without replacing the
	// concrete services used by the rest of the pipeline.
	isPatchSuperseded func(context.Context, string, string, string) (bool, error)
	publishStatus     func(context.Context, publisher.PublishStatusInput) (publisher.PublishStatusResult, error)
	buildAutoFixPatch func(context.Context, string, []repo.AutoFixSuggestion) (repo.AutoFixResult, error)
}

type Config struct {
	Workers int
}

// WithPromptRefiner sets an optional prompt refinement service on the runner.
// When set, the runner uses the active versioned reviewer prompt for each review.
func WithPromptRefiner(pr *promptrefine.Service) func(*Runner) {
	return func(r *Runner) {
		r.promptRefiner = pr
	}
}

// WithFewShotRetriever sets a custom few-shot retriever. When not set, the
// runner falls back to recency-based retrieval from the database.
func WithFewShotRetriever(fsr FewShotRetriever) func(*Runner) {
	return func(r *Runner) {
		r.fewShotRetriever = fsr
	}
}

// WithDocIngester sets an optional documentation ingester. When set, the
// runner indexes project docs after repo preparation so the QdrantProvider
// can retrieve them during context building.
func WithDocIngester(di DocIngester) func(*Runner) {
	return func(r *Runner) {
		r.docIngester = di
	}
}

// WithCodeIndexer sets an optional code indexer. When set, the runner
// indexes source code symbols after repo preparation so the related-code
// provider can retrieve semantically similar code during context building.
func WithCodeIndexer(ci CodeIndexer) func(*Runner) {
	return func(r *Runner) {
		r.codeIndexer = ci
	}
}

// WithSecurityScanner enables deterministic SAST scanning alongside LLM review.
// Scanner findings are deduplicated with LLM findings and merged into the final output.
func WithSecurityScanner(scanner *securityscan.Scanner) func(*Runner) {
	return func(r *Runner) {
		r.secScanner = scanner
	}
}

// WithPaymentAuthorizer enables per-repository payment gating before expensive review work.
func WithPaymentAuthorizer(auth PaymentAuthorizer) func(*Runner) {
	return func(r *Runner) {
		r.paymentAuth = auth
	}
}

// WithActivityHeartbeat sets a callback that is invoked by workers whenever
// they begin and complete processing a task.
func WithActivityHeartbeat(hook func()) func(*Runner) {
	return func(r *Runner) {
		r.activityHook = hook
	}
}

func New(
	cfg Config,
	store *db.Store,
	repoSvc *repo.Service,
	ctxBuilder *contextbuilder.Builder,
	engine *reviewengine.Engine,
	pubSvc *publisher.Service,
	metaSvc *metareview.Service,
	queue <-chan db.ReviewTask,
	logger *slog.Logger,
	opts ...func(*Runner),
) *Runner {
	workers := cfg.Workers
	if workers <= 0 {
		workers = 2
	}
	r := &Runner{
		store:      store,
		repoSvc:    repoSvc,
		ctxBuilder: ctxBuilder,
		engine:     engine,
		pubSvc:     pubSvc,
		metaSvc:    metaSvc,
		queue:      queue,
		workers:    workers,
		logger:     logger,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run starts worker goroutines and blocks until ctx is cancelled.
// It waits for all in-flight work to finish before returning.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < r.workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			r.work(ctx, id)
		}(i)
	}
	<-ctx.Done()
	r.logger.Info("pipeline shutdown: waiting for in-flight reviews to finish")
	wg.Wait()
	r.logger.Info("pipeline shutdown: all workers stopped")
}

func (r *Runner) work(ctx context.Context, id int) {
	log := r.logger.With("worker", id)
	for {
		select {
		case <-ctx.Done():
			return
		case task, ok := <-r.queue:
			if !ok {
				return
			}
			if r.activityHook != nil {
				r.activityHook()
			}
			metrics.ReviewQueueDepth.Dec()
			metrics.ReviewsStarted.Inc()
			metrics.WorkersActive.Inc()
			done := metrics.Timer(metrics.ReviewDuration)

			// Create trace context for this task
			taskCtx := tracing.WithTraceData(ctx, tracing.TraceData{
				TraceID: tracing.NewTraceID(),
				EventID: task.PatchEventID,
				RepoID:  task.RepoID,
			})
			taskLog := tracing.Logger(taskCtx, log)

			taskLog.Info("processing review task")
			if err := r.process(taskCtx, task); err != nil {
				metrics.ReviewsFinished.With("failed").Inc()
				taskLog.Error("review pipeline failed",
					"error", err,
					"elapsed_ms", tracing.Elapsed(taskCtx).Milliseconds())
				if markErr := r.store.MarkReviewFailed(ctx, task.PatchEventID, task.RepoID, err.Error()); markErr != nil {
					taskLog.Error("failed to mark review as failed", "error", markErr)
				}
			} else {
				metrics.ReviewsFinished.With("published").Inc()
				taskLog.Info("review pipeline completed",
					"elapsed_ms", tracing.Elapsed(taskCtx).Milliseconds())
			}
			done()
			metrics.WorkersActive.Dec()
			if r.activityHook != nil {
				r.activityHook()
			}
		}
	}
}

func (r *Runner) process(ctx context.Context, task db.ReviewTask) error {
	log := tracing.Logger(ctx, r.logger)
	timer := tracing.NewPipelineTimer(ctx, r.logger)
	defer timer.Summary()

	// 1. Prepare repo + apply patch series
	var prep repo.PrepareResult
	var prepErr error
	timer.Time(tracing.StageRepoPrepare, func() error {
		prep, prepErr = r.repoSvc.PreparePatchSeries(ctx, task.PatchEventID)
		return prepErr
	})
	if prepErr != nil {
		// Publish a review comment about the apply failure so the patch author gets feedback
		if prep.FailureHint != "" && r.pubSvc != nil {
			r.publishApplyFailure(ctx, task, prep.FailureHint)
		}
		return fmt.Errorf("prepare patch series: %w", prepErr)
	}
	// Clean up the throwaway review branch when done (success or failure)
	defer r.repoSvc.CleanupReviewBranch(ctx, prep.RepoPath, prep.Branch)

	// 1b. Load per-repo config from the base branch (before patches).
	repoCfg := repoconfig.Default()
	if len(prep.BaseRepoConfig) > 0 {
		var cfgErr error
		repoCfg, cfgErr = repoconfig.Parse(prep.BaseRepoConfig)
		if cfgErr != nil {
			r.logger.Warn("failed to parse .drydock.yaml, using defaults",
				"patch_event_id", task.PatchEventID, "repo_id", task.RepoID, "error", cfgErr)
			if repoconfig.ContainsPaymentsConfig(prep.BaseRepoConfig) {
				return fmt.Errorf("payment_blocked:invalid_repo_payment_policy")
			}
			repoCfg = repoconfig.Default()
		}
	}

	// 1c. Get patch event for payment authorization, context builder, and meta-review.
	patchRec, err := r.store.GetPatchEvent(ctx, task.PatchEventID)
	if err != nil {
		return fmt.Errorf("get patch event: %w", err)
	}
	var patchEvent nostr.Event
	if err := json.Unmarshal([]byte(patchRec.RawEvent), &patchEvent); err != nil {
		return fmt.Errorf("decode patch event: %w", err)
	}

	// 1d. Authorize payment-gated repositories before documentation/code indexing, context building, or LLM calls.
	if repoCfg.Payments.Enabled {
		if r.paymentAuth == nil {
			return fmt.Errorf("payment_blocked:payment_service_not_configured")
		}
		auth, err := r.paymentAuth.AuthorizePatch(ctx, patchEvent, task.RepoID, repoCfg.Payments)
		if err != nil {
			return fmt.Errorf("authorize payment: %w", err)
		}
		if !auth.Allowed {
			return fmt.Errorf("payment_blocked:%s", auth.Reason)
		}
		log.Info("review payment authorized",
			"patch_event_id", task.PatchEventID,
			"repo_id", task.RepoID,
			"access_kind", auth.AccessKind)
	}

	// 1e. Index project documentation (non-fatal; skip if repo config disables docs).
	if r.docIngester != nil && repoCfg.DocsEnabled() {
		timer.Time(tracing.StageDocIngest, func() error {
			if err := r.docIngester.IngestRepoDocs(ctx, prep.RepoPath, task.RepoID); err != nil {
				log.Warn("doc ingestion failed, continuing without", "error", err)
			}
			return nil // non-fatal
		})
	}

	// 1f. Index source code for semantic search. When configured, this is required:
	// silently reviewing without related-code context hides total index failures.
	if err := timer.Time(tracing.StageCodeIndex, func() error {
		return r.indexSourceCode(ctx, prep.RepoPath, task.RepoID, log)
	}); err != nil {
		return err
	}

	// 2. Determine the unified diff for review. Kind 1617 patch events carry
	// the diff in the event content; PR-style events (kind 1618/1619) carry a
	// cover letter there, so we use the git diff computed by repo prepare
	// (PR tip vs merge-base with the default branch) instead.
	patchDiffContent := patchEvent.Content
	if strings.TrimSpace(prep.Diff) != "" {
		patchDiffContent = prep.Diff
	} else if patchRec.Kind != 1617 {
		return fmt.Errorf("PR event %s (kind %d) produced no diff against its base", task.PatchEventID, patchRec.Kind)
	}

	// 3b. Validate that the patch diff is non-empty to avoid wasting an LLM call.
	if strings.TrimSpace(patchDiffContent) == "" {
		return fmt.Errorf("patch event %s has empty diff content", task.PatchEventID)
	}

	// 4. Build context bundle (with repo-config overrides)
	var bundle contextbuilder.ContextBundle
	if err := timer.Time(tracing.StageContextBuild, func() error {
		var buildErr error
		bundle, buildErr = r.ctxBuilder.Build(ctx, contextbuilder.BuildInput{
			PatchEventContent:   patchDiffContent,
			RepoPath:            prep.RepoPath,
			RepoID:              task.RepoID,
			TokenBudgetOverride: repoCfg.Context.TokenBudget,
			ExcludePaths:        repoCfg.Context.ExcludePaths,
			DisableDocs:         !repoCfg.DocsEnabled(),
		})
		return buildErr
	}); err != nil {
		return fmt.Errorf("build context: %w", err)
	}
	for _, status := range bundle.LayerStatuses {
		metrics.ContextLayersByStatus.With(status.Status).Inc()
		if status.Status != "used" {
			r.logger.Warn("context layer not fully available", "layer", status.Layer, "status", status.Status, "message", status.Message, "tokens", status.Tokens)
		}
	}

	// 5. Extract changed files from the context bundle (used for few-shot, engine, etc.).
	changedFiles := bundle.ChangedFiles

	// 5b. Retrieve few-shot examples for reviewer prompt injection
	var fewShot []string
	timer.Time(tracing.StageFewShotRetrieval, func() error {
		var fewShotErr error
		if r.fewShotRetriever != nil {
			fewShot, fewShotErr = r.fewShotRetriever.RetrieveFewShots(ctx, FewShotQuery{
				PatchDiff: patchDiffContent,
				Limit:     2,
				Language:  DetectLanguage(changedFiles),
				RepoID:    task.RepoID,
			})
		} else {
			fewShot, fewShotErr = r.store.GetRecentFewShots(ctx, 3)
		}
		if fewShotErr != nil {
			log.Warn("failed to retrieve few-shot examples, continuing without", "error", fewShotErr)
			fewShot = nil
		}
		return nil // non-fatal
	})

	// 6. Run LLM review engine (with active prompt version override if available)
	var promptOverride string
	if r.promptRefiner != nil {
		promptOverride = r.promptRefiner.ActiveReviewerPrompt(ctx)
	}

	// 6b. Check if exclusions left no reviewable files.
	if len(changedFiles) == 0 && len(bundle.ExcludedFiles) > 0 {
		// All changed files were excluded by repo policy — skip LLM call.
		reviewEventID, pubErr := r.pubSvc.PublishReview(ctx, publisher.PublishInput{
			PatchEventID:         task.PatchEventID,
			RepoID:               task.RepoID,
			Summary:              "This patch only modifies files excluded by repository review policy, so no automated review was run.",
			Model:                "none",
			ContextHash:          fmt.Sprintf("%x", sha256.Sum256([]byte(bundle.Content))),
			ContextLayersUsed:    bundle.LayersUsed,
			ContextLayersDropped: bundle.LayersDropped,
			ExcludedFiles:        bundle.ExcludedFiles,
		})
		if pubErr != nil {
			return fmt.Errorf("publish exclusion-only review: %w", pubErr)
		}
		r.logger.Info("skipped LLM review (all files excluded by repo policy)",
			"patch_event_id", task.PatchEventID, "review_event_id", reviewEventID,
			"excluded_files", len(bundle.ExcludedFiles))
		return nil
	}

	// 6c. Run review engine (single model or ensemble mode)
	runInput := reviewengine.RunInput{
		ContextBundle:                bundle.Content,
		ChangedFiles:                 changedFiles,
		FewShot:                      fewShot,
		ReviewerSystemPromptOverride: promptOverride,
		AdditionalInstructions:       repoCfg.PromptInstructions(),
		TestCoverageGaps:             bundle.TestCoverageGaps,
		SkipWalkthrough:              !repoCfg.WalkthroughEnabled(),
	}

	var result reviewengine.RunOutput
	if err := timer.Time(tracing.StageLLMReview, func() error {
		var reviewErr error
		if repoCfg.Ensemble.Enabled {
			ensembleCfg := repoCfg.Ensemble.ToReviewEngineEnsembleConfig()
			result, reviewErr = r.engine.RunEnsemble(ctx, runInput, ensembleCfg)
			if reviewErr != nil {
				return fmt.Errorf("ensemble review engine: %w", reviewErr)
			}
			log.Info("ensemble review completed",
				"models", len(ensembleCfg.Models),
				"findings", len(result.Review.Findings))
		} else {
			result, reviewErr = r.engine.Run(ctx, runInput)
			if reviewErr != nil {
				return fmt.Errorf("review engine: %w", reviewErr)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// 6d. Run security scanner (deterministic SAST, parallel with LLM review is possible
	// but kept sequential here for simplicity and determinism).
	var scanFindings []securityscan.SecurityFinding
	if r.secScanner != nil && len(changedFiles) > 0 {
		timer.Time(tracing.StageSecurityScan, func() error {
			scanResult := r.secScanner.ScanFiles(ctx, prep.RepoPath, changedFiles, patchDiffContent)
			scanFindings = scanResult.Findings
			if len(scanFindings) > 0 {
				metrics.SecurityScanFindings.Add(int64(len(scanFindings)))
				log.Info("security scan complete",
					"files_scanned", scanResult.FilesScanned,
					"findings", len(scanFindings))
			}
			return nil
		})
	}

	// 7. Compute context hash
	ctxHash := fmt.Sprintf("%x", sha256.Sum256([]byte(bundle.Content)))

	// 7b. Deduplicate scanner findings with LLM findings, then apply review policy.
	mergedFindings := securityscan.DeduplicateFindings(scanFindings, result.Review.Findings)
	mergedReview := result.Review
	mergedReview.Findings = mergedFindings
	filteredReview := applyReviewPolicy(mergedReview, repoCfg)

	// 8. Compute mean confidence
	confidence := meanConfidence(filteredReview.Findings)

	// 9. Check if this patch has been superseded by a newer revision. Fail
	// closed after one retry rather than publishing content whose freshness is
	// unknown.
	superseded, err := r.checkPatchSuperseded(ctx, task.PatchEventID, patchRec.RootID, task.RepoID)
	if err != nil {
		return err
	}
	if superseded {
		r.logger.Info("patch is superseded, using short TTL",
			"patch_event_id", task.PatchEventID, "root_id", patchRec.RootID)
	}

	// 10. Publish review
	var reviewEventID string
	if err := timer.Time(tracing.StagePublish, func() error {
		var pubErr error
		reviewEventID, pubErr = r.pubSvc.PublishReview(ctx, publisher.PublishInput{
			PatchEventID:         task.PatchEventID,
			RepoID:               task.RepoID,
			Summary:              filteredReview.Summary,
			Findings:             filteredReview.Findings,
			Model:                modelName(result.Route, r.engine),
			ContextHash:          ctxHash,
			Confidence:           confidence,
			ContextLayersUsed:    bundle.LayersUsed,
			ContextLayersDropped: bundle.LayersDropped,
			ExcludedFiles:        bundle.ExcludedFiles,
			Superseded:           superseded,
			DetailSeverityFloor:  repoCfg.Review.DetailSeverityFloor,
			Walkthrough:          result.Walkthrough,
		})
		return pubErr
	}); err != nil {
		return fmt.Errorf("publish review: %w", err)
	}

	// 11. Log success (MarkReviewPublished is already called inside PublishReview)
	log.Info("review published",
		"review_event_id", reviewEventID,
		"findings", len(filteredReview.Findings),
	)

	// 11b. Publish NIP-34 review status event. A configured status output is
	// part of task completion: returning its error lets the existing review
	// retry path reuse the durable review outbox and retry status idempotently.
	if r.pubSvc != nil {
		if err := timer.Time(tracing.StageStatusPublish, func() error {
			statusResult, statusErr := r.publishReviewStatus(ctx, publisher.PublishStatusInput{
				PatchEventID:  task.PatchEventID,
				RepoID:        task.RepoID,
				ReviewEventID: reviewEventID,
				Summary:       filteredReview.Summary,
				Findings:      filteredReview.Findings,
				Model:         modelName(result.Route, r.engine),
				Confidence:    confidence,
				Superseded:    superseded,
				Policy: publisher.StatusPolicy{
					Enabled:           repoCfg.Status.Enabled,
					OpenSeverityFloor: repoCfg.Status.OpenSeverityFloor,
					MinConfidence:     repoCfg.Status.MinConfidence,
				},
			})
			if statusErr != nil {
				return statusErr
			} else if statusResult.Published {
				log.Info("NIP-34 status event published",
					"status_event_id", statusResult.EventID,
					"kind", int(statusResult.Kind),
					"reason", statusResult.Reason)
			} else {
				log.Debug("NIP-34 status skipped", "reason", statusResult.Reason)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("publish NIP-34 status: %w", err)
		}
	}

	// 11c. Auto-fix patch generation (best-effort, non-fatal).
	if r.pubSvc != nil && repoCfg.AutoFix.Enabled {
		fixResult := r.tryAutoFix(ctx, task, prep, filteredReview, repoCfg, reviewEventID, modelName(result.Route, r.engine))
		if fixResult != nil && fixResult.Published {
			r.logger.Info("auto-fix patch published",
				"patch_event_id", task.PatchEventID,
				"fix_event_id", fixResult.EventID,
				"applied_count", fixResult.AppliedCount)
		}
	}

	// 12. Async meta-review (non-blocking, uses filtered review)
	if r.metaSvc != nil {
		r.metaSvc.RunAsync(ctx, metareview.Input{
			PatchEventID:  task.PatchEventID,
			RepoID:        task.RepoID,
			PatchDiff:     patchDiffContent,
			ContextBundle: bundle.Content,
			ContextHash:   ctxHash,
			ChangedFiles:  changedFiles,
			LocalReview:   filteredReview,
		})
	}

	return nil
}

func (r *Runner) indexSourceCode(ctx context.Context, repoPath, repoID string, log *slog.Logger) error {
	if r.codeIndexer == nil {
		return nil
	}
	if err := r.codeIndexer.IndexRepo(ctx, repoPath, repoID); err != nil {
		if log != nil {
			log.Error("code indexing failed", "error", err)
		}
		return fmt.Errorf("code indexing: %w", err)
	}
	return nil
}

func (r *Runner) publishApplyFailure(ctx context.Context, task db.ReviewTask, hint string) {
	summary := fmt.Sprintf(
		"This patch does not apply cleanly to the current HEAD.\n\nReason: %s\n\n"+
			"The patch may need to be rebased or updated to resolve conflicts.",
		hint,
	)
	_, err := r.pubSvc.PublishReview(ctx, publisher.PublishInput{
		PatchEventID:         task.PatchEventID,
		RepoID:               task.RepoID,
		Summary:              summary,
		Findings:             nil,
		Model:                "none",
		ContextHash:          "",
		Confidence:           0,
		ContextLayersUsed:    nil,
		ContextLayersDropped: nil,
		Superseded:           false,
	})
	if err != nil {
		r.logger.Warn("failed to publish apply-failure review",
			"patch_event_id", task.PatchEventID,
			"repo_id", task.RepoID,
			"error", err,
		)
	} else {
		r.logger.Info("published apply-failure review",
			"patch_event_id", task.PatchEventID,
			"repo_id", task.RepoID,
		)
	}
}

// autoFixResult is the pipeline-local view of a fix-patch publish outcome.
type autoFixResult struct {
	Attempted    bool
	Published    bool
	EventID      string
	AppliedCount int
	Reason       string
}

// tryAutoFix filters eligible findings, synthesizes a combined fix patch on the
// review branch, and publishes it as a NIP-34 kind 1617 event. Best-effort
// failures are recorded on the review and reflected in metrics.
func (r *Runner) tryAutoFix(
	ctx context.Context,
	task db.ReviewTask,
	prep repo.PrepareResult,
	review reviewengine.ReviewerOutput,
	cfg repoconfig.RepoConfig,
	reviewEventID string,
	model string,
) *autoFixResult {
	// 1. Filter eligible findings.
	var suggestions []repo.AutoFixSuggestion
	for _, f := range review.Findings {
		if f.SuggestedDiff == "" {
			continue
		}
		if f.Confidence < cfg.AutoFix.MinConfidence {
			continue
		}
		suggestions = append(suggestions, repo.AutoFixSuggestion{
			FilePath:      f.File,
			SuggestedDiff: f.SuggestedDiff,
			Confidence:    f.Confidence,
		})
		if len(suggestions) >= cfg.AutoFix.MaxFindings {
			break
		}
	}

	if len(suggestions) == 0 {
		metrics.AutoFixSkipped.Inc()
		r.logger.Debug("autofix: no eligible findings",
			"patch_event_id", task.PatchEventID)
		return nil
	}

	// 2. Synthesize combined patch on the review branch.
	fixResult, err := r.buildAutoFix(ctx, prep.RepoPath, suggestions)
	if err != nil {
		metrics.AutoFixPublishFailures.Inc()
		reason := fmt.Sprintf("patch synthesis failed: %v", err)
		r.recordAutoFixOutcome(ctx, task, "failed", reason)
		r.logger.Warn("autofix: patch synthesis failed",
			"patch_event_id", task.PatchEventID,
			"error", err)
		return &autoFixResult{Attempted: true, Reason: reason}
	}
	if fixResult.AppliedCount == 0 || fixResult.PatchDiff == "" {
		metrics.AutoFixSkipped.Inc()
		reason := "no suggestions applied cleanly"
		r.recordAutoFixOutcome(ctx, task, "failed", reason)
		r.logger.Debug("autofix: no suggestions applied cleanly",
			"patch_event_id", task.PatchEventID,
			"attempted", len(suggestions))
		return &autoFixResult{Attempted: true, Reason: reason}
	}

	// 3. Publish the fix patch.
	pubResult, err := r.pubSvc.PublishFixPatch(ctx, publisher.PublishFixPatchInput{
		PatchEventID:  task.PatchEventID,
		RepoID:        task.RepoID,
		ReviewEventID: reviewEventID,
		PatchDiff:     fixResult.PatchDiff,
		AppliedCount:  fixResult.AppliedCount,
		AppliedFiles:  fixResult.AppliedFiles,
		Model:         model,
	})
	if err != nil {
		reason := fmt.Sprintf("publish failed: %v", err)
		r.recordAutoFixOutcome(ctx, task, "failed", reason)
		r.logger.Warn("autofix: publish failed (non-fatal)",
			"patch_event_id", task.PatchEventID,
			"error", err)
		return &autoFixResult{Attempted: true, Reason: reason}
	}

	r.recordAutoFixOutcome(ctx, task, "succeeded", pubResult.EventID)
	return &autoFixResult{
		Attempted:    true,
		Published:    pubResult.Published,
		EventID:      pubResult.EventID,
		AppliedCount: fixResult.AppliedCount,
	}
}

func (r *Runner) checkPatchSuperseded(ctx context.Context, patchEventID, rootID, repoID string) (bool, error) {
	lookup := r.isPatchSuperseded
	if lookup == nil {
		lookup = r.store.IsPatchSuperseded
	}
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		superseded, err := lookup(ctx, patchEventID, rootID, repoID)
		if err == nil {
			return superseded, nil
		}
		lastErr = err
		r.logger.Warn("failed to check superseded status",
			"patch_event_id", patchEventID, "attempt", attempt, "error", err)
	}
	return false, fmt.Errorf("check superseded status after retry: %w", lastErr)
}

func (r *Runner) publishReviewStatus(ctx context.Context, in publisher.PublishStatusInput) (publisher.PublishStatusResult, error) {
	if r.publishStatus != nil {
		return r.publishStatus(ctx, in)
	}
	return r.pubSvc.PublishStatus(ctx, in)
}

func (r *Runner) buildAutoFix(ctx context.Context, repoPath string, suggestions []repo.AutoFixSuggestion) (repo.AutoFixResult, error) {
	if r.buildAutoFixPatch != nil {
		return r.buildAutoFixPatch(ctx, repoPath, suggestions)
	}
	return r.repoSvc.BuildAutoFixPatch(ctx, repoPath, suggestions)
}

func (r *Runner) recordAutoFixOutcome(ctx context.Context, task db.ReviewTask, outcome, reason string) {
	note := "autofix " + outcome
	if reason != "" {
		note += ": " + reason
	}
	if err := r.store.RecordReviewNote(ctx, task.PatchEventID, task.RepoID, note); err != nil {
		r.logger.Error("autofix: failed to persist outcome",
			"patch_event_id", task.PatchEventID,
			"repo_id", task.RepoID,
			"outcome", outcome,
			"error", err)
	}
}

func changedFilesFromBundle(b contextbuilder.ContextBundle) []string {
	// Extract filenames from the layers used — approximate from bundle content
	var files []string
	for _, line := range strings.Split(b.Content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "diff --git ") {
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				f := strings.TrimPrefix(parts[3], "b/")
				files = append(files, f)
			}
		}
	}
	return files
}

func meanConfidence(findings []reviewengine.Finding) float64 {
	if len(findings) == 0 {
		return 0.5
	}
	sum := 0.0
	for _, f := range findings {
		sum += f.Confidence
	}
	return sum / float64(len(findings))
}

// modelName resolves a planner route to the model identifier configured for
// that route's endpoint, so published reviews report the model that actually
// served the request rather than the internal route alias.
func modelName(route reviewengine.ModelRoute, engine *reviewengine.Engine) string {
	if engine != nil {
		return engine.ModelForRoute(route)
	}
	return string(route)
}

// applyReviewPolicy filters findings by the repo-config severity floor and
// category restrictions. This is a deterministic post-filter — it ensures the
// published review matches repo policy regardless of LLM compliance.
func applyReviewPolicy(review reviewengine.ReviewerOutput, cfg repoconfig.RepoConfig) reviewengine.ReviewerOutput {
	filtered := make([]reviewengine.Finding, 0, len(review.Findings))
	suppressed := 0
	for _, f := range review.Findings {
		if !cfg.AllowsSeverity(f.Severity) || !cfg.AllowsCategory(f.Category) {
			suppressed++
			continue
		}
		filtered = append(filtered, f)
	}

	result := review
	result.Findings = filtered
	if suppressed > 0 {
		result.Summary += fmt.Sprintf("\n\nRepository review policy suppressed %d finding(s) outside configured severity/category scope.", suppressed)
	}
	return result
}
