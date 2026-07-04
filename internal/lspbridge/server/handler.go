package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"drydock/internal/lspbridge"
)

const analyzeTimeout = 30 * time.Second

const (
	analyzeStatusOK       = "ok"
	analyzeStatusDegraded = "degraded"
	analyzeStatusError    = "error"
)

type managerAPI interface {
	GetOrStart(ctx context.Context, lang, repoPath string) (*lspConn, error)
	ProcessStatus() map[string]string
}

// HandlerOptions configures the LSP bridge HTTP API.
type HandlerOptions struct {
	// AllowedRepoRoots are absolute mount prefixes under which repo_path must live.
	// If empty, DRYDOCK_LSP_ALLOWED_ROOTS is used; if that is empty, the current
	// working directory is the only allowed root.
	AllowedRepoRoots []string
	// AuthTokens contains bearer/X-Drydock-LSP-Token values allowed to call
	// mutating/analysis endpoints. If empty, DRYDOCK_LSP_BRIDGE_TOKEN(S) is used.
	AuthTokens []string
}

// Handler provides the HTTP API for the LSP bridge.
type Handler struct {
	manager      managerAPI
	logger       *slog.Logger
	mux          *http.ServeMux
	allowedRoots []string
	authTokens   map[string]struct{}
}

// NewHandler creates the HTTP handler and wires routes.
func NewHandler(manager *Manager, logger *slog.Logger) *Handler {
	return NewHandlerWithOptions(manager, logger, HandlerOptions{})
}

// NewHandlerWithOptions creates the HTTP handler with explicit security options.
func NewHandlerWithOptions(manager managerAPI, logger *slog.Logger, opts HandlerOptions) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{
		manager:      manager,
		logger:       logger,
		mux:          http.NewServeMux(),
		allowedRoots: configuredAllowedRoots(opts.AllowedRepoRoots, logger),
		authTokens:   configuredAuthTokens(opts.AuthTokens),
	}
	h.mux.HandleFunc("POST /analyze", h.handleAnalyze)
	h.mux.HandleFunc("GET /healthz", h.handleHealth)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	statusOK(w, lspbridge.HealthResponse{
		Status:       "ok",
		Processes:    h.manager.ProcessStatus(),
		AllowedRoots: append([]string(nil), h.allowedRoots...),
		AuthRequired: len(h.authTokens) > 0,
	})
}

func (h *Handler) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		statusError(w, http.StatusUnauthorized, "missing or invalid LSP bridge token")
		return
	}

	var req lspbridge.AnalyzeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		statusError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.RepoPath == "" {
		statusError(w, http.StatusBadRequest, "repo_path is required")
		return
	}

	repoPath, err := h.validateRepoPath(req.RepoPath)
	if err != nil {
		statusError(w, httpStatusForPathError(err), err.Error())
		return
	}
	req.RepoPath = repoPath

	changedFiles, err := validateChangedFiles(req.ChangedFiles)
	if err != nil {
		statusError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.ChangedFiles = changedFiles

	ctx, cancel := context.WithTimeout(r.Context(), analyzeTimeout)
	defer cancel()

	resp := h.analyze(ctx, req)
	if len(resp.LanguageErrors) > 0 {
		statusJSON(w, http.StatusBadGateway, resp)
		return
	}
	statusOK(w, resp)
}

func (h *Handler) analyze(ctx context.Context, req lspbridge.AnalyzeRequest) lspbridge.AnalyzeResponse {
	// Detect languages from changed files.
	langFiles := make(map[string][]string)
	for _, f := range req.ChangedFiles {
		ext := filepath.Ext(f)
		lang := lspbridge.LangFromExt(ext)
		if lang != "" {
			langFiles[lang] = append(langFiles[lang], f)
		}
	}

	if len(langFiles) == 0 {
		return lspbridge.AnalyzeResponse{
			Status: analyzeStatusError,
			Error:  "no supported languages detected in changed files",
		}
	}

	resp := lspbridge.AnalyzeResponse{Status: analyzeStatusOK}

	for lang, files := range langFiles {
		conn, err := h.manager.GetOrStart(ctx, lang, req.RepoPath)
		if err != nil {
			h.logger.Warn("failed to start language server", "lang", lang, "error", err)
			resp.LanguageErrors = append(resp.LanguageErrors, lspbridge.LanguageError{
				Language: lang,
				Code:     "language_server_unavailable",
				Message:  err.Error(),
			})
			continue
		}
		resp.LSPAvailable = true

		// Open changed files so the language server knows about them.
		openedFiles := make([]string, 0, len(files))
		for _, f := range files {
			absPath := filepath.Join(req.RepoPath, f)
			if err := didOpen(conn, absPath, lang); err != nil {
				h.logger.Debug("didOpen failed", "file", f, "error", err)
				resp.LanguageErrors = append(resp.LanguageErrors, lspbridge.LanguageError{
					Language: lang,
					Code:     "did_open_failed",
					Message:  fmt.Sprintf("%s: %v", f, err),
				})
				continue
			}
			openedFiles = append(openedFiles, f)
		}

		resp.Diagnostics = append(resp.Diagnostics, h.collectDiagnostics(ctx, conn, req.RepoPath, openedFiles)...)

		// Search for requested symbols.
		for _, sym := range req.Symbols {
			defs, refs := h.lookupSymbol(ctx, conn, lang, req.RepoPath, sym)
			resp.Definitions = append(resp.Definitions, defs...)
			resp.References = append(resp.References, refs...)
		}
	}

	if len(resp.LanguageErrors) > 0 {
		resp.Status = analyzeStatusDegraded
		resp.Error = "one or more language servers failed"
	} else if !resp.LSPAvailable {
		resp.Status = analyzeStatusError
		resp.Error = "no language servers were available"
	}

	return resp
}

// lookupSymbol queries the language server for symbol definitions and references.
func (h *Handler) lookupSymbol(ctx context.Context, conn *lspConn, lang, repoPath, symbol string) ([]lspbridge.SymbolInfo, []lspbridge.Reference) {
	var defs []lspbridge.SymbolInfo
	var refs []lspbridge.Reference

	// workspace/symbol to find definitions.
	rawSymbols, err := workspaceSymbol(ctx, conn, symbol)
	if err != nil {
		h.logger.Debug("workspace/symbol failed", "symbol", symbol, "error", err)
		return defs, refs
	}

	for _, raw := range rawSymbols {
		var sym struct {
			Name     string `json:"name"`
			Kind     int    `json:"kind"`
			Location struct {
				URI   string `json:"uri"`
				Range struct {
					Start struct {
						Line      int `json:"line"`
						Character int `json:"character"`
					} `json:"start"`
				} `json:"range"`
			} `json:"location"`
			ContainerName string `json:"containerName"`
		}
		if err := json.Unmarshal(raw, &sym); err != nil {
			continue
		}

		// Only include symbols that match our query.
		if !strings.EqualFold(sym.Name, symbol) && !strings.Contains(strings.ToLower(sym.Name), strings.ToLower(symbol)) {
			continue
		}

		absPath := uriToPath(sym.Location.URI)
		relFile := relPath(absPath, repoPath)

		info := lspbridge.SymbolInfo{
			Name:     sym.Name,
			Kind:     lspSymbolKindName(sym.Kind),
			File:     relFile,
			Line:     sym.Location.Range.Start.Line + 1, // LSP is 0-indexed
			Language: lang,
		}
		if sym.ContainerName != "" {
			info.Detail = sym.ContainerName
		}
		defs = append(defs, info)

		// textDocument/references for this definition.
		refResults := h.findReferences(ctx, conn, sym.Location.URI,
			sym.Location.Range.Start.Line, sym.Location.Range.Start.Character,
			repoPath, symbol)
		refs = append(refs, refResults...)
	}

	return defs, refs
}

// findReferences sends textDocument/references for a specific location.
func (h *Handler) findReferences(ctx context.Context, conn *lspConn, uri string, line, col int, repoPath, symbol string) []lspbridge.Reference {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": col},
		"context":      map[string]any{"includeDeclaration": false},
	}

	result, err := conn.call(ctx, "textDocument/references", params)
	if err != nil {
		h.logger.Debug("references failed", "symbol", symbol, "error", err)
		return nil
	}

	var locations []struct {
		URI   string `json:"uri"`
		Range struct {
			Start struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"start"`
		} `json:"range"`
	}
	if err := json.Unmarshal(result, &locations); err != nil {
		return nil
	}

	refs := make([]lspbridge.Reference, 0, len(locations))
	for _, loc := range locations {
		absPath := uriToPath(loc.URI)
		refs = append(refs, lspbridge.Reference{
			Symbol: symbol,
			File:   relPath(absPath, repoPath),
			Line:   loc.Range.Start.Line + 1,
			Column: loc.Range.Start.Character + 1,
		})
	}
	return refs
}

func (h *Handler) collectDiagnostics(ctx context.Context, conn *lspConn, repoPath string, files []string) []lspbridge.Diagnostic {
	var diagnostics []lspbridge.Diagnostic
	for _, f := range files {
		absPath := filepath.Join(repoPath, f)
		diags, err := pullDiagnostics(ctx, conn, absPath, repoPath)
		if err != nil {
			h.logger.Debug("textDocument/diagnostic failed, using published diagnostics", "file", f, "error", err)
			diags = conn.publishedDiagnostics(absPath, repoPath)
		}
		diagnostics = append(diagnostics, diags...)
	}
	return diagnostics
}

func pullDiagnostics(ctx context.Context, conn *lspConn, absPath, repoPath string) ([]lspbridge.Diagnostic, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": fileURI(absPath)},
	}
	result, err := conn.call(ctx, "textDocument/diagnostic", params)
	if err != nil {
		return nil, err
	}

	var report struct {
		Items []struct {
			Range struct {
				Start struct {
					Line int `json:"line"`
				} `json:"start"`
			} `json:"range"`
			Severity int    `json:"severity"`
			Source   string `json:"source"`
			Message  string `json:"message"`
		} `json:"items"`
	}
	if err := json.Unmarshal(result, &report); err != nil {
		return nil, err
	}

	diags := make([]lspbridge.Diagnostic, 0, len(report.Items))
	for _, d := range report.Items {
		diags = append(diags, lspbridge.Diagnostic{
			File:     relPath(absPath, repoPath),
			Line:     d.Range.Start.Line + 1,
			Severity: lspDiagnosticSeverity(d.Severity),
			Message:  d.Message,
			Source:   d.Source,
		})
	}
	return diags, nil
}

// didOpen notifies the language server about an opened file.
func didOpen(conn *lspConn, absPath, lang string) error {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}

	return conn.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        fileURI(absPath),
			"languageId": lang,
			"version":    1,
			"text":       string(data),
		},
	})
}

var errRepoPathForbidden = errors.New("repo_path is outside allowed roots")

func (h *Handler) validateRepoPath(repoPath string) (string, error) {
	if containsPathTraversal(repoPath) {
		return "", fmt.Errorf("repo_path must not contain path traversal")
	}
	if !filepath.IsAbs(repoPath) {
		return "", fmt.Errorf("repo_path must be absolute")
	}
	clean := filepath.Clean(repoPath)
	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("repo_path is not accessible: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repo_path must be a directory")
	}
	realRepo, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("repo_path is not accessible: %w", err)
	}
	for _, root := range h.allowedRoots {
		if pathWithinRoot(realRepo, root) {
			return realRepo, nil
		}
	}
	return "", fmt.Errorf("%w: %s", errRepoPathForbidden, realRepo)
}

func validateChangedFiles(files []string) ([]string, error) {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if f == "" {
			continue
		}
		if containsPathTraversal(f) || filepath.IsAbs(f) {
			return nil, fmt.Errorf("changed_files must be relative paths without traversal: %s", f)
		}
		clean := filepath.Clean(f)
		if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
			return nil, fmt.Errorf("changed_files must be relative paths without traversal: %s", f)
		}
		out = append(out, filepath.ToSlash(clean))
	}
	return out, nil
}

func (h *Handler) authorized(r *http.Request) bool {
	if len(h.authTokens) == 0 {
		return true
	}
	token := strings.TrimSpace(r.Header.Get("X-Drydock-LSP-Token"))
	if token == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			token = strings.TrimSpace(auth[len("bearer "):])
		}
	}
	_, ok := h.authTokens[token]
	return ok
}

func configuredAuthTokens(tokens []string) map[string]struct{} {
	if len(tokens) == 0 {
		tokens = append(tokens, splitConfigList(os.Getenv("DRYDOCK_LSP_BRIDGE_TOKENS"))...)
		if single := strings.TrimSpace(os.Getenv("DRYDOCK_LSP_BRIDGE_TOKEN")); single != "" {
			tokens = append(tokens, single)
		}
	}
	out := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if token = strings.TrimSpace(token); token != "" {
			out[token] = struct{}{}
		}
	}
	return out
}

func configuredAllowedRoots(roots []string, logger *slog.Logger) []string {
	if len(roots) == 0 {
		roots = splitConfigList(os.Getenv("DRYDOCK_LSP_ALLOWED_ROOTS"))
	}
	if len(roots) == 0 {
		cwd, err := os.Getwd()
		if err == nil {
			roots = []string{cwd}
		}
	}

	out := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" || containsPathTraversal(root) || !filepath.IsAbs(root) {
			logger.Warn("ignoring invalid LSP allowed root", "root", root)
			continue
		}
		clean := filepath.Clean(root)
		if real, err := filepath.EvalSymlinks(clean); err == nil {
			clean = real
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

func splitConfigList(v string) []string {
	var out []string
	for _, commaPart := range strings.Split(v, ",") {
		for _, pathPart := range filepath.SplitList(commaPart) {
			if trimmed := strings.TrimSpace(pathPart); trimmed != "" {
				out = append(out, trimmed)
			}
		}
	}
	return out
}

func containsPathTraversal(p string) bool {
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

func pathWithinRoot(path, root string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func httpStatusForPathError(err error) int {
	if errors.Is(err, errRepoPathForbidden) {
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}
