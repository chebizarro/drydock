// Package server implements the dep-runner sidecar HTTP API: an isolated place
// where package-manager toolchains run against untrusted repository manifests.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/deprunner"
)

// maxRequestBytes caps the decoded request body. Manifests are text; a request
// larger than this is refused before it touches the sandbox.
const maxRequestBytes = 32 << 20 // 32 MiB

// updater is the sandbox seam the handler depends on (interface for testing:
// CI has no toolchains, mirroring lspbridge's managerAPI).
type updater interface {
	Update(ctx context.Context, req deprunner.UpdateRequest) (deprunner.UpdateResponse, error)
	AllowScripts() bool
}

// Handler serves the dep-runner HTTP API.
type Handler struct {
	sandbox    updater
	logger     *slog.Logger
	mux        *http.ServeMux
	authTokens map[string]struct{}
}

// HandlerOptions configures the HTTP API.
type HandlerOptions struct {
	// AuthTokens are the bearer / X-Drydock-Dep-Runner-Token values allowed to
	// call /update. When empty, /update is open (development only).
	AuthTokens []string
}

// NewHandler wires the routes for a sandbox and options.
func NewHandler(sandbox updater, logger *slog.Logger, opts HandlerOptions) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{
		sandbox:    sandbox,
		logger:     logger,
		mux:        http.NewServeMux(),
		authTokens: indexTokens(opts.AuthTokens),
	}
	h.mux.HandleFunc("POST /update", h.handleUpdate)
	h.mux.HandleFunc("GET /healthz", h.handleHealth)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, deprunner.HealthResponse{
		Status:       "ok",
		Ecosystems:   deprunner.SupportedEcosystems(),
		Toolchains:   detectToolchains(),
		AllowScripts: h.sandbox.AllowScripts(),
		AuthRequired: len(h.authTokens) > 0,
	})
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, deprunner.UpdateResponse{
			Status: deprunner.StatusError,
			Error:  "missing or invalid dep-runner token",
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	var req deprunner.UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, deprunner.UpdateResponse{
			Status: deprunner.StatusError,
			Error:  "invalid request body: " + err.Error(),
		})
		return
	}

	resp, err := h.sandbox.Update(r.Context(), req)
	if err != nil {
		h.logger.Error("dep-runner: update failed", "ecosystem", req.Ecosystem, "package", req.Package, "error", err)
		writeJSON(w, http.StatusInternalServerError, deprunner.UpdateResponse{
			Status: deprunner.StatusError,
			Error:  "internal error",
		})
		return
	}

	if resp.Status == deprunner.StatusError {
		writeJSON(w, http.StatusUnprocessableEntity, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) authorized(r *http.Request) bool {
	if len(h.authTokens) == 0 {
		return true
	}
	token := strings.TrimSpace(r.Header.Get("X-Drydock-Dep-Runner-Token"))
	if token == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			token = strings.TrimSpace(auth[len("bearer "):])
		}
	}
	if token == "" {
		return false
	}
	_, ok := h.authTokens[token]
	return ok
}

func detectToolchains() []string {
	var found []string
	for _, bin := range []string{"go", "npm", "cargo", "pip"} {
		if _, err := exec.LookPath(bin); err == nil {
			found = append(found, bin)
		}
	}
	return found
}

func indexTokens(tokens []string) map[string]struct{} {
	out := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		if t = strings.TrimSpace(t); t != "" {
			out[t] = struct{}{}
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
