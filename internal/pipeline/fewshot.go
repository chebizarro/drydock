package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"drydock/internal/db"
	"drydock/internal/embedding"
	"drydock/internal/symbols"
	"drydock/internal/vectorstore"
)

// FewShotQuery describes the context for a few-shot retrieval request.
type FewShotQuery struct {
	PatchDiff string // unified diff of the patch
	Limit     int    // max examples to return
	Language  string // primary language of the changed files
	RepoID    string // current repo (reserved for future same-repo affinity)
}

// FewShotRetriever retrieves few-shot examples for a given review context.
type FewShotRetriever interface {
	RetrieveFewShots(ctx context.Context, query FewShotQuery) ([]string, error)
}

// RecencyFewShotRetriever retrieves the most recent positive few-shot examples
// from the database. This is the fallback when Qdrant is not configured.
type RecencyFewShotRetriever struct {
	store *db.Store
}

func NewRecencyRetriever(store *db.Store) *RecencyFewShotRetriever {
	return &RecencyFewShotRetriever{store: store}
}

func (r *RecencyFewShotRetriever) RetrieveFewShots(ctx context.Context, query FewShotQuery) ([]string, error) {
	return r.store.GetRecentFewShots(ctx, query.Limit)
}

// QdrantFewShotRetriever embeds the patch diff and queries Qdrant for the
// most similar few-shot examples. Results are scored using a weighted
// combination of semantic similarity, quality, recency, and language affinity.
// Falls back to recency-based retrieval if embedding or search fails.
type QdrantFewShotRetriever struct {
	qdrant   *vectorstore.Client
	embedder *embedding.Client
	store    *db.Store
	logger   *slog.Logger
}

func NewQdrantRetriever(qdrant *vectorstore.Client, embedder *embedding.Client, store *db.Store, logger *slog.Logger) *QdrantFewShotRetriever {
	return &QdrantFewShotRetriever{
		qdrant:   qdrant,
		embedder: embedder,
		store:    store,
		logger:   logger,
	}
}

// Weighting factors for composite scoring.
const (
	weightSimilarity = 0.50
	weightQuality    = 0.20
	weightRecency    = 0.15
	weightLanguage   = 0.15

	// Qdrant returns more results than needed so we can post-filter and rank.
	searchOverfetch = 20
	// Minimum similarity score to consider a result.
	minSimilarity = 0.4
)

// scoredResult is a Qdrant hit with a composite score.
type scoredResult struct {
	content  string
	metadata fewShotMeta
	score    float64
}

// fewShotMeta is the metadata stored on each Qdrant few-shot point.
type fewShotMeta struct {
	Language   string   `json:"language"`
	RepoID     string   `json:"repo_id"`
	Quality    float64  `json:"quality"`
	Categories []string `json:"categories"`
	CreatedAt  int64    `json:"created_at"`
}

func (r *QdrantFewShotRetriever) RetrieveFewShots(ctx context.Context, query FewShotQuery) ([]string, error) {
	if query.Limit <= 0 {
		return nil, nil
	}
	if query.PatchDiff == "" {
		return r.fallback(ctx, query.Limit)
	}

	// Truncate diff for embedding (same cap as code index provider).
	diff := query.PatchDiff
	if len(diff) > 8*1024 {
		diff = diff[:8*1024]
	}

	vec, err := r.embedder.Embed(ctx, diff)
	if err != nil {
		r.logger.Warn("few-shot embedding failed, falling back to recency", "error", err)
		return r.fallback(ctx, query.Limit)
	}

	// Fetch more results than needed to allow for post-filtering and re-ranking.
	fetchLimit := query.Limit + searchOverfetch
	results, err := r.qdrant.Search(ctx, vectorstore.CollectionFewShot, vec, fetchLimit, nil)
	if err != nil {
		r.logger.Warn("Qdrant few-shot search failed, falling back to recency", "error", err)
		return r.fallback(ctx, query.Limit)
	}

	if len(results) == 0 {
		return r.fallback(ctx, query.Limit)
	}

	now := time.Now().Unix()
	var scored []scoredResult

	for _, res := range results {
		if res.Score < minSimilarity {
			continue
		}

		content, _ := res.Payload["content"].(string)
		if content == "" {
			continue
		}

		meta := extractMeta(res.Payload)

		// Composite score: similarity * w1 + quality * w2 + recency * w3 + lang * w4
		similarity := float64(res.Score)
		quality := meta.Quality
		if quality <= 0 {
			quality = 0.5 // default quality if not set
		}
		recency := recencyScore(meta.CreatedAt, now)
		langBoost := languageBoost(meta.Language, query.Language)

		composite := similarity*weightSimilarity +
			quality*weightQuality +
			recency*weightRecency +
			langBoost*weightLanguage

		scored = append(scored, scoredResult{
			content:  content,
			metadata: meta,
			score:    composite,
		})
	}

	if len(scored) == 0 {
		return r.fallback(ctx, query.Limit)
	}

	// Sort by composite score descending.
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	// Take top results.
	if len(scored) > query.Limit {
		scored = scored[:query.Limit]
	}

	// Format with metadata headers for the reviewer model.
	shots := make([]string, 0, len(scored))
	for _, s := range scored {
		formatted := formatFewShot(s)
		shots = append(shots, formatted)
	}

	topScore := 0.0
	if len(scored) > 0 {
		topScore = scored[0].score
	}
	r.logger.Info("retrieved weighted few-shot examples from Qdrant",
		"count", len(shots),
		"top_score", fmt.Sprintf("%.3f", topScore),
		"language_filter", query.Language,
	)
	return shots, nil
}

func (r *QdrantFewShotRetriever) fallback(ctx context.Context, limit int) ([]string, error) {
	return r.store.GetRecentFewShots(ctx, limit)
}

// extractMeta reads few-shot metadata from a Qdrant payload.
func extractMeta(payload map[string]any) fewShotMeta {
	meta := fewShotMeta{}
	if v, ok := payload["language"].(string); ok {
		meta.Language = v
	}
	if v, ok := payload["repo_id"].(string); ok {
		meta.RepoID = v
	}
	if v, ok := payload["quality"].(float64); ok {
		meta.Quality = v
	}
	if v, ok := payload["created_at"].(float64); ok {
		meta.CreatedAt = int64(v)
	}
	// Categories may be stored as []any.
	if cats, ok := payload["categories"].([]any); ok {
		for _, c := range cats {
			if s, ok := c.(string); ok {
				meta.Categories = append(meta.Categories, s)
			}
		}
	}
	return meta
}

// recencyScore returns a 0–1 score that decays over time.
// Full score (1.0) for examples < 1 day old, decays to ~0.3 at 30 days.
func recencyScore(createdAt, now int64) float64 {
	if createdAt <= 0 {
		return 0.5 // unknown creation time
	}
	ageDays := float64(now-createdAt) / 86400.0
	if ageDays < 0 {
		ageDays = 0
	}
	// Exponential decay: e^(-age/30), clamped to [0.1, 1.0].
	score := math.Exp(-ageDays / 30.0)
	if score < 0.1 {
		score = 0.1
	}
	return score
}

// languageBoost returns 1.0 if languages match, 0.3 otherwise.
// Empty language on either side returns 0.5 (neutral).
func languageBoost(metaLang, queryLang string) float64 {
	if metaLang == "" || queryLang == "" {
		return 0.5
	}
	if strings.EqualFold(metaLang, queryLang) {
		return 1.0
	}
	return 0.3
}

// formatFewShot wraps a few-shot example with a metadata header so the
// reviewer model understands the context of the example.
func formatFewShot(s scoredResult) string {
	var parts []string
	if s.metadata.Language != "" {
		parts = append(parts, "Language: "+s.metadata.Language)
	}
	if s.metadata.Quality > 0 {
		parts = append(parts, fmt.Sprintf("Quality: %.2f", s.metadata.Quality))
	}
	if len(s.metadata.Categories) > 0 {
		parts = append(parts, "Categories: "+strings.Join(s.metadata.Categories, ", "))
	}

	header := ""
	if len(parts) > 0 {
		header = "[" + strings.Join(parts, " | ") + "]\n"
	}
	return header + s.content
}

// DetectLanguage returns the primary language from a list of changed file paths
// by counting file extensions. On ties, the first-seen language wins for
// determinism. Returns empty string if no supported language is detected.
func DetectLanguage(changedFiles []string) string {
	counts := make(map[string]int)
	var order []string // first-seen order for deterministic tie-breaking
	for _, f := range changedFiles {
		ext := strings.ToLower(filepath.Ext(f))
		lang := symbols.LangFromExt(ext)
		if lang != "" {
			if counts[lang] == 0 {
				order = append(order, lang)
			}
			counts[lang]++
		}
	}
	if len(counts) == 0 {
		return ""
	}
	// Return the most common language; first-seen wins on ties.
	best := ""
	bestCount := 0
	for _, lang := range order {
		if counts[lang] > bestCount {
			best = lang
			bestCount = counts[lang]
		}
	}
	return best
}


