package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/deprunner"
)

type fakeSandbox struct {
	resp         deprunner.UpdateResponse
	err          error
	allowScripts bool
	gotReq       deprunner.UpdateRequest
}

func (f *fakeSandbox) Update(_ context.Context, req deprunner.UpdateRequest) (deprunner.UpdateResponse, error) {
	f.gotReq = req
	return f.resp, f.err
}
func (f *fakeSandbox) AllowScripts() bool { return f.allowScripts }

func doUpdate(t *testing.T, h *Handler, token string, body deprunner.UpdateRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/update", bytes.NewReader(raw))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHandlerRejectsUnauthenticatedUpdate(t *testing.T) {
	h := NewHandler(&fakeSandbox{}, nil, HandlerOptions{AuthTokens: []string{"secret"}})
	w := doUpdate(t, h, "", deprunner.UpdateRequest{Ecosystem: deprunner.EcosystemGo, Package: "x", ToVersion: "v1", Manifests: []deprunner.ManifestFile{{Path: "go.mod", Content: "m"}}})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHandlerRejectsWrongToken(t *testing.T) {
	h := NewHandler(&fakeSandbox{}, nil, HandlerOptions{AuthTokens: []string{"secret"}})
	w := doUpdate(t, h, "nope", deprunner.UpdateRequest{Ecosystem: deprunner.EcosystemGo, Package: "x", ToVersion: "v1", Manifests: []deprunner.ManifestFile{{Path: "go.mod", Content: "m"}}})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHandlerUpdateOK(t *testing.T) {
	fs := &fakeSandbox{resp: deprunner.UpdateResponse{Status: deprunner.StatusOK, ChangedFiles: []deprunner.ManifestFile{{Path: "go.mod", Content: "m2"}}}}
	h := NewHandler(fs, nil, HandlerOptions{AuthTokens: []string{"secret"}})
	w := doUpdate(t, h, "secret", deprunner.UpdateRequest{Ecosystem: deprunner.EcosystemGo, Package: "x", ToVersion: "v1", Manifests: []deprunner.ManifestFile{{Path: "go.mod", Content: "m"}}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if fs.gotReq.Package != "x" {
		t.Fatalf("sandbox saw %#v", fs.gotReq)
	}
}

func TestHandlerUpdateApplicationErrorIs422(t *testing.T) {
	fs := &fakeSandbox{resp: deprunner.UpdateResponse{Status: deprunner.StatusError, Error: "no wheel"}}
	h := NewHandler(fs, nil, HandlerOptions{AuthTokens: []string{"secret"}})
	w := doUpdate(t, h, "secret", deprunner.UpdateRequest{Ecosystem: deprunner.EcosystemPip, Package: "x", ToVersion: "1", Manifests: []deprunner.ManifestFile{{Path: "requirements.txt", Content: "x==0.1"}}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	var resp deprunner.UpdateResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != "no wheel" {
		t.Fatalf("unexpected body %#v", resp)
	}
}

func TestHandlerHealthReportsPolicy(t *testing.T) {
	h := NewHandler(&fakeSandbox{allowScripts: true}, nil, HandlerOptions{AuthTokens: []string{"secret"}})
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var health deprunner.HealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil {
		t.Fatalf("bad health json: %v", err)
	}
	if !health.AllowScripts || !health.AuthRequired {
		t.Fatalf("unexpected health %#v", health)
	}
	if len(health.Ecosystems) != 4 {
		t.Fatalf("expected 4 ecosystems, got %v", health.Ecosystems)
	}
}

func TestHandlerOpenWhenNoTokens(t *testing.T) {
	fs := &fakeSandbox{resp: deprunner.UpdateResponse{Status: deprunner.StatusNoChange}}
	h := NewHandler(fs, nil, HandlerOptions{}) // dev mode: no tokens
	w := doUpdate(t, h, "", deprunner.UpdateRequest{Ecosystem: deprunner.EcosystemGo, Package: "x", ToVersion: "v1", Manifests: []deprunner.ManifestFile{{Path: "go.mod", Content: "m"}}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 in dev mode", w.Code)
	}
}
