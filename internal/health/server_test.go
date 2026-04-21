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
)

type fakeDB struct {
	err error
}

func (f *fakeDB) Ping(_ context.Context) error { return f.err }

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestHealthzAlwaysOK(t *testing.T) {
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
