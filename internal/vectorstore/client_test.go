package vectorstore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnsureCollection_AlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/collections/test_col" {
			json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{
					"status":       "green",
					"points_count": 42,
					"config": map[string]any{
						"params": map[string]any{
							"vectors": map[string]any{"size": 384},
						},
					},
				},
			})
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	err := c.EnsureCollection(context.Background(), "test_col", 384)
	if err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
}

func TestEnsureCollection_Creates(t *testing.T) {
	created := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/collections/test_col" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"status":{"error":"not found"}}`))
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/collections/test_col" {
			created = true
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			vectors, ok := body["vectors"].(map[string]any)
			if !ok {
				t.Error("expected vectors in request body")
			}
			if vectors["distance"] != "Cosine" {
				t.Errorf("expected Cosine distance, got %v", vectors["distance"])
			}
			json.NewEncoder(w).Encode(map[string]any{"result": true})
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	err := c.EnsureCollection(context.Background(), "test_col", 384)
	if err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	if !created {
		t.Fatal("expected collection to be created")
	}
}

func TestGetCollection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"status":       "green",
				"points_count": 100,
				"config": map[string]any{
					"params": map[string]any{
						"vectors": map[string]any{"size": 768},
					},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	info, err := c.GetCollection(context.Background(), "test_col")
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	if info.Status != "green" {
		t.Errorf("expected status green, got %s", info.Status)
	}
	if info.PointsCount != 100 {
		t.Errorf("expected 100 points, got %d", info.PointsCount)
	}
	if info.VectorSize != 768 {
		t.Errorf("expected vector size 768, got %d", info.VectorSize)
	}
}

func TestUpsert(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		if r.URL.Path != "/collections/test_col/points" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("wait") != "true" {
			t.Error("expected wait=true query param")
		}
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"status": "completed"}})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	err := c.Upsert(context.Background(), "test_col", []Point{
		{ID: "p1", Vector: []float32{0.1, 0.2, 0.3}, Payload: map[string]any{"text": "hello"}},
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	points, ok := received["points"].([]any)
	if !ok || len(points) != 1 {
		t.Fatalf("expected 1 point in request, got %v", received["points"])
	}
}

func TestUpsertEmpty(t *testing.T) {
	c := NewClient("http://unused:6333", "")
	err := c.Upsert(context.Background(), "test_col", nil)
	if err != nil {
		t.Fatalf("Upsert with nil points should not error: %v", err)
	}
}

func TestSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/test_col/points/search" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"result": []map[string]any{
				{"id": "p1", "score": 0.95, "payload": map[string]any{"text": "matched"}},
				{"id": "p2", "score": 0.80, "payload": map[string]any{"text": "also matched"}},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	results, err := c.Search(context.Background(), "test_col", []float32{0.1, 0.2}, 5, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].ID != "p1" {
		t.Errorf("expected first result ID p1, got %s", results[0].ID)
	}
	if results[0].Score < 0.94 {
		t.Errorf("unexpected score: %f", results[0].Score)
	}
	if results[0].Payload["text"] != "matched" {
		t.Errorf("unexpected payload: %v", results[0].Payload)
	}
}

func TestSearchWithFilter(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(map[string]any{"result": []any{}})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	filter := map[string]any{
		"must": []map[string]any{
			{"key": "type", "match": map[string]any{"value": "positive"}},
		},
	}
	results, err := c.Search(context.Background(), "test_col", []float32{0.1}, 3, filter)
	if err != nil {
		t.Fatalf("Search with filter: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
	if received["filter"] == nil {
		t.Fatal("expected filter in request body")
	}
}

func TestDelete(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/test_col/points/delete" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"status": "completed"}})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	err := c.Delete(context.Background(), "test_col", []string{"p1", "p2"})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	points, ok := received["points"].([]any)
	if !ok || len(points) != 2 {
		t.Fatalf("expected 2 point IDs, got %v", received["points"])
	}
}

func TestDeleteEmpty(t *testing.T) {
	c := NewClient("http://unused:6333", "")
	err := c.Delete(context.Background(), "test_col", nil)
	if err != nil {
		t.Fatalf("Delete with nil IDs should not error: %v", err)
	}
}

func TestCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/test_col/points/count" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"count": 42},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	count, err := c.Count(context.Background(), "test_col", nil)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 42 {
		t.Errorf("expected count 42, got %d", count)
	}
}

func TestScroll(t *testing.T) {
	nextOffset := "next123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"points": []map[string]any{
					{"id": "p1", "payload": map[string]any{"text": "first"}},
					{"id": "p2", "payload": map[string]any{"text": "second"}},
				},
				"next_page_offset": nextOffset,
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	points, next, err := c.Scroll(context.Background(), "test_col", 2, nil, nil)
	if err != nil {
		t.Fatalf("Scroll: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("expected 2 points, got %d", len(points))
	}
	if points[0].ID != "p1" {
		t.Errorf("expected first point ID p1, got %s", points[0].ID)
	}
	if next == nil || *next != "next123" {
		t.Errorf("expected next offset next123, got %v", next)
	}
}

func TestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"status":{"error":"something broke"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	_, err := c.Search(context.Background(), "test_col", []float32{0.1}, 1, nil)
	if err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}

func TestAPIKeyHeader(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("api-key")
		json.NewEncoder(w).Encode(map[string]any{"result": []any{}})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-secret-key")
	_, _ = c.Search(context.Background(), "col", []float32{0.1}, 1, nil)
	if gotKey != "test-secret-key" {
		t.Errorf("expected api-key header 'test-secret-key', got %q", gotKey)
	}
}

func TestNoAPIKeyWhenEmpty(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("api-key")
		json.NewEncoder(w).Encode(map[string]any{"result": []any{}})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	_, _ = c.Search(context.Background(), "col", []float32{0.1}, 1, nil)
	if gotKey != "" {
		t.Errorf("expected no api-key header, got %q", gotKey)
	}
}

func TestCollectionConstants(t *testing.T) {
	if CollectionNIPSpecs != "nip_specs" {
		t.Errorf("unexpected CollectionNIPSpecs: %s", CollectionNIPSpecs)
	}
	if CollectionProjectDocs != "project_docs" {
		t.Errorf("unexpected CollectionProjectDocs: %s", CollectionProjectDocs)
	}
	if CollectionFewShot != "few_shot_reviews" {
		t.Errorf("unexpected CollectionFewShot: %s", CollectionFewShot)
	}
}
