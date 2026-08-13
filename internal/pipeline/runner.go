package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"drydock/internal/agenticreview"
	"drydock/internal/agenttools"
	"drydock/internal/contextbuilder"
	"drydock/internal/db"
	"drydock/internal/eventkind"
	"drydock/internal/metareview"
	"drydock/internal/metrics"
	"drydock/internal/payment"
	"drydock/internal/promptrefine"
	"drydock/internal/publisher"
	"drydock/internal/repo"
	"drydock/internal/repoconfig"
	"drydock/internal/reviewengine"
	"drydock/internal/reviewsession"
	"drydock/internal/scope"
	"drydock/internal/securityreview"
	"drydock/internal/securityscan"
	"drydock/internal/targetidentity"
	"drydock/internal/tracing"
	"drydock/internal/workspacesnapshot"

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

var (
	errPaymentBlockPersisted = errors.New("review payment blocked")
	errReactiveReviewSkipped = errors.New("reactive review skipped because repository is no longer monitored")
)

func retryablePaymentError(auth payment.AuthorizeResult) error {
	if !auth.Retryable {
		return nil
	}
	if auth.Reason != payment.ReasonPaymentPending {
		return fmt.Errorf("invalid retryable payment reason %q", auth.Reason)
	}
	return errors.New(payment.ReasonPaymentPending)
}

// MonitoringRegistry exposes the live reactive-review membership projection.
type MonitoringRegistry interface {
	Contains(repositoryAddress string) bool
}

// PaymentAuthorizer gates reviews according to the repository payment policy.
type PaymentAuthorizer interface {
	AuthorizePatch(ctx context.Context, patchEvent nostr.Event, repoID string, policy repoconfig.PaymentsConfig) (payment.AuthorizeResult, error)
}

// SecurityReviewStage runs the verified security lens over an assembled context bundle.
type SecurityReviewStage interface {
	Run(context.Context, contextbuilder.ContextBundle, string, repoconfig.SecurityConfig) securityreview.SecurityResult
}

type Runner struct {
	store                   *db.Store
	repoSvc                 *repo.Service
	ctxBuilder              *contextbuilder.Builder
	engine                  *reviewengine.Engine
	agenticSvc              *agenticreview.Service
	pubSvc                  *publisher.Service
	metaSvc                 *metareview.Service
	promptRefiner           PromptRefiner
	fewShotRetriever        FewShotRetriever
	docIngester             DocIngester
	codeIndexer             CodeIndexer
	secScanner              *securityscan.Scanner
	securityReviewer        SecurityReviewStage
	paymentAuth             PaymentAuthorizer
	monitoring              MonitoringRegistry
	queue                   <-chan db.ReviewTask
	workers                 int
	logger                  *slog.Logger
	activityHook            func()
	applyFailurePublication string
	agenticReviewFallback   bool

	// Narrow function seams keep failure handling testable without replacing the
	// concrete services used by the rest of the pipeline.
	isPatchSuperseded func(context.Context, string, string, string) (bool, error)
	publishStatus     func(context.Context, publisher.PublishStatusInput) (publisher.PublishStatusResult, error)
	buildAutoFixPatch func(context.Context, string, []repo.AutoFixSuggestion) (repo.AutoFixResult, error)
}

type Config struct {
	Workers                 int
	ApplyFailurePublication string
	// AgenticReviewFallback explicitly keeps the legacy deterministic
	// Builder.Build + Engine.Run path available during rollout.
	AgenticReviewFallback bool
}

const (
	ApplyFailurePublicationNotice   = "notice"
	ApplyFailurePublicationSuppress = "suppress"
)

// WithAgenticReviewService enables the default two-phase agentic review path.
func WithAgenticReviewService(service *agenticreview.Service) func(*Runner) {
	return func(r *Runner) {
		r.agenticSvc = service
	}
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

// WithSecurityReviewer enables the verified security review lens.
func WithSecurityReviewer(stage SecurityReviewStage) func(*Runner) {
	return func(r *Runner) {
		r.securityReviewer = stage
	}
}

// WithPaymentAuthorizer enables per-repository payment gating before expensive review work.
func WithPaymentAuthorizer(auth PaymentAuthorizer) func(*Runner) {
	return func(r *Runner) {
		r.paymentAuth = auth
	}
}

// WithMonitoringRegistry configures the live fail-closed reactive gate.
func WithMonitoringRegistry(registry MonitoringRegistry) func(*Runner) {
	return func(r *Runner) {
		r.monitoring = registry
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
	applyFailurePublication := strings.ToLower(strings.TrimSpace(cfg.ApplyFailurePublication))
	if applyFailurePublication == "" {
		applyFailurePublication = ApplyFailurePublicationNotice
	}
	r := &Runner{
		store:                   store,
		repoSvc:                 repoSvc,
		ctxBuilder:              ctxBuilder,
		engine:                  engine,
		pubSvc:                  pubSvc,
		metaSvc:                 metaSvc,
		queue:                   queue,
		workers:                 workers,
		logger:                  logger,
		applyFailurePublication: applyFailurePublication,
		agenticReviewFallback:   cfg.AgenticReviewFallback,
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

			taskLog.Info("processing review task", "invocation", task.Invocation)
			if err := r.process(taskCtx, task); err != nil {
				if errors.Is(err, errReactiveReviewSkipped) {
					metrics.ReviewsFinished.With("skipped").Inc()
					taskLog.Info("reactive review skipped after monitoring change",
						"error", err,
						"elapsed_ms", tracing.Elapsed(taskCtx).Milliseconds())
				} else {
					metrics.ReviewsFinished.With("failed").Inc()
					taskLog.Error("review pipeline failed",
						"error", err,
						"elapsed_ms", tracing.Elapsed(taskCtx).Milliseconds())
					if !errors.Is(err, errPaymentBlockPersisted) {
						if markErr := r.store.MarkReviewFailed(ctx, task.PatchEventID, task.RepoID, err.Error()); markErr != nil {
							taskLog.Error("failed to mark review as failed", "error", markErr)
						}
					}
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

	if err := r.requireReactiveMonitoring(ctx, task, "pipeline_start"); err != nil {
		return err
	}

	// 1. Prepare repo + apply patch series
	var prep repo.PrepareResult
	var prepErr error
	metrics.ReviewPrepareAttempts.Inc()
	timer.Time(tracing.StageRepoPrepare, func() error {
		prep, prepErr = r.repoSvc.PreparePatchSeries(ctx, task.PatchEventID)
		return prepErr
	})
	if prepErr != nil {
		// Preparation may have returned an owned worktree after a failed patch
		// apply. Retry its idempotent cleanup before leaving the task.
		defer r.repoSvc.CleanupPreparedReview(ctx, prep)
		// Preparation failures are operational outcomes, never model reviews.
		if prep.FailureStage != "" {
			metrics.ReviewApplyFailures.Inc()
			metrics.ReviewPrepareFailures.With(prep.FailureStage).Inc()
			if r.pubSvc != nil {
				if gateErr := r.requireReactiveMonitoring(ctx, task, "pre_publication"); gateErr != nil {
					return gateErr
				}
				hint := prep.FailureHint
				if strings.TrimSpace(hint) == "" {
					hint = prepErr.Error()
				}
				r.publishApplyFailure(ctx, task, prep.FailureStage, hint)
			}
		}
		return fmt.Errorf("prepare patch series: %w", prepErr)
	}
	// Hold the isolated review worktree through every filesystem-dependent
	// stage, then remove and prune it even if the task context was cancelled.
	defer r.repoSvc.CleanupPreparedReview(ctx, prep)

	if prep.RepoID != task.RepoID {
		return fmt.Errorf("prepared target identity mismatch: task repo %s, prepared %s", task.RepoID, prep.RepoID)
	}
	if prep.CanonicalRemoteIdentity == "" {
		return fmt.Errorf("prepared target identity missing canonical remote for repo %s", task.RepoID)
	}
	preparedPatch := false
	for _, eventID := range prep.AppliedIDs {
		if strings.EqualFold(eventID, task.PatchEventID) {
			preparedPatch = true
			break
		}
	}
	if !preparedPatch {
		return fmt.Errorf("prepared target identity mismatch: patch event %s was not prepared", task.PatchEventID)
	}

	// Persist PR diff provenance before any context construction or publication.
	if prep.DiffSHA256 != "" {
		if err := r.store.RecordReviewDiffProvenance(ctx, task.PatchEventID, task.RepoID, prep.BaseCommit, prep.TipCommit, prep.DiffSHA256); err != nil {
			return fmt.Errorf("persist PR diff provenance: %w", err)
		}
	}

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
	if !strings.EqualFold(patchRec.EventID, task.PatchEventID) {
		return fmt.Errorf("loaded patch identity mismatch: task %s, record %s", task.PatchEventID, patchRec.EventID)
	}
	if prep.RootID != patchRec.RootID {
		return fmt.Errorf("prepared target identity mismatch: patch root %s, prepared %s", patchRec.RootID, prep.RootID)
	}
	var patchEvent nostr.Event
	if err := json.Unmarshal([]byte(patchRec.RawEvent), &patchEvent); err != nil {
		return fmt.Errorf("decode patch event: %w", err)
	}
	if !strings.EqualFold(patchEvent.ID.Hex(), task.PatchEventID) {
		return fmt.Errorf("decoded patch identity mismatch: task %s, event %s", task.PatchEventID, patchEvent.ID.Hex())
	}

	// 1c2. Status gate: reviews run automatically only for roots whose
	// current NIP-34 status is allowed by repo config. An authorized explicit
	// force request is the sole bypass.
	if err := r.checkReviewStatus(ctx, task, patchRec.RootID, repoCfg.Review.Statuses); err != nil {
		return err
	}

	// 1d. Authorize payment-gated repositories before documentation/code indexing, context building, or LLM calls.
	if repoCfg.Payments.Enabled {
		if r.paymentAuth == nil {
			return fmt.Errorf("payment_blocked:payment_service_not_configured")
		}
		paymentAuthorized := false
		for attempt := 0; attempt < 3; attempt++ {
			auth, err := r.paymentAuth.AuthorizePatch(ctx, patchEvent, task.RepoID, repoCfg.Payments)
			if err != nil {
				return fmt.Errorf("authorize payment: %w", err)
			}
			if auth.Allowed {
				log.Info("review payment authorized",
					"patch_event_id", task.PatchEventID,
					"repo_id", task.RepoID,
					"access_kind", auth.AccessKind)
				paymentAuthorized = true
				break
			}
			if pendingErr := retryablePaymentError(auth); pendingErr != nil {
				// The worker records this canonical reason as an ordinary failed
				// review, so the durable retry sweep can pick it up.
				return pendingErr
			}
			advanced, err := r.store.MarkReviewPaymentBlocked(ctx, task.PatchEventID, task.RepoID, auth.Reason, auth.ZapReceiptCursor)
			if err != nil {
				return fmt.Errorf("persist payment block: %w", err)
			}
			if !advanced {
				return fmt.Errorf("%w: %s", errPaymentBlockPersisted, auth.Reason)
			}
			log.Info("zap receipt arrived during payment authorization; retrying",
				"patch_event_id", task.PatchEventID,
				"repo_id", task.RepoID)
		}
		if !paymentAuthorized {
			return errors.New("payment receipt churn")
		}
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
	patchDiffContent, err := patchDiffForReview(
		task.PatchEventID, patchRec.Kind, patchEvent.Content, prep.Diff,
	)
	if err != nil {
		return err
	}

	// 3b. Validate that the patch diff is non-empty to avoid wasting an LLM call.
	if strings.TrimSpace(patchDiffContent) == "" {
		return fmt.Errorf("patch event %s has empty diff content", task.PatchEventID)
	}

	if err := r.repoSvc.AssertPreparedReview(ctx, prep); err != nil {
		return fmt.Errorf("verify checkout before context build: %w", err)
	}

	// 4. Analyze the patch once through the shared facade. This is the
	// authoritative changed-file/exclusion set for preparation and few-shot
	// retrieval, independent of model behavior.
	analysis, err := contextbuilder.NewPatchFacade().Analyze(contextbuilder.PatchAnalysisRequest{
		Diff: patchDiffContent, ExcludePaths: repoCfg.Context.ExcludePaths,
	})
	if err != nil {
		return fmt.Errorf("analyze review patch: %w", err)
	}
	authoritativePatch := analysis.FilteredDiff
	patchDiffContent = authoritativePatch
	buildInput := contextbuilder.BuildInput{
		PatchEventContent: authoritativePatch, RepoPath: prep.RepoPath, RepoID: task.RepoID,
		TokenBudgetOverride: repoCfg.Context.TokenBudget, ExcludePaths: repoCfg.Context.ExcludePaths,
		DisableDocs: !repoCfg.DocsEnabled(),
	}

	var bundle contextbuilder.ContextBundle
	if len(analysis.ChangedFiles) == 0 && len(analysis.ExcludedFiles) > 0 {
		bundle = contextbuilder.ContextBundle{
			Content:       "No reviewable changes remain after repository path exclusions.",
			ExcludedFiles: append([]string(nil), analysis.ExcludedFiles...),
		}
	}
	var prepared *agenticreview.PreparedReview
	releasePrepared := func() {}
	if err := timer.Time(tracing.StageContextBuild, func() error {
		if repoCfg.Context.TokenBudget < 0 {
			return fmt.Errorf("invalid negative context token budget")
		}
		if bundle.Content != "" {
			return nil // pipeline-owned exclusion-only decision; no model call
		}
		if r.agenticReviewFallback {
			built, buildErr := r.ctxBuilder.Build(ctx, buildInput)
			if buildErr != nil {
				return buildErr
			}
			if r.agenticSvc != nil {
				bundle, buildErr = r.agenticSvc.GateDeterministicBundle(built)
			} else {
				// Compatibility-only construction used by legacy embedders still
				// receives an exact serialized-package gate; production always uses
				// the service-owned authoritative counter above.
				bundle, buildErr = agenttools.GateBundle(built, r.ctxBuilder.Counter, built.TokenBudget, agenttools.DefaultTokenHeadroom)
			}
			return buildErr
		}
		if r.agenticSvc == nil {
			return fmt.Errorf("agentic review service is required unless the explicit rollout fallback is enabled")
		}
		var prepareErr error
		prepared, prepareErr = r.agenticSvc.Prepare(ctx, agenticreview.PrepareInput{
			Mode: reviewsession.ModePatch,
			Snapshot: agenticreview.SnapshotSpec{
				Kind: workspacesnapshot.KindPinnedGit, RepoPath: prep.RepoPath,
				Ref: prep.ExpectedCommit, PatchRef: task.PatchEventID, Allowlist: analysis.ChangedFiles,
			},
			Patch: authoritativePatch, BuildInput: buildInput,
			Target: agenticreview.TargetInput{
				RepoID: task.RepoID, RootID: patchRec.RootID, PatchEventID: task.PatchEventID,
				CanonicalRemoteIdentity: prep.CanonicalRemoteIdentity, BaseCommit: prep.BaseCommit,
				TipCommit: prep.TipCommit, PreparedDiffSHA256: targetidentity.SHA256(authoritativePatch),
			},
		})
		if prepareErr == nil {
			bundle = prepared.Bundle()
			bundle.ExcludedFiles = append([]string(nil), analysis.ExcludedFiles...)
		}
		return prepareErr
	}); err != nil {
		return fmt.Errorf("prepare agentic context: %w", err)
	}
	if prepared != nil {
		released := false
		releasePrepared = func() {
			if !released {
				r.agenticSvc.ReleasePrepared(prepared)
				released = true
			}
		}
		defer releasePrepared()
	}
	for _, status := range bundle.LayerStatuses {
		metrics.ContextLayersByStatus.With(status.Status).Inc()
		if status.Status != "used" {
			r.logger.Warn("context layer not fully available", "layer", status.Layer, "status", status.Status, "message", status.Message, "tokens", status.Tokens)
		}
	}

	targetEnvelope := targetidentity.New(
		task.RepoID, patchRec.RootID, task.PatchEventID, prep.CanonicalRemoteIdentity,
		prep.BaseCommit, prep.TipCommit, targetidentity.SHA256(authoritativePatch), authoritativePatch, bundle.Content,
	)
	if err := targetEnvelope.VerifyMaterials(authoritativePatch, bundle.Content); err != nil {
		return fmt.Errorf("verify prepared target envelope: %w", err)
	}
	ctxHash, err := targetEnvelope.Hash()
	if err != nil {
		return fmt.Errorf("hash target identity envelope: %w", err)
	}

	// 5. Extract changed files from the context bundle (used for few-shot, engine, etc.).
	changedFiles := bundle.ChangedFiles
	if len(changedFiles) == 0 && len(bundle.ExcludedFiles) > 0 {
		// All changed files were excluded by repo policy — skip LLM call and
		// publish an explicit policy-skip review instead of failing.
		if err := r.requireReactiveMonitoring(ctx, task, "pre_publication"); err != nil {
			return err
		}
		reviewEventID, pubErr := r.pubSvc.PublishReview(ctx, publisher.PublishInput{
			PatchEventID:         task.PatchEventID,
			RepoID:               task.RepoID,
			Summary:              "This patch only modifies files excluded by repository review policy, so no automated review was run.",
			Model:                "policy",
			ContextHash:          ctxHash,
			TargetEnvelope:       targetEnvelope,
			ContextLayersUsed:    bundle.LayersUsed,
			ContextLayersDropped: bundle.LayersDropped,
			ExcludedFiles:        bundle.ExcludedFiles,
			BaseCommit:           prep.BaseCommit,
			TipCommit:            prep.TipCommit,
			DiffSHA256:           prep.DiffSHA256,
		})
		if pubErr != nil {
			return fmt.Errorf("publish exclusion-only review: %w", pubErr)
		}
		r.logger.Info("skipped LLM review (all files excluded by repo policy)",
			"patch_event_id", task.PatchEventID, "review_event_id", reviewEventID,
			"excluded_files", len(bundle.ExcludedFiles))
		return nil
	}
	if len(changedFiles) == 0 {
		// Fail closed: with no deterministic changed-file set, the reviewer
		// would be anchored to nothing but contextual layers and can present
		// documentation as modified files (seen when a kind-1618 cover letter
		// was parsed as a diff). A baseless review is worse than none.
		return fmt.Errorf("no changed files parsed from diff of patch event %s; refusing to review without a deterministic change set", task.PatchEventID)
	}

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

	// 6b. Run review engine (single model or ensemble mode)
	runInput := reviewengine.RunInput{
		ContextBundle:                bundle.Content,
		PatchDiff:                    patchDiffContent,
		TargetEnvelope:               targetEnvelope,
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
		if r.agenticReviewFallback {
			if repoCfg.Ensemble.Enabled {
				ensembleCfg := repoCfg.Ensemble.ToReviewEngineEnsembleConfig()
				result, reviewErr = r.engine.RunEnsemble(ctx, runInput, ensembleCfg)
			} else {
				result, reviewErr = r.engine.Run(ctx, runInput)
			}
		} else {
			if prepared == nil {
				return fmt.Errorf("agentic review preparation is missing")
			}
			options := agenticreview.ReviewOptions{
				ReviewerSystemPromptOverride: promptOverride,
				AdditionalInstructions:       repoCfg.PromptInstructions(), FewShot: fewShot,
				SkipWalkthrough: !repoCfg.WalkthroughEnabled(),
			}
			if repoCfg.Ensemble.Enabled {
				ensembleCfg := repoCfg.Ensemble.ToReviewEngineEnsembleConfig()
				options.Ensemble = &ensembleCfg
			}
			result, reviewErr = r.agenticSvc.ReviewPrepared(ctx, prepared, options)
		}
		if reviewErr != nil {
			return fmt.Errorf("review engine: %w", reviewErr)
		}
		if repoCfg.Ensemble.Enabled {
			log.Info("ensemble review completed",
				"models", len(repoCfg.Ensemble.Models), "findings", len(result.Review.Findings))
		}
		return nil
	}); err != nil {
		return err
	}

	// 6d. Run the verified security lens when explicitly enabled or when the
	// deterministic changed-file set contains a security-sensitive path.
	var verifiedSecurityFindings []reviewengine.Finding
	generalSecurity := repoCfg.Security.Enabled || reviewengine.IsSecuritySensitive(changedFiles)
	nostrConfigured := repoCfg.Security.Nostr.Enabled != "" && repoCfg.Security.Nostr.Enabled != "false"
	if r.securityReviewer != nil && (generalSecurity || nostrConfigured) {
		if err := r.repoSvc.AssertPreparedReview(ctx, prep); err != nil {
			return fmt.Errorf("verify checkout before security review: %w", err)
		}
		stageConfig := repoCfg.Security
		stageConfig.Enabled = generalSecurity
		securityResult := r.securityReviewer.Run(ctx, bundle, prep.RepoPath, stageConfig)
		if securityResult.Error != nil {
			return fmt.Errorf("security review: %w", securityResult.Error)
		}
		verifiedSecurityFindings = securityResult.Findings
		log.Info("security review completed", "findings", len(verifiedSecurityFindings))
	}

	// 6e. Run security scanner (deterministic SAST, parallel with LLM review is possible
	// but kept sequential here for simplicity and determinism).
	var scanFindings []securityscan.SecurityFinding
	if r.secScanner != nil && len(changedFiles) > 0 {
		if err := timer.Time(tracing.StageSecurityScan, func() error {
			if err := r.repoSvc.AssertPreparedReview(ctx, prep); err != nil {
				return fmt.Errorf("verify checkout before security scan: %w", err)
			}
			scanResult := r.secScanner.ScanFiles(ctx, prep.RepoPath, changedFiles, patchDiffContent)
			scanFindings = scanResult.Findings
			if len(scanFindings) > 0 {
				metrics.SecurityScanFindings.Add(int64(len(scanFindings)))
				log.Info("security scan complete",
					"files_scanned", scanResult.FilesScanned,
					"findings", len(scanFindings))
			}
			return nil
		}); err != nil {
			return err
		}
	}

	// 7b. Deduplicate scanner findings with LLM findings, then apply review policy.
	llmFindings := append(result.Review.Findings, verifiedSecurityFindings...)
	mergedFindings := securityscan.DeduplicateFindings(scanFindings, llmFindings)
	mergedReview := result.Review
	mergedReview.Findings = mergedFindings
	filteredReview := applyReviewPolicy(mergedReview, repoCfg)
	filteredReview, result.Walkthrough, err = reviewengine.FilterOutputToChangedFiles(
		filteredReview, result.Walkthrough, changedFiles, targetEnvelope,
		patchDiffContent, bundle.Content, log,
	)
	if err != nil {
		return fmt.Errorf("final target identity filter: %w", err)
	}

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
		if err := r.requireReactiveMonitoring(ctx, task, "pre_publication"); err != nil {
			return err
		}
		var pubErr error
		reviewEventID, pubErr = r.pubSvc.PublishReview(ctx, publisher.PublishInput{
			PatchEventID:         task.PatchEventID,
			RepoID:               task.RepoID,
			Summary:              filteredReview.Summary,
			Findings:             filteredReview.Findings,
			Model:                modelName(result, r.engine),
			ContextHash:          ctxHash,
			TargetEnvelope:       targetEnvelope,
			Confidence:           confidence,
			ContextLayersUsed:    bundle.LayersUsed,
			ContextLayersDropped: bundle.LayersDropped,
			ExcludedFiles:        bundle.ExcludedFiles,
			BaseCommit:           prep.BaseCommit,
			TipCommit:            prep.TipCommit,
			DiffSHA256:           prep.DiffSHA256,
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

	statusFindings, statusConfidence, statusPolicy := statusPublishParameters(filteredReview.Findings, verifiedSecurityFindings, repoCfg)

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
				Findings:      statusFindings,
				Model:         modelName(result, r.engine),
				Confidence:    statusConfidence,
				Superseded:    superseded,
				Policy:        statusPolicy,
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
		fixResult := r.tryAutoFix(ctx, task, prep, filteredReview, repoCfg, reviewEventID, modelName(result, r.engine))
		if fixResult != nil && fixResult.Published {
			r.logger.Info("auto-fix patch published",
				"patch_event_id", task.PatchEventID,
				"fix_event_id", fixResult.EventID,
				"applied_count", fixResult.AppliedCount)
		}
	}

	// All filesystem-dependent stages are complete. Release the retained
	// prepared-snapshot lease before the asynchronous meta-review begins.
	releasePrepared()

	// 12. Async meta-review (non-blocking, uses filtered review and agent trace metadata).
	if r.metaSvc != nil {
		var discoveryTrace any
		if prepared != nil {
			discoveryTrace = prepared.DiscoveryTrace()
		}
		r.metaSvc.RunAsync(ctx, metareview.Input{
			PatchEventID:     task.PatchEventID,
			RepoID:           task.RepoID,
			PatchDiff:        authoritativePatch,
			ContextBundle:    bundle.Content,
			ContextHash:      ctxHash,
			ChangedFiles:     changedFiles,
			LocalReview:      filteredReview,
			SecurityFindings: metareview.ConfirmedSecurityFindings(verifiedSecurityFindings),
			DiscoveryTrace:   discoveryTrace,
			AgentTrace:       result.ReviewerTrace,
			EnsembleTraces:   append([]reviewengine.EnsembleReviewerTrace(nil), result.EnsembleStatus.ReviewerTraces...),
		})
	}

	return nil
}

func patchDiffForReview(patchEventID string, kind int, eventContent, preparedDiff string) (string, error) {
	switch kind {
	case 1617:
		// Revision-scoped patch reviews always prompt with the requested
		// event's diff. Applied ancestors only establish its worktree state.
		return eventContent, nil
	case 1618, 1619:
		if strings.TrimSpace(preparedDiff) == "" {
			return "", fmt.Errorf("PR event %s (kind %d) produced no diff against its base", patchEventID, kind)
		}
		return preparedDiff, nil
	default:
		return "", fmt.Errorf("patch event %s has unsupported review kind %d", patchEventID, kind)
	}
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

func (r *Runner) requireReactiveMonitoring(ctx context.Context, task db.ReviewTask, stage string) error {
	if task.Invocation != "" && task.Invocation != db.ReviewInvocationReactive {
		return nil
	}
	repositoryAddress := fmt.Sprintf("%d:%s", scope.RepositoryAnnouncementKind, task.RepoID)
	if _, err := scope.ParseRepositoryRef(repositoryAddress); err == nil && r.monitoring != nil && r.monitoring.Contains(repositoryAddress) {
		return nil
	}
	if err := r.store.MarkReviewSkipped(ctx, task.PatchEventID, task.RepoID, "monitoring_removed"); err != nil {
		return fmt.Errorf("persist reactive monitoring skip at %s: %w", stage, err)
	}
	return fmt.Errorf("%w at %s", errReactiveReviewSkipped, stage)
}

func (r *Runner) publishApplyFailure(ctx context.Context, task db.ReviewTask, stage, hint string) {
	if r.applyFailurePublication == ApplyFailurePublicationSuppress {
		metrics.FailureNoticesSuppressed.Inc()
		r.logger.Info("suppressed apply-failure operational notice",
			"patch_event_id", task.PatchEventID,
			"repo_id", task.RepoID,
		)
		return
	}
	_, err := r.pubSvc.PublishFailureNotice(ctx, publisher.PublishFailureNoticeInput{
		PatchEventID: task.PatchEventID,
		RepoID:       task.RepoID,
		FailureStage: stage,
		Reason:       hint,
	})
	if err != nil {
		r.logger.Warn("failed to publish apply-failure operational notice",
			"patch_event_id", task.PatchEventID,
			"repo_id", task.RepoID,
			"error", err,
		)
	} else {
		r.logger.Info("published apply-failure operational notice",
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

// statusPublishParameters keeps unverified security findings out of status
// decisions. Confirmed security findings use the security gate thresholds;
// all other findings retain the existing status block semantics.
func statusPublishParameters(all, verifiedSecurity []reviewengine.Finding, cfg repoconfig.RepoConfig) ([]reviewengine.Finding, float64, publisher.StatusPolicy) {
	securityGates := make([]reviewengine.Finding, 0, len(verifiedSecurity))
	for _, finding := range verifiedSecurity {
		if !cfg.AllowsSeverity(finding.Severity) || !cfg.AllowsCategory("security") {
			continue
		}
		if finding.Confidence >= cfg.Security.MinConfidence && reviewengine.IsAtOrAboveSeverity(finding.Severity, cfg.Security.GateSeverity) {
			securityGates = append(securityGates, finding)
		}
	}
	if len(securityGates) > 0 {
		return securityGates, meanConfidence(securityGates), publisher.StatusPolicy{
			Enabled:           cfg.Status.Enabled,
			OpenSeverityFloor: cfg.Security.GateSeverity,
			MinConfidence:     cfg.Security.MinConfidence,
		}
	}

	nonSecurity := make([]reviewengine.Finding, 0, len(all))
	for _, finding := range all {
		if finding.Category != "security" {
			nonSecurity = append(nonSecurity, finding)
		}
	}
	return nonSecurity, meanConfidence(nonSecurity), publisher.StatusPolicy{
		Enabled:           cfg.Status.Enabled,
		OpenSeverityFloor: cfg.Status.OpenSeverityFloor,
		MinConfidence:     cfg.Status.MinConfidence,
	}
}

func (r *Runner) checkReviewStatus(ctx context.Context, task db.ReviewTask, rootID string, allowedStatuses []string) error {
	if task.Force {
		r.logger.Info("bypassing root status gate for authorized forced review",
			"patch_event_id", task.PatchEventID, "repo_id", task.RepoID)
		return nil
	}
	statusKind, _, _, hasStatus, err := r.store.GetRootStatus(ctx, rootID, task.RepoID)
	if err != nil {
		return fmt.Errorf("get root status: %w", err)
	}
	if reason, allowed := reviewStatusAllowed(statusKind, hasStatus, allowedStatuses); !allowed {
		r.logger.Info("skipping review for root status",
			"patch_event_id", task.PatchEventID, "repo_id", task.RepoID, "reason", reason)
		return fmt.Errorf("status_skipped:%s", reason)
	}
	return nil
}

// reviewStatusAllowed reports whether a root with the given NIP-34 status may
// be reviewed automatically under the configured allowed statuses. A root
// with no status event counts as open. Applied/merged (1631) and closed
// (1632) roots are never allowed regardless of configuration.
func reviewStatusAllowed(statusKind int, hasStatus bool, allowedStatuses []string) (reason string, allowed bool) {
	status := "open"
	if hasStatus {
		switch nostr.Kind(statusKind) {
		case eventkind.StatusOpen:
			status = "open"
		case eventkind.StatusApplied:
			return "root status is applied/merged (1631)", false
		case eventkind.StatusClosed:
			return "root status is closed (1632)", false
		case eventkind.StatusDraft:
			status = "draft"
		default:
			return fmt.Sprintf("root has unknown status kind %d", statusKind), false
		}
	}
	for _, a := range allowedStatuses {
		if a == status {
			return "", true
		}
	}
	return fmt.Sprintf("root status %q is not in configured review statuses %v", status, allowedStatuses), false
}

// modelName returns the label for a published review: the model the reviewer
// endpoint reported serving for this specific run when known, otherwise the
// served-model registry / configured model for the route, otherwise the route
// alias. Never the internal route alias when better information exists.
func modelName(result reviewengine.RunOutput, engine *reviewengine.Engine) string {
	if served := strings.TrimSpace(result.ServedModel); served != "" {
		return served
	}
	if engine != nil {
		return engine.ModelForRoute(result.Route)
	}
	return string(result.Route)
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
