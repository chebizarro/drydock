package codeindex

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/contextbuilder"
	"drydock/internal/embedding"
	"drydock/internal/symbols"
	"drydock/internal/vectorstore"
)

// --- Test helpers ---

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// initTestRepo creates a bare-minimum git repo with an initial commit
// containing the given files. Returns the repo path.
func initTestRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "test@test.com")
	gitRun(t, dir, "config", "user.name", "Test")

	for path, content := range files {
		absPath := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", path)
	}

	gitRun(t, dir, "commit", "-m", "initial")
	return dir
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00+00:00",
		"GIT_COMMITTER_DATE=2020-01-01T00:00:00+00:00",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func getCommit(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// fakeEmbedServer returns an httptest server that returns dummy embeddings.
func fakeEmbedServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Input string `json:"input"`
		}
		json.Unmarshal(body, &req)

		// Return a deterministic dummy vector.
		vec := make([]float64, 768)
		for i := range vec {
			vec[i] = float64(i) * 0.001
		}
		resp := map[string]any{
			"data": []map[string]any{
				{"embedding": vec, "index": 0},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

// fakeQdrantServer returns an httptest server that simulates Qdrant.
// It tracks upserted points and supports search/count/scroll/delete.
type fakeQdrant struct {
	server *httptest.Server
	points map[string][]vectorstore.Point // collection → points
}

func newFakeQdrant() *fakeQdrant {
	fq := &fakeQdrant{
		points: make(map[string][]vectorstore.Point),
	}
	fq.server = httptest.NewServer(http.HandlerFunc(fq.handle))
	return fq
}

func (fq *fakeQdrant) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// GET /collections/<name> → check existence
	if r.Method == http.MethodGet && strings.HasPrefix(path, "/collections/") {
		col := strings.TrimPrefix(path, "/collections/")
		col = strings.TrimSuffix(col, "/")
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"status":       "green",
				"points_count": len(fq.points[col]),
				"config": map[string]any{
					"params": map[string]any{
						"vectors": map[string]any{"size": 768},
					},
				},
			},
		})
		return
	}

	// PUT /collections/<name> → create collection
	if r.Method == http.MethodPut && strings.HasPrefix(path, "/collections/") && !strings.Contains(path, "/points") {
		col := strings.TrimPrefix(path, "/collections/")
		col = strings.TrimSuffix(col, "/")
		if fq.points[col] == nil {
			fq.points[col] = []vectorstore.Point{}
		}
		json.NewEncoder(w).Encode(map[string]any{"result": true})
		return
	}

	// PUT /collections/<name>/points → upsert
	if r.Method == http.MethodPut && strings.Contains(path, "/points") {
		col := extractCollection(path)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Points []vectorstore.Point `json:"points"`
		}
		json.Unmarshal(body, &req)
		fq.points[col] = append(fq.points[col], req.Points...)
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"status": "completed"}})
		return
	}

	// POST /collections/<name>/points/count → count
	if r.Method == http.MethodPost && strings.HasSuffix(path, "/points/count") {
		col := extractCollection(path)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Filter map[string]any `json:"filter"`
		}
		json.Unmarshal(body, &req)

		count := 0
		repoFilter := extractRepoFilter(req.Filter)
		for _, p := range fq.points[col] {
			if repoFilter == "" || p.Payload["repo_id"] == repoFilter {
				count++
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"count": count},
		})
		return
	}

	// POST /collections/<name>/points/scroll → scroll
	if r.Method == http.MethodPost && strings.HasSuffix(path, "/points/scroll") {
		col := extractCollection(path)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Filter map[string]any `json:"filter"`
		}
		json.Unmarshal(body, &req)

		repoFilter := extractRepoFilter(req.Filter)
		fileFilter := extractFileFilter(req.Filter)

		var matched []map[string]any
		for _, p := range fq.points[col] {
			if repoFilter != "" && p.Payload["repo_id"] != repoFilter {
				continue
			}
			if fileFilter != "" && p.Payload["file_path"] != fileFilter {
				continue
			}
			matched = append(matched, map[string]any{
				"id":      p.ID,
				"payload": p.Payload,
			})
		}
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"points":           matched,
				"next_page_offset": nil,
			},
		})
		return
	}

	// POST /collections/<name>/points/delete → delete
	if r.Method == http.MethodPost && strings.HasSuffix(path, "/points/delete") {
		col := extractCollection(path)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Points []string `json:"points"`
		}
		json.Unmarshal(body, &req)

		deleteSet := make(map[string]bool, len(req.Points))
		for _, id := range req.Points {
			deleteSet[id] = true
		}
		var kept []vectorstore.Point
		for _, p := range fq.points[col] {
			if !deleteSet[p.ID] {
				kept = append(kept, p)
			}
		}
		fq.points[col] = kept
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"status": "completed"}})
		return
	}

	// POST /collections/<name>/points/search → search
	if r.Method == http.MethodPost && strings.HasSuffix(path, "/points/search") {
		col := extractCollection(path)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Filter map[string]any `json:"filter"`
		}
		json.Unmarshal(body, &req)

		repoFilter := extractRepoFilter(req.Filter)
		var results []map[string]any
		for _, p := range fq.points[col] {
			if repoFilter != "" && p.Payload["repo_id"] != repoFilter {
				continue
			}
			results = append(results, map[string]any{
				"id":      p.ID,
				"score":   0.85,
				"payload": p.Payload,
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"result": results})
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func extractCollection(path string) string {
	// /collections/code_chunks/points/... → code_chunks
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "collections" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func extractRepoFilter(filter map[string]any) string {
	return extractFilterValue(filter, "repo_id")
}

func extractFileFilter(filter map[string]any) string {
	return extractFilterValue(filter, "file_path")
}

func extractFilterValue(filter map[string]any, key string) string {
	if filter == nil {
		return ""
	}
	must, ok := filter["must"].([]any)
	if !ok {
		return ""
	}
	for _, item := range must {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["key"] == key {
			if match, ok := m["match"].(map[string]any); ok {
				if v, ok := match["value"].(string); ok {
					return v
				}
			}
		}
	}
	return ""
}

// --- Indexer tests ---

func TestIndexRepoCreatesChunks(t *testing.T) {
	goSource := `package main

func Hello() string {
	return "hello"
}

func Goodbye() string {
	return "goodbye"
}
`
	repoPath := initTestRepo(t, map[string]string{
		"main.go": goSource,
	})

	fq := newFakeQdrant()
	defer fq.server.Close()
	embedSrv := fakeEmbedServer()
	defer embedSrv.Close()

	qdrant := vectorstore.NewClient(fq.server.URL, "")
	embed := embedding.NewClient(embedSrv.URL, "", "test-model")
	indexer := New(qdrant, embed, testLogger())

	ctx := context.Background()
	err := indexer.IndexRepo(ctx, repoPath, "test-repo")
	if err != nil {
		t.Fatal(err)
	}

	points := fq.points[vectorstore.CollectionCodeChunks]
	if len(points) == 0 {
		t.Fatal("expected code chunks to be created")
	}

	// Should have chunks for Hello and Goodbye.
	foundHello, foundGoodbye := false, false
	for _, p := range points {
		name, _ := p.Payload["symbol_name"].(string)
		if name == "Hello" {
			foundHello = true
			// Verify payload fields.
			if p.Payload["repo_id"] != "test-repo" {
				t.Errorf("wrong repo_id: %v", p.Payload["repo_id"])
			}
			if p.Payload["file_path"] != "main.go" {
				t.Errorf("wrong file_path: %v", p.Payload["file_path"])
			}
			if p.Payload["language"] != "go" {
				t.Errorf("wrong language: %v", p.Payload["language"])
			}
			if p.Payload["symbol_kind"] != "function" {
				t.Errorf("wrong symbol_kind: %v", p.Payload["symbol_kind"])
			}
		}
		if name == "Goodbye" {
			foundGoodbye = true
		}
	}
	if !foundHello || !foundGoodbye {
		t.Errorf("expected Hello and Goodbye, found Hello=%v Goodbye=%v", foundHello, foundGoodbye)
	}

	// State file should be written.
	state := readState(repoPath)
	if state.Commit == "" {
		t.Error("expected state file to be written with commit hash")
	}
}

func TestIndexRepoIncrementalSkipsUnchanged(t *testing.T) {
	repoPath := initTestRepo(t, map[string]string{
		"main.go": "package main\n\nfunc Hello() {}\n",
	})

	fq := newFakeQdrant()
	defer fq.server.Close()
	embedSrv := fakeEmbedServer()
	defer embedSrv.Close()

	qdrant := vectorstore.NewClient(fq.server.URL, "")
	embed := embedding.NewClient(embedSrv.URL, "", "test-model")
	indexer := New(qdrant, embed, testLogger())

	ctx := context.Background()

	// First index.
	if err := indexer.IndexRepo(ctx, repoPath, "test-repo"); err != nil {
		t.Fatal(err)
	}
	firstCount := len(fq.points[vectorstore.CollectionCodeChunks])

	// Second index without changes — should be a no-op.
	if err := indexer.IndexRepo(ctx, repoPath, "test-repo"); err != nil {
		t.Fatal(err)
	}
	secondCount := len(fq.points[vectorstore.CollectionCodeChunks])

	if secondCount != firstCount {
		t.Errorf("incremental re-index created new points: %d → %d", firstCount, secondCount)
	}
}

func TestIndexRepoIncrementalIndexesNewFiles(t *testing.T) {
	repoPath := initTestRepo(t, map[string]string{
		"main.go": "package main\n\nfunc Hello() {}\n",
	})

	fq := newFakeQdrant()
	defer fq.server.Close()
	embedSrv := fakeEmbedServer()
	defer embedSrv.Close()

	qdrant := vectorstore.NewClient(fq.server.URL, "")
	embed := embedding.NewClient(embedSrv.URL, "", "test-model")
	indexer := New(qdrant, embed, testLogger())

	ctx := context.Background()

	// First index.
	if err := indexer.IndexRepo(ctx, repoPath, "test-repo"); err != nil {
		t.Fatal(err)
	}
	firstCount := len(fq.points[vectorstore.CollectionCodeChunks])

	// Add a new file and commit.
	newFile := filepath.Join(repoPath, "utils.go")
	os.WriteFile(newFile, []byte("package main\n\nfunc Util() {}\n"), 0o644)
	gitRun(t, repoPath, "add", "utils.go")
	gitRun(t, repoPath, "commit", "-m", "add utils")

	// Re-index should pick up the new file.
	if err := indexer.IndexRepo(ctx, repoPath, "test-repo"); err != nil {
		t.Fatal(err)
	}
	secondCount := len(fq.points[vectorstore.CollectionCodeChunks])

	if secondCount <= firstCount {
		t.Errorf("expected more points after adding file: %d → %d", firstCount, secondCount)
	}
}

func TestIndexRepoDeletedFileCleanup(t *testing.T) {
	repoPath := initTestRepo(t, map[string]string{
		"main.go":  "package main\n\nfunc Hello() {}\n",
		"utils.go": "package main\n\nfunc Util() {}\n",
	})

	fq := newFakeQdrant()
	defer fq.server.Close()
	embedSrv := fakeEmbedServer()
	defer embedSrv.Close()

	qdrant := vectorstore.NewClient(fq.server.URL, "")
	embed := embedding.NewClient(embedSrv.URL, "", "test-model")
	indexer := New(qdrant, embed, testLogger())

	ctx := context.Background()

	// First index.
	if err := indexer.IndexRepo(ctx, repoPath, "test-repo"); err != nil {
		t.Fatal(err)
	}
	initialCount := len(fq.points[vectorstore.CollectionCodeChunks])

	// Delete utils.go and commit.
	os.Remove(filepath.Join(repoPath, "utils.go"))
	gitRun(t, repoPath, "add", "-A")
	gitRun(t, repoPath, "commit", "-m", "delete utils")

	// Re-index should remove utils.go chunks.
	if err := indexer.IndexRepo(ctx, repoPath, "test-repo"); err != nil {
		t.Fatal(err)
	}

	// Verify utils.go chunks are gone.
	for _, p := range fq.points[vectorstore.CollectionCodeChunks] {
		if p.Payload["file_path"] == "utils.go" {
			t.Error("expected utils.go chunks to be deleted")
		}
	}

	finalCount := len(fq.points[vectorstore.CollectionCodeChunks])
	if finalCount >= initialCount {
		t.Errorf("expected fewer points after deletion: %d → %d", initialCount, finalCount)
	}
}

func TestIndexRepoSkipsUnsupportedLanguages(t *testing.T) {
	repoPath := initTestRepo(t, map[string]string{
		"data.csv":  "a,b,c\n1,2,3\n",
		"config.yml": "key: value\n",
		"main.go":   "package main\n\nfunc Hello() {}\n",
	})

	fq := newFakeQdrant()
	defer fq.server.Close()
	embedSrv := fakeEmbedServer()
	defer embedSrv.Close()

	qdrant := vectorstore.NewClient(fq.server.URL, "")
	embed := embedding.NewClient(embedSrv.URL, "", "test-model")
	indexer := New(qdrant, embed, testLogger())

	ctx := context.Background()
	if err := indexer.IndexRepo(ctx, repoPath, "test-repo"); err != nil {
		t.Fatal(err)
	}

	// Should only have chunks from main.go, not csv/yml.
	for _, p := range fq.points[vectorstore.CollectionCodeChunks] {
		fp, _ := p.Payload["file_path"].(string)
		if fp != "main.go" {
			t.Errorf("unexpected file indexed: %s", fp)
		}
	}
}

func TestIndexRepoSkipsLargeFiles(t *testing.T) {
	// Create a Go file that exceeds maxFileBytes.
	bigContent := "package main\n\nfunc Big() {\n"
	for len(bigContent) < maxFileBytes+1000 {
		bigContent += "\t// padding line\n"
	}
	bigContent += "}\n"

	repoPath := initTestRepo(t, map[string]string{
		"big.go":   bigContent,
		"small.go": "package main\n\nfunc Small() {}\n",
	})

	fq := newFakeQdrant()
	defer fq.server.Close()
	embedSrv := fakeEmbedServer()
	defer embedSrv.Close()

	qdrant := vectorstore.NewClient(fq.server.URL, "")
	embed := embedding.NewClient(embedSrv.URL, "", "test-model")
	indexer := New(qdrant, embed, testLogger())

	ctx := context.Background()
	if err := indexer.IndexRepo(ctx, repoPath, "test-repo"); err != nil {
		t.Fatal(err)
	}

	// Should only have chunks from small.go.
	for _, p := range fq.points[vectorstore.CollectionCodeChunks] {
		fp, _ := p.Payload["file_path"].(string)
		if fp == "big.go" {
			t.Error("large file should have been skipped")
		}
	}
}

func TestIndexRepoForcesRebuildOnEmptyCollection(t *testing.T) {
	repoPath := initTestRepo(t, map[string]string{
		"main.go": "package main\n\nfunc Hello() {}\n",
	})

	fq := newFakeQdrant()
	defer fq.server.Close()
	embedSrv := fakeEmbedServer()
	defer embedSrv.Close()

	qdrant := vectorstore.NewClient(fq.server.URL, "")
	embed := embedding.NewClient(embedSrv.URL, "", "test-model")
	indexer := New(qdrant, embed, testLogger())

	ctx := context.Background()

	// First index.
	if err := indexer.IndexRepo(ctx, repoPath, "test-repo"); err != nil {
		t.Fatal(err)
	}
	if len(fq.points[vectorstore.CollectionCodeChunks]) == 0 {
		t.Fatal("expected points after first index")
	}

	// Simulate Qdrant data loss — clear points but keep state file.
	fq.points[vectorstore.CollectionCodeChunks] = nil

	// Re-index should detect divergence and rebuild.
	if err := indexer.IndexRepo(ctx, repoPath, "test-repo"); err != nil {
		t.Fatal(err)
	}

	if len(fq.points[vectorstore.CollectionCodeChunks]) == 0 {
		t.Error("expected points after forced rebuild")
	}
}

// --- State file tests ---

func TestReadWriteState(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	os.MkdirAll(gitDir, 0o755)

	s := indexState{Commit: "abc123", UpdatedAt: 1000}
	if err := writeState(dir, s); err != nil {
		t.Fatal(err)
	}

	got := readState(dir)
	if got.Commit != "abc123" || got.UpdatedAt != 1000 {
		t.Errorf("state mismatch: %+v", got)
	}
}

func TestReadStateMissingFile(t *testing.T) {
	dir := t.TempDir()
	got := readState(dir)
	if got.Commit != "" {
		t.Errorf("expected empty state for missing file, got: %+v", got)
	}
}

// --- Git helper tests ---

func TestResolveCanonicalCommit(t *testing.T) {
	repoPath := initTestRepo(t, map[string]string{
		"main.go": "package main\n",
	})

	ctx := context.Background()
	commit, err := resolveCanonicalCommit(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(commit) < 40 {
		t.Errorf("expected full commit hash, got: %s", commit)
	}
}

func TestGitDiffNameStatus(t *testing.T) {
	repoPath := initTestRepo(t, map[string]string{
		"a.go": "package main\n\nfunc A() {}\n",
	})

	commit1 := getCommit(t, repoPath)

	// Add a new file, modify existing, commit.
	os.WriteFile(filepath.Join(repoPath, "b.go"), []byte("package main\n\nfunc B() {}\n"), 0o644)
	os.WriteFile(filepath.Join(repoPath, "a.go"), []byte("package main\n\nfunc A() { return }\n"), 0o644)
	gitRun(t, repoPath, "add", "-A")
	gitRun(t, repoPath, "commit", "-m", "changes")

	commit2 := getCommit(t, repoPath)

	ctx := context.Background()
	changes, err := gitDiffNameStatus(ctx, repoPath, commit1, commit2)
	if err != nil {
		t.Fatal(err)
	}

	if len(changes) < 2 {
		t.Fatalf("expected at least 2 changes, got %d", len(changes))
	}

	hasA, hasB := false, false
	for _, c := range changes {
		if c.effectivePath() == "a.go" && c.status == "M" {
			hasA = true
		}
		if c.effectivePath() == "b.go" && c.status == "A" {
			hasB = true
		}
	}
	if !hasA || !hasB {
		t.Errorf("expected M for a.go and A for b.go, changes: %+v", changes)
	}
}

func TestExtractChunk(t *testing.T) {
	lines := []string{
		"package main",
		"",
		"import \"fmt\"",
		"",
		"func Hello() {",
		"\tfmt.Println(\"hello\")",
		"}",
		"",
		"func Bye() {}",
	}

	// Symbol at lines 4-6 (0-based).
	sym := symbols.Symbol{Name: "Hello", Kind: symbols.KindFunction, StartLine: 4, EndLine: 6}

	chunk := extractChunk(lines, sym)
	if chunk == nil {
		t.Fatal("expected non-nil chunk")
	}

	content := string(chunk)
	if !strings.Contains(content, "func Hello()") {
		t.Errorf("chunk should contain the function, got: %s", content)
	}
	// Should include context (import line is 2 lines before).
	if !strings.Contains(content, "import") {
		t.Errorf("chunk should include context lines, got: %s", content)
	}
}

func TestChunkPointID(t *testing.T) {
	sym1 := symbols.Symbol{Name: "Hello", Kind: symbols.KindFunction, StartLine: 4, EndLine: 6}
	sym2 := symbols.Symbol{Name: "Hello", Kind: symbols.KindFunction, StartLine: 4, EndLine: 6}
	sym3 := symbols.Symbol{Name: "Bye", Kind: symbols.KindFunction, StartLine: 8, EndLine: 8}

	id1 := chunkPointID("repo1", "main.go", sym1)
	id2 := chunkPointID("repo1", "main.go", sym2)
	id3 := chunkPointID("repo1", "main.go", sym3)

	if id1 != id2 {
		t.Error("same inputs should produce same ID")
	}
	if id1 == id3 {
		t.Error("different symbols should produce different IDs")
	}
}

// --- Provider tests ---

func TestProviderLayerMeta(t *testing.T) {
	fq := newFakeQdrant()
	defer fq.server.Close()
	embedSrv := fakeEmbedServer()
	defer embedSrv.Close()

	qdrant := vectorstore.NewClient(fq.server.URL, "")
	embed := embedding.NewClient(embedSrv.URL, "", "test-model")
	p := NewProvider(qdrant, embed, testLogger())

	if p.LayerName() != "symbols-related-code" {
		t.Errorf("wrong layer name: %s", p.LayerName())
	}
	if p.Priority() != 3 {
		t.Errorf("wrong priority: %d", p.Priority())
	}
}

func TestProviderNilClients(t *testing.T) {
	p := NewProvider(nil, nil, testLogger())
	if p != nil {
		t.Error("expected nil provider for nil clients")
	}
}

func TestProviderEmptyPatch(t *testing.T) {
	fq := newFakeQdrant()
	defer fq.server.Close()
	embedSrv := fakeEmbedServer()
	defer embedSrv.Close()

	qdrant := vectorstore.NewClient(fq.server.URL, "")
	embed := embedding.NewClient(embedSrv.URL, "", "test-model")
	p := NewProvider(qdrant, embed, testLogger())

	ctx := context.Background()
	content, err := p.Build(ctx, contextbuilder.BuildInput{
		RepoID:            "test-repo",
		PatchEventContent: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if content != "" {
		t.Errorf("expected empty content for empty patch, got: %s", content)
	}
}

func TestProviderExcludesChangedFiles(t *testing.T) {
	fq := newFakeQdrant()
	defer fq.server.Close()
	embedSrv := fakeEmbedServer()
	defer embedSrv.Close()

	// Seed Qdrant with chunks from two files.
	fq.points[vectorstore.CollectionCodeChunks] = []vectorstore.Point{
		{
			ID:     "point-1",
			Vector: make([]float32, 768),
			Payload: map[string]any{
				"repo_id":     "test-repo",
				"file_path":   "changed.go", // this file is in the patch
				"symbol_name": "ChangedFunc",
				"symbol_kind": "function",
				"start_line":  float64(1),
				"end_line":    float64(5),
				"content":     "func ChangedFunc() {}",
			},
		},
		{
			ID:     "point-2",
			Vector: make([]float32, 768),
			Payload: map[string]any{
				"repo_id":     "test-repo",
				"file_path":   "related.go", // this is NOT in the patch
				"symbol_name": "RelatedFunc",
				"symbol_kind": "function",
				"start_line":  float64(1),
				"end_line":    float64(5),
				"content":     "func RelatedFunc() {}",
			},
		},
	}

	qdrant := vectorstore.NewClient(fq.server.URL, "")
	embed := embedding.NewClient(embedSrv.URL, "", "test-model")
	p := NewProvider(qdrant, embed, testLogger())

	ctx := context.Background()
	patch := "diff --git a/changed.go b/changed.go\n--- a/changed.go\n+++ b/changed.go\n@@ -1,3 +1,4 @@\n func ChangedFunc() {\n+\treturn\n }\n"

	content, err := p.Build(ctx, contextbuilder.BuildInput{
		RepoID:            "test-repo",
		PatchEventContent: patch,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should include RelatedFunc but NOT ChangedFunc.
	if strings.Contains(content, "ChangedFunc") {
		t.Error("changed file should be excluded from related-code results")
	}
	if !strings.Contains(content, "RelatedFunc") {
		t.Error("related code from unchanged file should be included")
	}
}

func TestProviderRespectsWorkspaceRoots(t *testing.T) {
	fq := newFakeQdrant()
	defer fq.server.Close()
	embedSrv := fakeEmbedServer()
	defer embedSrv.Close()

	// Seed chunks from two different workspace paths.
	fq.points[vectorstore.CollectionCodeChunks] = []vectorstore.Point{
		{
			ID:     "point-in-workspace",
			Vector: make([]float32, 768),
			Payload: map[string]any{
				"repo_id":     "test-repo",
				"file_path":   "packages/core/helper.go",
				"symbol_name": "InWorkspace",
				"symbol_kind": "function",
				"start_line":  float64(1),
				"end_line":    float64(3),
				"content":     "func InWorkspace() {}",
			},
		},
		{
			ID:     "point-outside-workspace",
			Vector: make([]float32, 768),
			Payload: map[string]any{
				"repo_id":     "test-repo",
				"file_path":   "packages/other/unrelated.go",
				"symbol_name": "OutsideWorkspace",
				"symbol_kind": "function",
				"start_line":  float64(1),
				"end_line":    float64(3),
				"content":     "func OutsideWorkspace() {}",
			},
		},
	}

	qdrant := vectorstore.NewClient(fq.server.URL, "")
	embed := embedding.NewClient(embedSrv.URL, "", "test-model")
	p := NewProvider(qdrant, embed, testLogger())

	ctx := context.Background()
	patch := "diff --git a/packages/core/main.go b/packages/core/main.go\n--- a/packages/core/main.go\n+++ b/packages/core/main.go\n@@ -1 +1 @@\n-old\n+new\n"

	content, err := p.Build(ctx, contextbuilder.BuildInput{
		RepoID:            "test-repo",
		PatchEventContent: patch,
		WorkspaceRoots:    []string{"packages/core"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(content, "OutsideWorkspace") {
		t.Error("code outside workspace roots should be filtered out")
	}
	if !strings.Contains(content, "InWorkspace") {
		t.Error("code inside workspace roots should be included")
	}
}

func TestProviderNoRepoID(t *testing.T) {
	fq := newFakeQdrant()
	defer fq.server.Close()
	embedSrv := fakeEmbedServer()
	defer embedSrv.Close()

	qdrant := vectorstore.NewClient(fq.server.URL, "")
	embed := embedding.NewClient(embedSrv.URL, "", "test-model")
	p := NewProvider(qdrant, embed, testLogger())

	ctx := context.Background()
	content, err := p.Build(ctx, contextbuilder.BuildInput{
		PatchEventContent: "diff --git a/a.go b/a.go\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if content != "" {
		t.Error("expected empty content when RepoID is missing")
	}
}

func TestPayloadInt(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want int
	}{
		{"float64", float64(42), 42},
		{"int", int(10), 10},
		{"int64", int64(99), 99},
		{"string", "nope", 0},
		{"nil", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := payloadInt(map[string]any{"k": tt.val}, "k")
			if got != tt.want {
				t.Errorf("payloadInt = %d, want %d", got, tt.want)
			}
		})
	}
}


