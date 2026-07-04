package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"drydock/internal/metrics"
)

// Checker can verify a dependency is healthy.
type Checker interface {
	Ping(ctx context.Context) error
}

// CheckFunc adapts a function into a Checker.
type CheckFunc func(ctx context.Context) error

// Ping runs the adapted readiness check.
func (f CheckFunc) Ping(ctx context.Context) error { return f(ctx) }

type namedCheck struct {
	name    string
	checker Checker
}

// Server provides /healthz and /readyz HTTP endpoints.
type Server struct {
	mux              *http.ServeMux
	srv              *http.Server
	logger           *slog.Logger
	db               Checker
	ready            atomic.Bool
	lastActivityUnix atomic.Int64
	heartbeatTimeout time.Duration
	checksMu         sync.RWMutex
	readinessChecks  []namedCheck
}

// New creates a health check server.
func New(db Checker, logger *slog.Logger) *Server {
	s := &Server{
		mux:              http.NewServeMux(),
		logger:           logger,
		db:               db,
		heartbeatTimeout: 60 * time.Second,
	}
	s.RecordActivity()
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/readyz", s.handleReadyz)
	s.mux.Handle("/metrics", metrics.Handler())
	return s
}

// Mux returns the underlying ServeMux so additional handlers can be registered.
func (s *Server) Mux() *http.ServeMux { return s.mux }

// SetReady marks the service as ready to receive traffic.
func (s *Server) SetReady(ready bool) {
	s.ready.Store(ready)
}

// AddReadinessCheck registers an additional dependency checked by /readyz.
// Use this for configured services such as Qdrant and embedding providers.
func (s *Server) AddReadinessCheck(name string, checker Checker) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("readiness check name is required")
	}
	if checker == nil {
		return fmt.Errorf("readiness check %q has nil checker", name)
	}

	s.checksMu.Lock()
	defer s.checksMu.Unlock()
	s.readinessChecks = append(s.readinessChecks, namedCheck{name: name, checker: checker})
	return nil
}

// AddReadinessFunc registers an additional dependency check function for /readyz.
func (s *Server) AddReadinessFunc(name string, fn func(context.Context) error) error {
	if fn == nil {
		return fmt.Errorf("readiness check %q has nil function", strings.TrimSpace(name))
	}
	return s.AddReadinessCheck(name, CheckFunc(fn))
}

// RecordActivity updates the service heartbeat timestamp.
func (s *Server) RecordActivity() {
	s.lastActivityUnix.Store(time.Now().UnixNano())
}

// ListenAndServe starts the health check server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	s.srv = &http.Server{
		Addr:         addr,
		Handler:      s.mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	s.logger.Info("health server listening", "addr", addr)
	return s.srv.ListenAndServe()
}

// Shutdown gracefully shuts down the health server, waiting for in-flight
// requests to complete within the given context deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	s.logger.Info("shutting down health server")
	return s.srv.Shutdown(ctx)
}

type healthResponse struct {
	Status     string            `json:"status"`
	Error      string            `json:"error,omitempty"`
	Degraded   []string          `json:"degraded,omitempty"`
	Components []componentStatus `json:"components,omitempty"`
}

type componentStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) readinessCheckSnapshot() []namedCheck {
	s.checksMu.RLock()
	defer s.checksMu.RUnlock()
	checks := make([]namedCheck, len(s.readinessChecks))
	copy(checks, s.readinessChecks)
	return checks
}

func (s *Server) runReadinessChecks(ctx context.Context) []componentStatus {
	checks := s.readinessCheckSnapshot()
	components := make([]componentStatus, 0, len(checks))
	for _, check := range checks {
		component := componentStatus{Name: check.name, Status: "ok"}
		if err := check.checker.Ping(ctx); err != nil {
			component.Status = "degraded"
			component.Error = err.Error()
		}
		components = append(components, component)
	}
	return components
}

func degradedComponents(components []componentStatus) []string {
	var degraded []string
	for _, component := range components {
		if component.Status == "degraded" {
			degraded = append(degraded, component.Name)
		}
	}
	return degraded
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	lastActivity := time.Unix(0, s.lastActivityUnix.Load())
	if time.Since(lastActivity) > s.heartbeatTimeout {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(healthResponse{Status: "unhealthy", Error: "heartbeat stale"})
		return
	}

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
		json.NewEncoder(w).Encode(healthResponse{Status: "not_ready", Error: "database unreachable", Degraded: []string{"database"}, Components: []componentStatus{{Name: "database", Status: "degraded", Error: err.Error()}}})
		return
	}

	components := s.runReadinessChecks(ctx)
	degraded := degradedComponents(components)
	if len(degraded) > 0 {
		s.logger.Warn("readiness check failed: dependencies degraded", "components", strings.Join(degraded, ","))
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(healthResponse{Status: "degraded", Error: "dependencies degraded", Degraded: degraded, Components: components})
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(healthResponse{Status: "ready", Components: components})
}
