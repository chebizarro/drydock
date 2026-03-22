package pipeline

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"

	"drydock/internal/contextbuilder"
	"drydock/internal/db"
	"drydock/internal/metareview"
	"drydock/internal/publisher"
	"drydock/internal/repo"
	"drydock/internal/reviewengine"
)

// Runner reads review tasks from a channel and executes the full pipeline:
// repo prepare → context build → LLM review → publish → meta-review.
type Runner struct {
	store      *db.Store
	repoSvc    *repo.Service
	ctxBuilder *contextbuilder.Builder
	engine     *reviewengine.Engine
	pubSvc     *publisher.Service
	metaSvc    *metareview.Service
	queue      <-chan db.ReviewTask
	workers    int
	logger     *slog.Logger
}

type Config struct {
	Workers int
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
) *Runner {
	workers := cfg.Workers
	if workers <= 0 {
		workers = 2
	}
	return &Runner{
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
}

// Run starts worker goroutines and blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	for i := 0; i < r.workers; i++ {
		go r.work(ctx, i)
	}
	<-ctx.Done()
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
			log.Info("processing review task", "patch_event_id", task.PatchEventID, "repo_id", task.RepoID)
			if err := r.process(ctx, task); err != nil {
				log.Error("review pipeline failed", "patch_event_id", task.PatchEventID, "repo_id", task.RepoID, "error", err)
				if markErr := r.store.MarkReviewFailed(ctx, task.PatchEventID, task.RepoID, err.Error()); markErr != nil {
					log.Error("failed to mark review as failed", "error", markErr)
				}
			}
		}
	}
}

func (r *Runner) process(ctx context.Context, task db.ReviewTask) error {
	// 1. Prepare repo + apply patch series
	prep, err := r.repoSvc.PreparePatchSeries(ctx, task.PatchEventID)
	if err != nil {
		return fmt.Errorf("prepare patch series: %w", err)
	}

	// 2. Get patch event content for context builder
	patchRec, err := r.store.GetPatchEvent(ctx, task.PatchEventID)
	if err != nil {
		return fmt.Errorf("get patch event: %w", err)
	}

	// 3. Build context bundle
	bundle, err := r.ctxBuilder.Build(ctx, contextbuilder.BuildInput{
		PatchEventContent: patchRec.RawEvent,
		RepoPath:          prep.RepoPath,
	})
	if err != nil {
		return fmt.Errorf("build context: %w", err)
	}

	// 4. Run LLM review engine
	result, err := r.engine.Run(ctx, reviewengine.RunInput{
		ContextBundle: bundle.Content,
		ChangedFiles:  changedFilesFromBundle(bundle),
	})
	if err != nil {
		return fmt.Errorf("review engine: %w", err)
	}

	// 5. Compute context hash
	ctxHash := fmt.Sprintf("%x", sha256.Sum256([]byte(bundle.Content)))

	// 6. Compute mean confidence
	confidence := meanConfidence(result.Review.Findings)

	// 7. Publish review
	reviewEventID, err := r.pubSvc.PublishReview(ctx, publisher.PublishInput{
		PatchEventID:         task.PatchEventID,
		RepoID:               task.RepoID,
		Summary:              result.Review.Summary,
		Findings:             result.Review.Findings,
		Model:                modelName(result.Route, r.engine),
		ContextHash:          ctxHash,
		Confidence:           confidence,
		ContextLayersUsed:    bundle.LayersUsed,
		ContextLayersDropped: bundle.LayersDropped,
		Superseded:           false,
	})
	if err != nil {
		return fmt.Errorf("publish review: %w", err)
	}

	// 8. Mark published
	if err := r.store.MarkReviewPublished(ctx, task.PatchEventID, task.RepoID, reviewEventID); err != nil {
		r.logger.Warn("failed to mark review published in db", "error", err)
	}

	r.logger.Info("review published",
		"patch_event_id", task.PatchEventID,
		"repo_id", task.RepoID,
		"review_event_id", reviewEventID,
		"findings", len(result.Review.Findings),
	)

	// 9. Async meta-review (non-blocking)
	if r.metaSvc != nil {
		r.metaSvc.RunAsync(ctx, metareview.Input{
			PatchEventID:  task.PatchEventID,
			RepoID:        task.RepoID,
			PatchDiff:     patchRec.RawEvent,
			ContextBundle: bundle.Content,
			ContextHash:   ctxHash,
			ChangedFiles:  changedFilesFromBundle(bundle),
			LocalReview:   result.Review,
		})
	}

	return nil
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
