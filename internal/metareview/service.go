package metareview

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"slices"
	"strings"

	"drydock/internal/db"
	"drydock/internal/embedding"
	"drydock/internal/reviewengine"
	"drydock/internal/vectorstore"
	"golang.org/x/sync/semaphore"
)

const (
	WhyMissedInsufficientContext = "insufficient_context"
	WhyMissedModelLimitation     = "model_limitation"
	WhyMissedPromptGap           = "prompt_gap"
	
	ActionFlagContextBuilder    = "flag-context-builder-pattern"
	ActionFlagModelRouting      = "flag-model-routing-review"
	ActionQueuePromptRefinement = "queue-prompt-refinement"
)

type LLMClient interface {
	ChatCompletion(ctx context.Context, req reviewengine.ChatRequest) (string, error)
}

type Config struct {
	Endpoint            reviewengine.ModelEndpoint
	RandomSampleRate    float64
	MinReuseJaccard     float64
	FewShotCap          int
	MaxConcurrent       int64
}

type Input struct {
	PatchEventID   string
	RepoID         string
	PatchDiff      string
	ContextBundle  string
	ContextHash    string
	ChangedFiles   []string
	LocalReview    reviewengine.ReviewerOutput
}

type Result struct {
	Triggered bool
	Reused    bool
	Reasons   []string
	Output    *MetaReviewOutput
}

type Service struct {
	cfg      Config
	store    *db.Store
	client   LLMClient
	logger   *slog.Logger
	sem      *semaphore.Weighted
	qdrant   *vectorstore.Client
	embedder *embedding.Client
}

// WithQdrant configures Qdrant + embedding clients for similarity-based
// few-shot upsert. When set, positive few-shot examples are embedded and
// upserted into the few_shot_reviews Qdrant collection.
func WithQdrant(qdrant *vectorstore.Client, embed *embedding.Client) func(*Service) {
	return func(s *Service) {
		s.qdrant = qdrant
		s.embedder = embed
	}
}

func New(cfg Config, store *db.Store, client LLMClient, logger *slog.Logger, opts ...func(*Service)) *Service {
	if cfg.RandomSampleRate <= 0 {
		cfg.RandomSampleRate = 0.15
	}
	if cfg.MinReuseJaccard <= 0 {
		cfg.MinReuseJaccard = 0.85
	}
	if cfg.FewShotCap <= 0 {
		cfg.FewShotCap = 500
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 10
	}
	s := &Service{
		cfg:    cfg,
		store:  store,
		client: client,
		logger: logger,
		sem:    semaphore.NewWeighted(cfg.MaxConcurrent),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Service) RunAsync(ctx context.Context, in Input) {
	go func() {
		if err := s.sem.Acquire(ctx, 1); err != nil {
			s.logger.Warn("meta-review semaphore acquire failed", "patch_event_id", in.PatchEventID, "error", err)
			return
		}
		defer s.sem.Release(1)
		
		if _, err := s.Run(ctx, in); err != nil {
			s.logger.Error("meta-review async run failed", "patch_event_id", in.PatchEventID, "repo_id", in.RepoID, "error", err)
		}
	}()
}

func (s *Service) Run(ctx context.Context, in Input) (Result, error) {
	reasons := gateReasons(in.LocalReview, in.ChangedFiles, s.cfg.RandomSampleRate)
	if len(reasons) == 0 {
		return Result{Triggered: false}, nil
	}
	changedLines := changedLineSet(in.PatchDiff)
	gateReason := strings.Join(reasons, ",")

	reuse, err := s.store.FindReusableMetaReview(ctx, in.ContextHash, changedLines, s.cfg.MinReuseJaccard)
	if err != nil {
		return Result{}, err
	}

	var parsed MetaReviewOutput
	if reuse != nil {
		parsed, err = ParseMetaReviewOutput(extractJSON(reuse.ResponseJSON))
		if err != nil {
			return Result{}, err
		}
		if err := s.routeFeedback(ctx, in, parsed); err != nil {
			return Result{}, err
		}
		if err := s.store.InsertMetaReviewLog(ctx, in.PatchEventID, in.RepoID, in.ContextHash, changedLines, gateReason, reuse.ResponseJSON); err != nil {
			return Result{}, err
		}
		return Result{Triggered: true, Reused: true, Reasons: reasons, Output: &parsed}, nil
	}

	req := reviewengine.ChatRequest{
		BaseURL:     s.cfg.Endpoint.BaseURL,
		APIKey:      s.cfg.Endpoint.APIKey,
		Model:       s.cfg.Endpoint.Model,
		Temperature: 0.1,
		System:      metaReviewSystemPrompt(),
		User:        metaReviewUserPrompt(in),
	}
	raw, err := s.client.ChatCompletion(ctx, req)
	if err != nil {
		return Result{}, fmt.Errorf("meta-review completion failed: %w", err)
	}
	parsed, err = ParseMetaReviewOutput(extractJSON(raw))
	if err != nil {
		return Result{}, err
	}

	if err := s.store.InsertMetaReviewLog(ctx, in.PatchEventID, in.RepoID, in.ContextHash, changedLines, gateReason, raw); err != nil {
		return Result{}, err
	}
	if err := s.routeFeedback(ctx, in, parsed); err != nil {
		return Result{}, err
	}
	if err := s.queuePromptGaps(ctx, in, parsed); err != nil {
		return Result{}, err
	}
	if err := s.updateFewShot(ctx, in, parsed); err != nil {
		return Result{}, err
	}

	return Result{Triggered: true, Reused: false, Reasons: reasons, Output: &parsed}, nil
}

func gateReasons(local reviewengine.ReviewerOutput, changedFiles []string, randomRate float64) []string {
	reasons := make([]string, 0, 3)
	if meanConfidence(local.Findings) < 0.7 {
		reasons = append(reasons, "mean-confidence-below-0.7")
	}
	for _, file := range changedFiles {
		l := strings.ToLower(file)
		if strings.Contains(l, "auth") || strings.Contains(l, "crypto") || strings.Contains(l, "security") {
			reasons = append(reasons, "security-sensitive-path")
			break
		}
	}
	if rand.Float64() < randomRate {
		reasons = append(reasons, "random-baseline-sample")
	}
	return dedupe(reasons)
}

func meanConfidence(findings []reviewengine.Finding) float64 {
	if len(findings) == 0 {
		return 1
	}
	sum := 0.0
	for _, f := range findings {
		sum += f.Confidence
	}
	return sum / float64(len(findings))
}

func (s *Service) routeFeedback(ctx context.Context, in Input, out MetaReviewOutput) error {
	for _, mf := range out.MissedFindings {
		action := ActionQueuePromptRefinement
		switch mf.WhyMissed {
		case WhyMissedInsufficientContext:
			action = ActionFlagContextBuilder
		case WhyMissedModelLimitation:
			action = ActionFlagModelRouting
		case WhyMissedPromptGap:
			action = ActionQueuePromptRefinement
		}
		if err := s.store.InsertMetaReviewRoute(ctx, in.PatchEventID, in.RepoID, mf.WhyMissed, action); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) queuePromptGaps(ctx context.Context, in Input, out MetaReviewOutput) error {
	for _, gap := range out.PromptGaps {
		gap = strings.TrimSpace(gap)
		if gap == "" {
			continue
		}
		if err := s.store.InsertPromptGap(ctx, in.PatchEventID, in.RepoID, gap); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) updateFewShot(ctx context.Context, in Input, out MetaReviewOutput) error {
	if out.SuggestedFewShot {
		payload := map[string]any{
			"patch_diff": in.PatchDiff,
			"meta_review": out,
			"local_review": in.LocalReview,
		}
		buf, _ := json.Marshal(payload)
		content := string(buf)
		if err := s.store.InsertFewShot(ctx, in.PatchEventID, in.RepoID, "positive", content, out.ReasoningQuality); err != nil {
			return err
		}

		// Embed and upsert into Qdrant for similarity-based retrieval.
		if s.qdrant != nil && s.embedder != nil && in.PatchDiff != "" {
			vec, err := s.embedder.Embed(ctx, in.PatchDiff)
			if err != nil {
				s.logger.Warn("failed to embed few-shot for Qdrant", "patch_event_id", in.PatchEventID, "error", err)
			} else {
				point := vectorstore.Point{
					ID:     in.PatchEventID,
					Vector: vec,
					Payload: map[string]any{
						"content":  content,
						"repo_id":  in.RepoID,
						"quality":  out.ReasoningQuality,
					},
				}
				if err := s.qdrant.Upsert(ctx, vectorstore.CollectionFewShot, []vectorstore.Point{point}); err != nil {
					s.logger.Warn("failed to upsert few-shot to Qdrant", "patch_event_id", in.PatchEventID, "error", err)
				}
			}
		}
	}
	if len(out.FalsePositives) > 0 {
		payload := map[string]any{
			"kind": "negative-pattern",
			"false_positives": out.FalsePositives,
		}
		buf, _ := json.Marshal(payload)
		if err := s.store.InsertFewShot(ctx, in.PatchEventID, in.RepoID, "negative", string(buf), out.ContextUtilization); err != nil {
			return err
		}
	}
	return s.store.PruneFewShotToCap(ctx, s.cfg.FewShotCap)
}

func metaReviewSystemPrompt() string {
	return `You are a meta-review evaluator.
Return JSON only with keys:
missed_findings, false_positives, reasoning_quality, context_utilization, prompt_gaps, suggested_few_shot.`
}

func metaReviewUserPrompt(in Input) string {
	localReviewJSON, _ := json.Marshal(in.LocalReview)
	return fmt.Sprintf(
		"Patch:\n%s\n\nContext:\n%s\n\nLocal review JSON:\n%s",
		in.PatchDiff, in.ContextBundle, string(localReviewJSON),
	)
}

func changedLineSet(diff string) []string {
	rows := strings.Split(diff, "\n")
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		if row[0] != '+' && row[0] != '-' {
			continue
		}
		if strings.HasPrefix(row, "+++") || strings.HasPrefix(row, "---") {
			continue
		}
		line := strings.TrimSpace(row[1:])
		if line != "" {
			lines = append(lines, line)
		}
	}
	slices.Sort(lines)
	lines = slices.Compact(lines)
	return lines
}

func dedupe(items []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item) == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func extractJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start >= 0 && end > start {
		return raw[start : end+1]
	}
	return raw
}

