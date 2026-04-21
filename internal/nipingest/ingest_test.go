package nipingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"drydock/internal/embedding"
	"drydock/internal/vectorstore"
)

const sampleNIP = `# NIP-34

## Repository Announcements

Event kind 30617 is used for repository announcements.

Repositories are identified by their a tag value.

## Patch Events

Kind 1617 events represent patches submitted to a repository.

The patch content should be a valid git patch.

## Pull Requests

Kind 1618 represents a pull request. Kind 1619 is for PR updates.
`

func TestChunkMarkdown(t *testing.T) {
	chunks := ChunkMarkdown("34", sampleNIP)

	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}

	// Check first chunk.
	if chunks[0].NIPID != "34" {
		t.Errorf("expected NIP ID 34, got %s", chunks[0].NIPID)
	}
	if chunks[0].SectionTitle != "Repository Announcements" {
		t.Errorf("unexpected section title: %s", chunks[0].SectionTitle)
	}
	if len(chunks[0].EventKinds) == 0 {
		t.Error("expected event kinds to be extracted from first chunk")
	}
	found30617 := false
	for _, k := range chunks[0].EventKinds {
		if k == 30617 {
			found30617 = true
		}
	}
	if !found30617 {
		t.Errorf("expected kind 30617 in first chunk, got %v", chunks[0].EventKinds)
	}

	// Check content hash is set.
	if chunks[0].ContentHash == "" {
		t.Error("expected content hash to be set")
	}

	// Check second chunk.
	if chunks[1].SectionTitle != "Patch Events" {
		t.Errorf("unexpected second section: %s", chunks[1].SectionTitle)
	}

	// Check third chunk.
	if chunks[2].SectionTitle != "Pull Requests" {
		t.Errorf("unexpected third section: %s", chunks[2].SectionTitle)
	}
}

func TestChunkMarkdown_Empty(t *testing.T) {
	chunks := ChunkMarkdown("99", "")
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for empty content, got %d", len(chunks))
	}
}

func TestChunkMarkdown_NoHeadings(t *testing.T) {
	chunks := ChunkMarkdown("99", "# Top Heading\n\nSome intro text here.\nMore text.")
	if len(chunks) != 1 {
		t.Fatalf("expected 1 intro chunk, got %d", len(chunks))
	}
	if chunks[0].SectionTitle != "Introduction" {
		t.Errorf("expected Introduction section, got %s", chunks[0].SectionTitle)
	}
}

func TestChunkMarkdown_ContentHash_Deterministic(t *testing.T) {
	c1 := ChunkMarkdown("01", sampleNIP)
	c2 := ChunkMarkdown("01", sampleNIP)

	if len(c1) != len(c2) {
		t.Fatal("different chunk counts")
	}
	for i := range c1 {
		if c1[i].ContentHash != c2[i].ContentHash {
			t.Errorf("chunk %d hash mismatch: %s vs %s", i, c1[i].ContentHash, c2[i].ContentHash)
		}
	}
}

func TestExtractNIPID(t *testing.T) {
	tests := []struct {
		filename string
		want     string
	}{
		{"01.md", "01"},
		{"NIP-46.md", "46"},
		{"nip-34.md", "34"},
		{"5F.md", "5F"},
		{"NIP01.md", "01"},
	}
	for _, tt := range tests {
		got := extractNIPID(tt.filename)
		if got != tt.want {
			t.Errorf("extractNIPID(%q) = %q, want %q", tt.filename, got, tt.want)
		}
	}
}

func TestExtractEventKinds(t *testing.T) {
	text := "This uses kind 1617 for patches and kind 30617 for repo announcements. Also mentions kind: 1111."
	kinds := extractEventKinds(text)
	if len(kinds) == 0 {
		t.Fatal("expected event kinds to be extracted")
	}

	wantKinds := map[int]bool{1617: false, 30617: false, 1111: false}
	for _, k := range kinds {
		wantKinds[k] = true
	}
	for k, found := range wantKinds {
		if !found {
			t.Errorf("expected kind %d to be extracted", k)
		}
	}
}

func TestChunkID_Stable(t *testing.T) {
	c := Chunk{NIPID: "34", SectionTitle: "Patch Events"}
	id1 := chunkID(c)
	id2 := chunkID(c)
	if id1 != id2 {
		t.Errorf("chunkID not stable: %s vs %s", id1, id2)
	}
	if len(id1) != 32 {
		t.Errorf("expected 32 char ID, got %d", len(id1))
	}
}

func TestChunkID_Unique(t *testing.T) {
	c1 := Chunk{NIPID: "34", SectionTitle: "Patch Events"}
	c2 := Chunk{NIPID: "34", SectionTitle: "Repository Announcements"}
	if chunkID(c1) == chunkID(c2) {
		t.Error("different chunks should have different IDs")
	}
}

// TestRun_Integration tests the full pipeline with mock Qdrant and embedding servers.
func TestRun_Integration(t *testing.T) {
	// Create temp dir with NIP files.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "34.md"), []byte(sampleNIP), 0644); err != nil {
		t.Fatal(err)
	}

	// Mock embedding server.
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3}, "index": 0},
			},
		})
	}))
	defer embedSrv.Close()

	// Mock Qdrant server.
	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			// GetCollection — return 404 to trigger create.
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"status":{"error":"not found"}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/nip_specs":
			// CreateCollection.
			json.NewEncoder(w).Encode(map[string]any{"result": true})
		case r.Method == http.MethodPost && r.URL.Path == "/collections/nip_specs/points/scroll":
			// Scroll — empty (no existing points).
			json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{"points": []any{}, "next_page_offset": nil},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/collections/nip_specs/points":
			// Upsert.
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"status": "completed"}})
		default:
			t.Logf("unhandled request: %s %s", r.Method, r.URL.Path)
			json.NewEncoder(w).Encode(map[string]any{"result": true})
		}
	}))
	defer qdrantSrv.Close()

	embedClient := embedding.NewClient(embedSrv.URL, "", "test-model")
	qdrantClient := vectorstore.NewClient(qdrantSrv.URL, "")
	ingester := NewIngester(qdrantClient, embedClient, nil)

	n, err := ingester.Run(context.Background(), Config{
		NIPsDir:   dir,
		VectorDim: 3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 chunks upserted, got %d", n)
	}
}

// TestRun_EmptyDir tests graceful handling of empty directory.
func TestRun_EmptyDir(t *testing.T) {
	dir := t.TempDir()

	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("embed should not be called for empty dir")
	}))
	defer embedSrv.Close()

	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EnsureCollection check.
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{
					"status": "green", "points_count": 0,
					"config": map[string]any{"params": map[string]any{"vectors": map[string]any{"size": 3}}},
				},
			})
			return
		}
		if r.Method == http.MethodPost {
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"points": []any{}}})
			return
		}
	}))
	defer qdrantSrv.Close()

	embedClient := embedding.NewClient(embedSrv.URL, "", "test-model")
	qdrantClient := vectorstore.NewClient(qdrantSrv.URL, "")
	ingester := NewIngester(qdrantClient, embedClient, nil)

	n, err := ingester.Run(context.Background(), Config{NIPsDir: dir, VectorDim: 3})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 upserted, got %d", n)
	}
}
