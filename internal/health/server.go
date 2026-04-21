package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Checker can verify a dependency is healthy.
type Checker interface {
	Ping(ctx context.Context) error
}

// Server provides /healthz and /readyz HTTP endpoints.
type Server struct {
	mux    *http.ServeMux
	logger *slog.Logger
	db     Checker
	ready  atomic.Bool
}

// New creates a health check server.
func New(db Checker, logger *slog.Logger) *Server {
	s := &Server{
		mux:    http.NewServeMux(),
		logger: logger,
		db:     db,
	}
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/readyz", s.handleReadyz)
	return s
}

// SetReady marks the service as ready to receive traffic.
func (s *Server) SetReady(ready bool) {
	s.ready.Store(ready)
}

// ListenAndServe starts the health check server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	srv := &http.Server{
		Addr:         addr,
		Handler:      s.mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	s.logger.Info("health server listening", "addr", addr)
	return srv.ListenAndServe()
}

type healthResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(healthResponse{Status: "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if !s.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(healthResponse{Status: "not_ready", Error: "service is starting"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.db.Ping(ctx); err != nil {
		s.logger.Warn("readiness check failed: database unreachable", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(healthResponse{Status: "not_ready", Error: "database unreachable"})
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(healthResponse{Status: "ready"})
}
