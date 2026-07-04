package health

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeDB struct {
	err error
}

func (f *fakeDB) Ping(_ context.Context) error { return f.err }

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestHealthzReturnsOKWhenHeartbeatFresh(t *testing.T) {
	srv := New(&fakeDB{}, testLogger())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp healthResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %s", resp.Status)
	}
}

func TestHealthzReturns503WhenHeartbeatStale(t *testing.T) {
	srv := New(&fakeDB{}, testLogger())
	srv.lastActivityUnix.Store(time.Now().Add(-2 * time.Minute).UnixNano())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	var resp healthResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Status != "unhealthy" {
		t.Fatalf("expected status unhealthy, got %s", resp.Status)
	}
}

func TestReadyzNotReady(t *testing.T) {
	srv := New(&fakeDB{}, testLogger())
	// ready is false by default
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestReadyzReady(t *testing.T) {
	srv := New(&fakeDB{}, testLogger())
	srv.SetReady(true)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp healthResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Status != "ready" {
		t.Fatalf("expected status ready, got %s", resp.Status)
	}
}

func TestReadyzDBDown(t *testing.T) {
	srv := New(&fakeDB{err: errors.New("connection refused")}, testLogger())
	srv.SetReady(true)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	var resp healthResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Status != "not_ready" {
		t.Fatalf("expected status not_ready, got %s", resp.Status)
	}
}

func TestReadyzReportsDegradedDependency(t *testing.T) {
	srv := New(&fakeDB{}, testLogger())
	srv.SetReady(true)
	if err := srv.AddReadinessFunc("qdrant", func(ctx context.Context) error {
		return errors.New("connection refused")
	}); err != nil {
		t.Fatalf("AddReadinessFunc: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	var resp healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "degraded" {
		t.Fatalf("expected status degraded, got %s", resp.Status)
	}
	if len(resp.Degraded) != 1 || resp.Degraded[0] != "qdrant" {
		t.Fatalf("expected qdrant degraded component, got %#v", resp.Degraded)
	}
	if len(resp.Components) != 1 || resp.Components[0].Name != "qdrant" || resp.Components[0].Status != "degraded" || resp.Components[0].Error == "" {
		t.Fatalf("unexpected component status: %#v", resp.Components)
	}
}
