package docsingest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/embedding"
	"drydock/internal/vectorstore"
)

func TestChunkDocument_Markdown(t *testing.T) {
	content := `# My Project

Some intro text here.

## Installation

Run go install.

## Usage

Use it like this.
`
	chunks := ChunkDocument("repo-1", "README.md", content)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}

	if chunks[0].SectionTitle != "Introduction" {
		t.Errorf("expected Introduction, got %q", chunks[0].SectionTitle)
	}
	if chunks[0].RepoID != "repo-1" {
		t.Errorf("expected repo-1, got %q", chunks[0].RepoID)
	}
	if chunks[0].FilePath != "README.md" {
		t.Errorf("expected README.md, got %q", chunks[0].FilePath)
	}
	if chunks[1].SectionTitle != "Installation" {
		t.Errorf("expected Installation, got %q", chunks[1].SectionTitle)
	}
	if chunks[2].SectionTitle != "Usage" {
		t.Errorf("expected Usage, got %q", chunks[2].SectionTitle)
	}
	if chunks[2].ContentHash == "" {
		t.Error("expected non-empty content hash")
	}
}

func TestChunkDocument_YAML(t *testing.T) {
	content := `openapi: "3.0.0"
info:
  title: My API
  version: "1.0"
paths: {}
`
	chunks := ChunkDocument("repo-1", "openapi.yaml", content)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].SectionTitle != "openapi.yaml" {
		t.Errorf("expected openapi.yaml, got %q", chunks[0].SectionTitle)
	}
	if chunks[0].RepoID != "repo-1" {
		t.Errorf("expected repo-1, got %q", chunks[0].RepoID)
	}
}

func TestChunkDocument_Empty(t *testing.T) {
	chunks := ChunkDocument("repo-1", "README.md", "")
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks, got %d", len(chunks))
	}
}

func TestChunkDocument_NoHeadings(t *testing.T) {
	chunks := ChunkDocument("repo-1", "README.md", "Just some text\nwith no headings.\n")
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].SectionTitle != "Introduction" {
		t.Errorf("expected Introduction, got %q", chunks[0].SectionTitle)
	}
}

func TestChunkDocument_ContentHashDeterministic(t *testing.T) {
	content := "## Section\nSome content here.\n"
	c1 := ChunkDocument("r", "f.md", content)
	c2 := ChunkDocument("r", "f.md", content)
	if len(c1) != 1 || len(c2) != 1 {
		t.Fatal("expected 1 chunk each")
	}
	if c1[0].ContentHash != c2[0].ContentHash {
		t.Error("expected deterministic content hashes")
	}
}

func TestChunkID_Stable(t *testing.T) {
	c := Chunk{RepoID: "repo-1", FilePath: "README.md", SectionTitle: "Usage"}
	id1 := chunkID(c)
	id2 := chunkID(c)
	if id1 != id2 {
		t.Errorf("chunk IDs not stable: %s vs %s", id1, id2)
	}
}

func TestChunkID_Unique(t *testing.T) {
	c1 := Chunk{RepoID: "repo-1", FilePath: "README.md", SectionTitle: "A"}
	c2 := Chunk{RepoID: "repo-1", FilePath: "README.md", SectionTitle: "B"}
	c3 := Chunk{RepoID: "repo-2", FilePath: "README.md", SectionTitle: "A"}
	if chunkID(c1) == chunkID(c2) {
		t.Error("different sections should have different IDs")
	}
	if chunkID(c1) == chunkID(c3) {
		t.Error("different repos should have different IDs")
	}
}

func TestDiscoverDocFiles(t *testing.T) {
	dir := t.TempDir()

	// Create files.
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "CONTRIBUTING.md"), []byte("# Contrib"), 0o644)
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, "docs", "guide.md"), []byte("# Guide"), 0o644)
	os.WriteFile(filepath.Join(dir, "openapi.yaml"), []byte("openapi: 3.0"), 0o644)
	// Binary file should not be picked up by well-known names,
	// but docs/ scan only checks extension.
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)

	ing := &Ingester{logger: nil}
	if ing.logger == nil {
		ing.logger = nopLogger()
	}
	files := ing.discoverDocFiles(dir, nil)

	if len(files) < 4 {
		t.Fatalf("expected at least 4 files, got %d: %v", len(files), files)
	}

	found := make(map[string]bool)
	for _, f := range files {
		found[f] = true
	}
	for _, want := range []string{"README.md", "CONTRIBUTING.md", "openapi.yaml"} {
		if !found[want] {
			t.Errorf("expected %s in discovered files", want)
		}
	}
	// docs/guide.md should be found via recursive scan
	if !found[filepath.Join("docs", "guide.md")] {
		t.Errorf("expected docs/guide.md in discovered files")
	}
	// main.go should not be found
	if found["main.go"] {
		t.Error("main.go should not be discovered as documentation")
	}
}

func TestDiscoverDocFiles_WorkspaceRoots(t *testing.T) {
	dir := t.TempDir()

	os.MkdirAll(filepath.Join(dir, "packages", "auth"), 0o755)
	os.WriteFile(filepath.Join(dir, "packages", "auth", "README.md"), []byte("# Auth"), 0o644)

	ing := &Ingester{logger: nopLogger()}
	files := ing.discoverDocFiles(dir, []string{filepath.Join("packages", "auth")})

	found := make(map[string]bool)
	for _, f := range files {
		found[f] = true
	}
	want := filepath.Join("packages", "auth", "README.md")
	if !found[want] {
		t.Errorf("expected %s in discovered files, got %v", want, files)
	}
}

func TestIsProbablyText(t *testing.T) {
	if !isProbablyText([]byte("hello world")) {
		t.Error("expected text")
	}
	if isProbablyText([]byte{0x00, 0x01, 0x02}) {
		t.Error("expected binary")
	}
}

func TestRun_Integration(t *testing.T) {
	// Set up a fake embedding + Qdrant server.
	var upsertedPoints int
	mux := http.NewServeMux()
	mux.HandleFunc("/collections/project_docs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{"status": "green", "points_count": 0,
					"config": map[string]any{"params": map[string]any{"vectors": map[string]any{"size": 4}}}},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"result": true})
	})
	mux.HandleFunc("/collections/project_docs/points/scroll", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"points": []any{}, "next_page_offset": nil},
		})
	})
	mux.HandleFunc("/collections/project_docs/points", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if pts, ok := body["points"].([]any); ok {
			upsertedPoints += len(pts)
		}
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"status": "completed"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.1, 0.2, 0.3, 0.4}, "index": 0}},
		})
	}))
	defer embedSrv.Close()

	// Create a fake repo with docs.
	repoDir := t.TempDir()
	os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Project\n\nIntro text.\n\n## Usage\n\nUse it.\n"), 0o644)
	os.WriteFile(filepath.Join(repoDir, "CONTRIBUTING.md"), []byte("# Contributing\n\n## Code Style\n\nUse gofmt.\n"), 0o644)

	qdrantClient := vectorstore.NewClient(srv.URL, "")
	embedClient := embedding.NewClient(embedSrv.URL, "", "test-model")
	ingester := NewIngester(qdrantClient, embedClient, nopLogger())

	n, err := ingester.Run(context.Background(), Config{
		RepoPath:  repoDir,
		RepoID:    "test-repo",
		VectorDim: 4,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	// README.md: Introduction + Usage = 2 chunks
	// CONTRIBUTING.md: Code Style = 1 chunk
	if n != 3 {
		t.Errorf("expected 3 upserted chunks, got %d", n)
	}
	if upsertedPoints != 3 {
		t.Errorf("expected 3 points sent to Qdrant, got %d", upsertedPoints)
	}
}

func TestRun_EmptyDir(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"status": "green", "points_count": 0,
				"config": map[string]any{"params": map[string]any{"vectors": map[string]any{"size": 4}}}},
		})
	}))
	defer srv.Close()

	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.1, 0.2, 0.3, 0.4}, "index": 0}},
		})
	}))
	defer embedSrv.Close()

	qdrantClient := vectorstore.NewClient(srv.URL, "")
	embedClient := embedding.NewClient(embedSrv.URL, "", "test-model")
	ingester := NewIngester(qdrantClient, embedClient, nopLogger())

	n, err := ingester.Run(context.Background(), Config{
		RepoPath:  t.TempDir(),
		RepoID:    "empty-repo",
		VectorDim: 4,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 upserted, got %d", n)
	}
}

func TestRun_MissingConfig(t *testing.T) {
	ing := NewIngester(nil, nil, nopLogger())

	_, err := ing.Run(context.Background(), Config{})
	if err == nil || !strings.Contains(err.Error(), "RepoPath") {
		t.Errorf("expected RepoPath error, got %v", err)
	}

	_, err = ing.Run(context.Background(), Config{RepoPath: "/tmp"})
	if err == nil || !strings.Contains(err.Error(), "RepoID") {
		t.Errorf("expected RepoID error, got %v", err)
	}
}

func TestRun_Dedup(t *testing.T) {
	// Simulate existing hashes in Qdrant that match current content.
	content := "## Section\n\nSome content.\n"
	chunks := ChunkDocument("repo-1", "README.md", content)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	existingID := chunkID(chunks[0])
	existingHash := chunks[0].ContentHash

	var upsertCalled bool
	mux := http.NewServeMux()
	mux.HandleFunc("/collections/project_docs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"status": "green", "points_count": 1,
				"config": map[string]any{"params": map[string]any{"vectors": map[string]any{"size": 4}}}},
		})
	})
	mux.HandleFunc("/collections/project_docs/points/scroll", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"points": []map[string]any{
					{"id": existingID, "payload": map[string]any{"content_hash": existingHash, "repo_id": "repo-1"}},
				},
				"next_page_offset": nil,
			},
		})
	})
	mux.HandleFunc("/collections/project_docs/points", func(w http.ResponseWriter, r *http.Request) {
		upsertCalled = true
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"status": "completed"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.1, 0.2, 0.3, 0.4}, "index": 0}},
		})
	}))
	defer embedSrv.Close()

	repoDir := t.TempDir()
	os.WriteFile(filepath.Join(repoDir, "README.md"), []byte(content), 0o644)

	qdrantClient := vectorstore.NewClient(srv.URL, "")
	embedClient := embedding.NewClient(embedSrv.URL, "", "test-model")
	ingester := NewIngester(qdrantClient, embedClient, nopLogger())

	n, err := ingester.Run(context.Background(), Config{
		RepoPath:  repoDir,
		RepoID:    "repo-1",
		VectorDim: 4,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 upserted (dedup), got %d", n)
	}
	if upsertCalled {
		t.Error("upsert should not have been called for unchanged content")
	}
}

func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
