package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbedSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/embeddings" {
			t.Errorf("expected /embeddings, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %s", r.Header.Get("Authorization"))
		}

		var req embeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "nomic-embed-text" {
			t.Errorf("expected model nomic-embed-text, got %s", req.Model)
		}
		if req.Input == "" {
			t.Errorf("expected non-empty input")
		}

		resp := embeddingResponse{
			Data: []embeddingData{
				{Embedding: []float32{0.1, 0.2, 0.3, 0.4}, Index: 0},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key", "nomic-embed-text")
	vec, err := client.Embed(context.Background(), "func main() { fmt.Println(\"hello\") }")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 4 {
		t.Fatalf("expected 4-dim vector, got %d", len(vec))
	}
	if vec[0] != 0.1 || vec[2] != 0.3 {
		t.Errorf("unexpected vector: %v", vec)
	}
}

func TestEmbedEmptyInput(t *testing.T) {
	client := NewClient("http://localhost:9999", "", "model")
	_, err := client.Embed(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestEmbedHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "model not loaded"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", "model")
	_, err := client.Embed(context.Background(), "test text")
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestEmbedEmptyDataResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(embeddingResponse{Data: nil})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", "model")
	_, err := client.Embed(context.Background(), "test text")
	if err == nil {
		t.Fatal("expected error for empty data response")
	}
}

func TestEmbedNoAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("expected no auth header, got %s", r.Header.Get("Authorization"))
		}
		resp := embeddingResponse{
			Data: []embeddingData{{Embedding: []float32{1.0}, Index: 0}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", "model")
	vec, err := client.Embed(context.Background(), "test")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 1 {
		t.Fatalf("expected 1-dim vector, got %d", len(vec))
	}
}

func TestModelAccessor(t *testing.T) {
	client := NewClient("http://localhost", "key", "nomic-embed-text")
	if client.Model() != "nomic-embed-text" {
		t.Errorf("Model() = %q, want %q", client.Model(), "nomic-embed-text")
	}
}
