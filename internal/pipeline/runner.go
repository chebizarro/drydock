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
	"drydock/internal/promptrefine"
	"drydock/internal/publisher"
	"drydock/internal/repo"
	"drydock/internal/repoconfig"
	"drydock/internal/reviewengine"

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
	queue            <-chan db.ReviewTask
	workers          int
	logger           *slog.Logger
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
			metrics.ReviewQueueDepth.Dec()
			metrics.ReviewsStarted.Inc()
			metrics.WorkersActive.Inc()
			done := metrics.Timer(metrics.ReviewDuration)
			log.Info("processing review task", "patch_event_id", task.PatchEventID, "repo_id", task.RepoID)
			if err := r.process(ctx, task); err != nil {
				metrics.ReviewsFinished.With("failed").Inc()
				log.Error("review pipeline failed", "patch_event_id", task.PatchEventID, "repo_id", task.RepoID, "error", err)
				if markErr := r.store.MarkReviewFailed(ctx, task.PatchEventID, task.RepoID, err.Error()); markErr != nil {
					log.Error("failed to mark review as failed", "error", markErr)
				}
			} else {
				metrics.ReviewsFinished.With("published").Inc()
			}
			done()
			metrics.WorkersActive.Dec()
		}
	}
}

func (r *Runner) process(ctx context.Context, task db.ReviewTask) error {
	// 1. Prepare repo + apply patch series
	prep, err := r.repoSvc.PreparePatchSeries(ctx, task.PatchEventID)
	if err != nil {
		// Publish a review comment about the apply failure so the patch author gets feedback
		if prep.FailureHint != "" && r.pubSvc != nil {
			r.publishApplyFailure(ctx, task, prep.FailureHint)
		}
		return fmt.Errorf("prepare patch series: %w", err)
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
			repoCfg = repoconfig.Default()
		}
	}

	// 1c. Index project documentation (non-fatal; skip if repo config disables docs).
	if r.docIngester != nil && repoCfg.DocsEnabled() {
		if err := r.docIngester.IngestRepoDocs(ctx, prep.RepoPath, task.RepoID); err != nil {
			r.logger.Warn("doc ingestion failed, continuing without",
				"repo_id", task.RepoID, "error", err)
		}
	}

	// 2. Get patch event for context builder and meta-review
	patchRec, err := r.store.GetPatchEvent(ctx, task.PatchEventID)
	if err != nil {
		return fmt.Errorf("get patch event: %w", err)
	}

	// 3. Extract actual diff content from the raw event.
	// The context builder expects unified diff content, not the JSON envelope.
	var patchEvent nostr.Event
	if err := json.Unmarshal([]byte(patchRec.RawEvent), &patchEvent); err != nil {
		return fmt.Errorf("decode patch event for context: %w", err)
	}
	patchDiffContent := patchEvent.Content

	// 3b. Validate that the patch diff is non-empty to avoid wasting an LLM call.
	if strings.TrimSpace(patchDiffContent) == "" {
		return fmt.Errorf("patch event %s has empty diff content", task.PatchEventID)
	}

	// 4. Build context bundle (with repo-config overrides)
	bundle, err := r.ctxBuilder.Build(ctx, contextbuilder.BuildInput{
		PatchEventContent:   patchDiffContent,
		RepoPath:            prep.RepoPath,
		RepoID:              task.RepoID,
		TokenBudgetOverride: repoCfg.Context.TokenBudget,
		ExcludePaths:        repoCfg.Context.ExcludePaths,
		DisableDocs:         !repoCfg.DocsEnabled(),
	})
	if err != nil {
		return fmt.Errorf("build context: %w", err)
	}

	// 5. Retrieve few-shot examples for reviewer prompt injection
	var fewShot []string
	if r.fewShotRetriever != nil {
		fewShot, err = r.fewShotRetriever.RetrieveFewShots(ctx, patchDiffContent, 2)
	} else {
		fewShot, err = r.store.GetRecentFewShots(ctx, 3)
	}
	if err != nil {
		r.logger.Warn("failed to retrieve few-shot examples, continuing without", "error", err)
		fewShot = nil
	}

	// 6. Run LLM review engine (with active prompt version override if available)
	var promptOverride string
	if r.promptRefiner != nil {
		promptOverride = r.promptRefiner.ActiveReviewerPrompt(ctx)
	}

	// 6b. Check if exclusions left no reviewable files.
	changedFiles := bundle.ChangedFiles
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

	result, err := r.engine.Run(ctx, reviewengine.RunInput{
		ContextBundle:                bundle.Content,
		ChangedFiles:                 changedFiles,
		FewShot:                      fewShot,
		ReviewerSystemPromptOverride: promptOverride,
		AdditionalInstructions:       repoCfg.PromptInstructions(),
		TestCoverageGaps:             bundle.TestCoverageGaps,
		SkipWalkthrough:              !repoCfg.WalkthroughEnabled(),
	})
	if err != nil {
		return fmt.Errorf("review engine: %w", err)
	}

	// 7. Compute context hash
	ctxHash := fmt.Sprintf("%x", sha256.Sum256([]byte(bundle.Content)))

	// 7b. Apply deterministic review policy filtering.
	filteredReview := applyReviewPolicy(result.Review, repoCfg)

	// 8. Compute mean confidence
	confidence := meanConfidence(filteredReview.Findings)

	// 9. Check if this patch has been superseded by a newer revision
	superseded := false
	if sup, err := r.store.IsPatchSuperseded(ctx, task.PatchEventID, patchRec.RootID, task.RepoID); err != nil {
		r.logger.Warn("failed to check superseded status, assuming not superseded",
			"patch_event_id", task.PatchEventID, "error", err)
	} else {
		superseded = sup
		if superseded {
			r.logger.Info("patch is superseded, using short TTL",
				"patch_event_id", task.PatchEventID, "root_id", patchRec.RootID)
		}
	}

	// 10. Publish review
	reviewEventID, err := r.pubSvc.PublishReview(ctx, publisher.PublishInput{
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
	if err != nil {
		return fmt.Errorf("publish review: %w", err)
	}

	// 11. Log success (MarkReviewPublished is already called inside PublishReview)
	r.logger.Info("review published",
		"patch_event_id", task.PatchEventID,
		"repo_id", task.RepoID,
		"review_event_id", reviewEventID,
		"findings", len(filteredReview.Findings),
	)

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

func modelName(route reviewengine.ModelRoute, _ *reviewengine.Engine) string {
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
