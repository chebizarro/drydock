package pipeline

import (
	"context"
	"fmt"
	"log/slog"

	"drydock/internal/db"
	"drydock/internal/embedding"
	"drydock/internal/vectorstore"
)

// FewShotRetriever retrieves few-shot examples for a given patch diff.
type FewShotRetriever interface {
	RetrieveFewShots(ctx context.Context, patchDiff string, limit int) ([]string, error)
}

// RecencyFewShotRetriever retrieves the most recent positive few-shot examples
// from the database. This is the fallback when Qdrant is not configured.
type RecencyFewShotRetriever struct {
	store *db.Store
}

func NewRecencyRetriever(store *db.Store) *RecencyFewShotRetriever {
	return &RecencyFewShotRetriever{store: store}
}

func (r *RecencyFewShotRetriever) RetrieveFewShots(ctx context.Context, _ string, limit int) ([]string, error) {
	return r.store.GetRecentFewShots(ctx, limit)
}

// QdrantFewShotRetriever embeds the patch diff and queries Qdrant for the
// most similar few-shot examples. Falls back to recency-based retrieval if
// the embedding or search fails.
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

func (r *QdrantFewShotRetriever) RetrieveFewShots(ctx context.Context, patchDiff string, limit int) ([]string, error) {
	if patchDiff == "" {
		return r.fallback(ctx, limit)
	}

	vec, err := r.embedder.Embed(ctx, patchDiff)
	if err != nil {
		r.logger.Warn("few-shot embedding failed, falling back to recency", "error", err)
		return r.fallback(ctx, limit)
	}

	results, err := r.qdrant.Search(ctx, vectorstore.CollectionFewShot, vec, limit, nil)
	if err != nil {
		r.logger.Warn("Qdrant few-shot search failed, falling back to recency", "error", err)
		return r.fallback(ctx, limit)
	}

	if len(results) == 0 {
		return r.fallback(ctx, limit)
	}

	shots := make([]string, 0, len(results))
	for _, res := range results {
		content, ok := res.Payload["content"].(string)
		if !ok || content == "" {
			continue
		}
		shots = append(shots, content)
	}

	if len(shots) == 0 {
		return r.fallback(ctx, limit)
	}

	r.logger.Info("retrieved few-shot examples from Qdrant",
		"count", len(shots),
		"top_score", fmt.Sprintf("%.3f", results[0].Score),
	)
	return shots, nil
}

func (r *QdrantFewShotRetriever) fallback(ctx context.Context, limit int) ([]string, error) {
	return r.store.GetRecentFewShots(ctx, limit)
}
