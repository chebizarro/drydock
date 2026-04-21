package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"drydock/internal/db"
	"drydock/internal/embedding"
	"drydock/internal/vectorstore"
)

func TestRecencyRetriever(t *testing.T) {
	ctx := context.Background()
	store := mustStoreForFewShot(t, ctx)

	// Insert a few-shot example.
	if err := store.InsertFewShot(ctx, "evt1", "repo1", "positive", `{"example":"one"}`, 0.9); err != nil {
		t.Fatalf("insert: %v", err)
	}

	r := NewRecencyRetriever(store)
	results, err := r.RetrieveFewShots(ctx, "any diff content", 5)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0] != `{"example":"one"}` {
		t.Fatalf("unexpected content: %s", results[0])
	}
}

func TestQdrantRetrieverSuccess(t *testing.T) {
	// Mock embedding server.
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3}, "index": 0},
			},
		})
	}))
	defer embedSrv.Close()

	// Mock Qdrant search server.
	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"result": []map[string]any{
				{"id": "hit1", "score": 0.95, "payload": map[string]any{"content": `{"patch":"similar"}`, "repo_id": "r1"}},
				{"id": "hit2", "score": 0.80, "payload": map[string]any{"content": `{"patch":"also similar"}`, "repo_id": "r1"}},
			},
		})
	}))
	defer qdrantSrv.Close()

	ctx := context.Background()
	store := mustStoreForFewShot(t, ctx)
	embedClient := embedding.NewClient(embedSrv.URL, "", "test-model")
	qdrantClient := vectorstore.NewClient(qdrantSrv.URL, "")

	retriever := NewQdrantRetriever(qdrantClient, embedClient, store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	results, err := retriever.RetrieveFewShots(ctx, "some patch diff", 2)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0] != `{"patch":"similar"}` {
		t.Fatalf("unexpected first result: %s", results[0])
	}
}

func TestQdrantRetrieverFallsBackOnEmbedFailure(t *testing.T) {
	// Embedding server that always fails.
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "embed error")
	}))
	defer embedSrv.Close()

	ctx := context.Background()
	store := mustStoreForFewShot(t, ctx)

	// Insert a recency-based example for fallback.
	if err := store.InsertFewShot(ctx, "evt1", "repo1", "positive", `{"fallback":"yes"}`, 0.8); err != nil {
		t.Fatalf("insert: %v", err)
	}

	embedClient := embedding.NewClient(embedSrv.URL, "", "test-model")
	qdrantClient := vectorstore.NewClient("http://unused:6333", "")

	retriever := NewQdrantRetriever(qdrantClient, embedClient, store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	results, err := retriever.RetrieveFewShots(ctx, "some diff", 2)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(results) != 1 || results[0] != `{"fallback":"yes"}` {
		t.Fatalf("expected fallback result, got: %v", results)
	}
}

func TestQdrantRetrieverFallsBackOnEmptyDiff(t *testing.T) {
	ctx := context.Background()
	store := mustStoreForFewShot(t, ctx)

	if err := store.InsertFewShot(ctx, "evt1", "repo1", "positive", `{"recency":"example"}`, 0.8); err != nil {
		t.Fatalf("insert: %v", err)
	}

	retriever := NewQdrantRetriever(nil, nil, store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	results, err := retriever.RetrieveFewShots(ctx, "", 2)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(results) != 1 || results[0] != `{"recency":"example"}` {
		t.Fatalf("expected recency fallback, got: %v", results)
	}
}

func mustStoreForFewShot(t *testing.T, ctx context.Context) *db.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
