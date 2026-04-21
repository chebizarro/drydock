package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"drydock/internal/lspbridge"
)

func TestHealthEndpoint(t *testing.T) {
	mgr := NewManager(nil)
	defer mgr.Shutdown()

	h := NewHandler(mgr, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp lspbridge.HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status ok, got %s", resp.Status)
	}
}

func TestAnalyzeEndpoint_MissingRepoPath(t *testing.T) {
	mgr := NewManager(nil)
	defer mgr.Shutdown()

	h := NewHandler(mgr, nil)
	body := `{"changed_files":["main.go"]}`
	req := httptest.NewRequest(http.MethodPost, "/analyze", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestAnalyzeEndpoint_InvalidJSON(t *testing.T) {
	mgr := NewManager(nil)
	defer mgr.Shutdown()

	h := NewHandler(mgr, nil)
	req := httptest.NewRequest(http.MethodPost, "/analyze", bytes.NewBufferString("{invalid"))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestAnalyzeEndpoint_NoSupportedLanguages(t *testing.T) {
	mgr := NewManager(nil)
	defer mgr.Shutdown()

	h := NewHandler(mgr, nil)
	body := `{"repo_path":"/tmp/test","changed_files":["README.md","notes.txt"]}`
	req := httptest.NewRequest(http.MethodPost, "/analyze", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp lspbridge.AnalyzeResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == "" {
		t.Error("expected error about no supported languages")
	}
}

func TestLSPHelpers(t *testing.T) {
	// fileURI
	if got := fileURI("/tmp/test.go"); got != "file:///tmp/test.go" {
		t.Errorf("fileURI: %s", got)
	}

	// uriToPath
	if got := uriToPath("file:///tmp/test.go"); got != "/tmp/test.go" {
		t.Errorf("uriToPath: %s", got)
	}

	// relPath
	if got := relPath("/repos/myproject/src/main.go", "/repos/myproject"); got != "src/main.go" {
		t.Errorf("relPath: %s", got)
	}
	if got := relPath("/other/path.go", "/repos/myproject"); got != "/other/path.go" {
		t.Errorf("relPath non-relative: %s", got)
	}

	// lspSymbolKindName
	if got := lspSymbolKindName(12); got != "function" {
		t.Errorf("symbolKind 12: %s", got)
	}
	if got := lspSymbolKindName(999); got != "kind-999" {
		t.Errorf("symbolKind 999: %s", got)
	}
}
