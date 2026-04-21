package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"drydock/internal/lspbridge"
)

const analyzeTimeout = 30 * time.Second

// Handler provides the HTTP API for the LSP bridge.
type Handler struct {
	manager *Manager
	logger  *slog.Logger
	mux     *http.ServeMux
}

// NewHandler creates the HTTP handler and wires routes.
func NewHandler(manager *Manager, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{
		manager: manager,
		logger:  logger,
		mux:     http.NewServeMux(),
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
		Status:    "ok",
		Processes: h.manager.ProcessStatus(),
	})
}

func (h *Handler) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	var req lspbridge.AnalyzeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		statusError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.RepoPath == "" {
		statusError(w, http.StatusBadRequest, "repo_path is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), analyzeTimeout)
	defer cancel()

	resp := h.analyze(ctx, req)
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
		return lspbridge.AnalyzeResponse{Error: "no supported languages detected in changed files"}
	}

	var resp lspbridge.AnalyzeResponse

	for lang, files := range langFiles {
		conn, err := h.manager.GetOrStart(ctx, lang, req.RepoPath)
		if err != nil {
			h.logger.Warn("failed to start language server", "lang", lang, "error", err)
			continue
		}

		// Open changed files so the language server knows about them.
		for _, f := range files {
			absPath := filepath.Join(req.RepoPath, f)
			if err := didOpen(conn, absPath, lang); err != nil {
				h.logger.Debug("didOpen failed", "file", f, "error", err)
			}
		}

		// Search for requested symbols.
		for _, sym := range req.Symbols {
			defs, refs := h.lookupSymbol(ctx, conn, lang, req.RepoPath, sym)
			resp.Definitions = append(resp.Definitions, defs...)
			resp.References = append(resp.References, refs...)
		}
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
