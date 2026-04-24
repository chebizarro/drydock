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
	"time"

	"drydock/internal/db"
	"drydock/internal/embedding"
	"drydock/internal/vectorstore"
)

func TestRecencyRetriever(t *testing.T) {
	ctx := context.Background()
	store := mustStoreForFewShot(t, ctx)

	if err := store.InsertFewShot(ctx, "evt1", "repo1", "positive", `{"example":"one"}`, 0.9); err != nil {
		t.Fatalf("insert: %v", err)
	}

	r := NewRecencyRetriever(store)
	results, err := r.RetrieveFewShots(ctx, FewShotQuery{
		PatchDiff: "any diff content",
		Limit:     5,
	})
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
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3}, "index": 0},
			},
		})
	}))
	defer embedSrv.Close()

	now := time.Now().Unix()
	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"result": []map[string]any{
				{
					"id": "hit1", "score": 0.95,
					"payload": map[string]any{
						"content":    `{"patch":"similar"}`,
						"repo_id":    "r1",
						"quality":    0.9,
						"language":   "go",
						"categories": []string{"error-handling"},
						"created_at": float64(now),
					},
				},
				{
					"id": "hit2", "score": 0.80,
					"payload": map[string]any{
						"content":    `{"patch":"also similar"}`,
						"repo_id":    "r2",
						"quality":    0.85,
						"language":   "go",
						"categories": []string{"performance"},
						"created_at": float64(now - 86400),
					},
				},
			},
		})
	}))
	defer qdrantSrv.Close()

	ctx := context.Background()
	store := mustStoreForFewShot(t, ctx)
	embedClient := embedding.NewClient(embedSrv.URL, "", "test-model")
	qdrantClient := vectorstore.NewClient(qdrantSrv.URL, "")

	retriever := NewQdrantRetriever(qdrantClient, embedClient, store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	results, err := retriever.RetrieveFewShots(ctx, FewShotQuery{
		PatchDiff: "some patch diff",
		Limit:     2,
		Language:  "go",
		RepoID:    "r1",
	})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Results should be formatted with metadata headers.
	if len(results[0]) == 0 {
		t.Fatal("expected non-empty formatted result")
	}
}

func TestQdrantRetrieverLanguageBoost(t *testing.T) {
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3}, "index": 0},
			},
		})
	}))
	defer embedSrv.Close()

	now := time.Now().Unix()
	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"result": []map[string]any{
				// Lower similarity but matching language.
				{
					"id": "hit1", "score": 0.70,
					"payload": map[string]any{
						"content":    `{"same_lang":"yes"}`,
						"quality":    0.9,
						"language":   "go",
						"created_at": float64(now),
					},
				},
				// Higher similarity but different language.
				{
					"id": "hit2", "score": 0.85,
					"payload": map[string]any{
						"content":    `{"diff_lang":"yes"}`,
						"quality":    0.9,
						"language":   "python",
						"created_at": float64(now),
					},
				},
			},
		})
	}))
	defer qdrantSrv.Close()

	ctx := context.Background()
	store := mustStoreForFewShot(t, ctx)
	embedClient := embedding.NewClient(embedSrv.URL, "", "test-model")
	qdrantClient := vectorstore.NewClient(qdrantSrv.URL, "")

	retriever := NewQdrantRetriever(qdrantClient, embedClient, store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	results, err := retriever.RetrieveFewShots(ctx, FewShotQuery{
		PatchDiff: "func main() {}",
		Limit:     1,
		Language:  "go",
	})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	// The Go example should rank higher due to language boost,
	// even though its raw similarity is lower.
	// hit1 composite: 0.70*0.5 + 0.9*0.2 + 1.0*0.15 + 1.0*0.15 = 0.35+0.18+0.15+0.15 = 0.83
	// hit2 composite: 0.85*0.5 + 0.9*0.2 + 1.0*0.15 + 0.3*0.15 = 0.425+0.18+0.15+0.045 = 0.80
	if !containsSubstring(results[0], "same_lang") {
		t.Errorf("expected same-language result to rank first, got: %s", results[0])
	}
}

func TestQdrantRetrieverFallsBackOnEmbedFailure(t *testing.T) {
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "embed error")
	}))
	defer embedSrv.Close()

	ctx := context.Background()
	store := mustStoreForFewShot(t, ctx)

	if err := store.InsertFewShot(ctx, "evt1", "repo1", "positive", `{"fallback":"yes"}`, 0.8); err != nil {
		t.Fatalf("insert: %v", err)
	}

	embedClient := embedding.NewClient(embedSrv.URL, "", "test-model")
	qdrantClient := vectorstore.NewClient("http://unused:6333", "")

	retriever := NewQdrantRetriever(qdrantClient, embedClient, store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	results, err := retriever.RetrieveFewShots(ctx, FewShotQuery{
		PatchDiff: "some diff",
		Limit:     2,
	})
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

	results, err := retriever.RetrieveFewShots(ctx, FewShotQuery{
		PatchDiff: "",
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(results) != 1 || results[0] != `{"recency":"example"}` {
		t.Fatalf("expected recency fallback, got: %v", results)
	}
}

func TestQdrantRetrieverLowSimilarityFiltered(t *testing.T) {
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3}, "index": 0},
			},
		})
	}))
	defer embedSrv.Close()

	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"result": []map[string]any{
				{
					"id": "hit1", "score": 0.2, // below minSimilarity
					"payload": map[string]any{
						"content": `{"low":"quality"}`,
					},
				},
			},
		})
	}))
	defer qdrantSrv.Close()

	ctx := context.Background()
	store := mustStoreForFewShot(t, ctx)

	if err := store.InsertFewShot(ctx, "evt1", "repo1", "positive", `{"fallback":"yes"}`, 0.8); err != nil {
		t.Fatalf("insert: %v", err)
	}

	embedClient := embedding.NewClient(embedSrv.URL, "", "test-model")
	qdrantClient := vectorstore.NewClient(qdrantSrv.URL, "")

	retriever := NewQdrantRetriever(qdrantClient, embedClient, store, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	results, err := retriever.RetrieveFewShots(ctx, FewShotQuery{
		PatchDiff: "some diff",
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	// Should fall back since all results are below threshold.
	if len(results) != 1 || results[0] != `{"fallback":"yes"}` {
		t.Fatalf("expected fallback for low-similarity results, got: %v", results)
	}
}

// --- Helper scoring tests ---

func TestRecencyScore(t *testing.T) {
	now := time.Now().Unix()

	// Very recent.
	score := recencyScore(now-3600, now) // 1 hour ago
	if score < 0.95 {
		t.Errorf("1-hour-old should be near 1.0, got %.3f", score)
	}

	// 30 days old.
	score = recencyScore(now-30*86400, now)
	if score < 0.3 || score > 0.4 {
		t.Errorf("30-day-old should be ~0.37, got %.3f", score)
	}

	// Unknown timestamp.
	score = recencyScore(0, now)
	if score != 0.5 {
		t.Errorf("unknown timestamp should return 0.5, got %.3f", score)
	}
}

func TestLanguageBoost(t *testing.T) {
	if languageBoost("go", "go") != 1.0 {
		t.Error("matching language should return 1.0")
	}
	if languageBoost("go", "python") != 0.3 {
		t.Error("different language should return 0.3")
	}
	if languageBoost("", "go") != 0.5 {
		t.Error("empty meta language should return 0.5")
	}
	if languageBoost("go", "") != 0.5 {
		t.Error("empty query language should return 0.5")
	}
}

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		files []string
		want  string
	}{
		{[]string{"main.go", "util.go", "test.py"}, "go"},
		{[]string{"app.py", "models.py"}, "python"},
		{[]string{"index.ts", "App.tsx"}, "typescript"},
		{[]string{"data.csv", "config.yml"}, ""}, // no supported lang
		{nil, ""},
	}
	for _, tt := range tests {
		got := DetectLanguage(tt.files)
		if got != tt.want {
			t.Errorf("DetectLanguage(%v) = %q, want %q", tt.files, got, tt.want)
		}
	}
}

func TestFormatFewShot(t *testing.T) {
	s := scoredResult{
		content: `{"example":"data"}`,
		metadata: fewShotMeta{
			Language:   "go",
			Quality:    0.85,
			Categories: []string{"error-handling", "security"},
		},
		score: 0.9,
	}
	formatted := formatFewShot(s)
	if !containsSubstring(formatted, "Language: go") {
		t.Error("formatted result should include language")
	}
	if !containsSubstring(formatted, "Quality: 0.85") {
		t.Error("formatted result should include quality")
	}
	if !containsSubstring(formatted, "error-handling, security") {
		t.Error("formatted result should include categories")
	}
	if !containsSubstring(formatted, `{"example":"data"}`) {
		t.Error("formatted result should include content")
	}
}

func TestFormatFewShotNoMetadata(t *testing.T) {
	s := scoredResult{
		content:  `{"example":"data"}`,
		metadata: fewShotMeta{},
		score:    0.5,
	}
	formatted := formatFewShot(s)
	// Should be just the raw content with no header.
	if formatted != `{"example":"data"}` {
		t.Errorf("expected raw content, got: %s", formatted)
	}
}

func TestExtractMeta(t *testing.T) {
	payload := map[string]any{
		"content":    "test content",
		"repo_id":    "repo-1",
		"quality":    float64(0.9),
		"language":   "go",
		"categories": []any{"security", "performance"},
		"created_at": float64(1234567890),
	}
	meta := extractMeta(payload)
	if meta.Language != "go" {
		t.Errorf("expected go, got %s", meta.Language)
	}
	if meta.RepoID != "repo-1" {
		t.Errorf("expected repo-1, got %s", meta.RepoID)
	}
	if meta.Quality != 0.9 {
		t.Errorf("expected 0.9, got %f", meta.Quality)
	}
	if len(meta.Categories) != 2 {
		t.Errorf("expected 2 categories, got %d", len(meta.Categories))
	}
	if meta.CreatedAt != 1234567890 {
		t.Errorf("expected 1234567890, got %d", meta.CreatedAt)
	}
}

func TestExtractMetaEmpty(t *testing.T) {
	meta := extractMeta(map[string]any{})
	if meta.Language != "" || meta.RepoID != "" || meta.Quality != 0 {
		t.Errorf("empty payload should yield zero meta, got: %+v", meta)
	}
}

func containsSubstring(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && len(s) >= len(substr) && (s == substr || findSubstring(s, substr))
}

func findSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
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
