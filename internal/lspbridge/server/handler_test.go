package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/lspbridge"
)

type fakeManager struct {
	conn *lspConn
	err  error
}

func (f fakeManager) GetOrStart(_ context.Context, _, _ string) (*lspConn, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.conn, nil
}

func (f fakeManager) ProcessStatus() map[string]string { return map[string]string{"go@test": "fake"} }

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
	if len(resp.AllowedRoots) == 0 {
		t.Error("expected allowed roots to be reported")
	}
}

func TestAnalyzeEndpoint_MissingRepoPath(t *testing.T) {
	h := NewHandlerWithOptions(fakeManager{}, nil, HandlerOptions{})
	body := `{"changed_files":["main.go"]}`
	req := httptest.NewRequest(http.MethodPost, "/analyze", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestAnalyzeEndpoint_InvalidJSON(t *testing.T) {
	h := NewHandlerWithOptions(fakeManager{}, nil, HandlerOptions{})
	req := httptest.NewRequest(http.MethodPost, "/analyze", bytes.NewBufferString("{invalid"))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestAnalyzeEndpoint_NoSupportedLanguages(t *testing.T) {
	repo := t.TempDir()
	h := NewHandlerWithOptions(fakeManager{}, nil, HandlerOptions{AllowedRepoRoots: []string{repo}})
	body := fmt.Sprintf(`{"repo_path":%q,"changed_files":["README.md","notes.txt"]}`, repo)
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

func TestAnalyzeEndpoint_LanguageServerFailureReturnsStructuredBadGateway(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "main.go"), "package main\n")
	h := NewHandlerWithOptions(fakeManager{err: errors.New("gopls missing")}, nil, HandlerOptions{AllowedRepoRoots: []string{repo}})
	body := fmt.Sprintf(`{"repo_path":%q,"changed_files":["main.go"],"symbols":["main"]}`, repo)
	req := httptest.NewRequest(http.MethodPost, "/analyze", strings.NewReader(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
	}
	var resp lspbridge.AnalyzeResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "degraded" || resp.Error == "" {
		t.Fatalf("expected degraded error response, got %+v", resp)
	}
	if len(resp.LanguageErrors) != 1 || resp.LanguageErrors[0].Language != lspbridge.LangGo {
		t.Fatalf("expected per-language go error, got %+v", resp.LanguageErrors)
	}
}

func TestAnalyzeEndpoint_RejectsRepoPathOutsideAllowedRoots(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	h := NewHandlerWithOptions(fakeManager{}, nil, HandlerOptions{AllowedRepoRoots: []string{allowed}})
	body := fmt.Sprintf(`{"repo_path":%q,"changed_files":["main.go"]}`, outside)
	req := httptest.NewRequest(http.MethodPost, "/analyze", strings.NewReader(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAnalyzeEndpoint_RejectsRepoPathTraversal(t *testing.T) {
	allowed := t.TempDir()
	h := NewHandlerWithOptions(fakeManager{}, nil, HandlerOptions{AllowedRepoRoots: []string{allowed}})
	traversal := allowed + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(allowed)
	body := fmt.Sprintf(`{"repo_path":%q,"changed_files":["main.go"]}`, traversal)
	req := httptest.NewRequest(http.MethodPost, "/analyze", strings.NewReader(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAnalyzeEndpoint_RejectsChangedFileTraversal(t *testing.T) {
	repo := t.TempDir()
	h := NewHandlerWithOptions(fakeManager{}, nil, HandlerOptions{AllowedRepoRoots: []string{repo}})
	body := fmt.Sprintf(`{"repo_path":%q,"changed_files":["../main.go"]}`, repo)
	req := httptest.NewRequest(http.MethodPost, "/analyze", strings.NewReader(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAnalyzeEndpoint_RequiresTokenWhenConfigured(t *testing.T) {
	repo := t.TempDir()
	h := NewHandlerWithOptions(fakeManager{}, nil, HandlerOptions{AllowedRepoRoots: []string{repo}, AuthTokens: []string{"secret"}})
	body := fmt.Sprintf(`{"repo_path":%q,"changed_files":["main.go"]}`, repo)
	unauth := httptest.NewRequest(http.MethodPost, "/analyze", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, unauth)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthenticated request to get 401, got %d", w.Code)
	}

	auth := httptest.NewRequest(http.MethodPost, "/analyze", strings.NewReader(body))
	auth.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, auth)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("expected bearer token to authorize request")
	}
}

func TestAnalyzeEndpoint_ReturnsPulledDiagnostics(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "main.go"), "package main\nfunc main() {\n}\n")

	conn := newFakeLSPConn(t, func(method string, _ json.RawMessage) (any, *jsonRPCError) {
		switch method {
		case "textDocument/diagnostic":
			return map[string]any{
				"kind": "full",
				"items": []map[string]any{{
					"range":    map[string]any{"start": map[string]any{"line": 1, "character": 0}},
					"severity": 2,
					"source":   "fake-gopls",
					"message":  "unused variable",
				}},
			}, nil
		case "workspace/symbol":
			return []any{}, nil
		default:
			return nil, &jsonRPCError{Code: -32601, Message: "method not found"}
		}
	})
	defer conn.close()

	h := NewHandlerWithOptions(fakeManager{conn: conn}, nil, HandlerOptions{AllowedRepoRoots: []string{repo}})
	body := fmt.Sprintf(`{"repo_path":%q,"changed_files":["main.go"]}`, repo)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/analyze", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp lspbridge.AnalyzeResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.LSPAvailable || resp.Status != "ok" {
		t.Fatalf("expected ok LSP response, got %+v", resp)
	}
	if len(resp.Diagnostics) != 1 {
		t.Fatalf("expected one diagnostic, got %+v", resp.Diagnostics)
	}
	d := resp.Diagnostics[0]
	if d.File != "main.go" || d.Line != 2 || d.Severity != "warning" || d.Source != "fake-gopls" {
		t.Fatalf("unexpected diagnostic: %+v", d)
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

func newFakeLSPConn(t *testing.T, handle func(method string, params json.RawMessage) (any, *jsonRPCError)) *lspConn {
	t.Helper()
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()
	conn := newLSPConn(clientToServerW, serverToClientR)

	go func() {
		defer serverToClientW.Close()
		reader := bufio.NewReader(clientToServerR)
		for {
			data, err := readFramedMessage(reader)
			if err != nil {
				return
			}
			var req struct {
				ID     *int64          `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(data, &req); err != nil || req.ID == nil {
				continue
			}
			result, rpcErr := handle(req.Method, req.Params)
			resp := map[string]any{"jsonrpc": "2.0", "id": *req.ID}
			if rpcErr != nil {
				resp["error"] = rpcErr
			} else {
				resp["result"] = result
			}
			if err := writeFramedMessage(serverToClientW, resp); err != nil {
				return
			}
		}
	}()

	return conn
}

func readFramedMessage(r *bufio.Reader) ([]byte, error) {
	var contentLength int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length:") {
			if _, err := fmt.Sscanf(line, "Content-Length: %d", &contentLength); err != nil {
				return nil, err
			}
		}
	}
	if contentLength <= 0 {
		return nil, fmt.Errorf("invalid content length")
	}
	data := make([]byte, contentLength)
	_, err := io.ReadFull(r, data)
	return data, err
}

func writeFramedMessage(w io.Writer, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(data)); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
