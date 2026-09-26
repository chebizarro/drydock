package deprunner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientUpdateForwardsTokenAndParsesResponse(t *testing.T) {
	var gotAuth string
	var gotReq UpdateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(UpdateResponse{
			Status:       StatusOK,
			ChangedFiles: []ManifestFile{{Path: "go.mod", Content: "module x\nrequire foo v1.2.3\n"}},
		})
	}))
	defer srv.Close()

	c := NewClientWithToken(srv.URL, "secret-token")
	resp, err := c.Update(context.Background(), UpdateRequest{
		Ecosystem: EcosystemGo, Package: "foo", ToVersion: "v1.2.3",
		Manifests: []ManifestFile{{Path: "go.mod", Content: "module x\n"}},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q, want bearer token", gotAuth)
	}
	if gotReq.Package != "foo" {
		t.Fatalf("server saw package %q", gotReq.Package)
	}
	if resp.Status != StatusOK || len(resp.ChangedFiles) != 1 {
		t.Fatalf("unexpected response %#v", resp)
	}
}

func TestClientUpdateParses422AsStructuredError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(UpdateResponse{Status: StatusError, Error: "no wheel available"})
	}))
	defer srv.Close()

	c := NewClientWithToken(srv.URL, "t")
	resp, err := c.Update(context.Background(), UpdateRequest{
		Ecosystem: EcosystemPip, Package: "foo", ToVersion: "1.0.0",
		Manifests: []ManifestFile{{Path: "requirements.txt", Content: "foo==0.9.0\n"}},
	})
	if err != nil {
		t.Fatalf("Update() error = %v, want structured 422 response", err)
	}
	if resp.Status != StatusError || resp.Error != "no wheel available" {
		t.Fatalf("unexpected response %#v", resp)
	}
}

func TestClientUpdateRejectsInvalidBeforeCall(t *testing.T) {
	c := NewClientWithToken("http://127.0.0.1:0", "t")
	_, err := c.Update(context.Background(), UpdateRequest{Ecosystem: "bogus"})
	if err == nil {
		t.Fatal("expected client-side validation error")
	}
}

func TestClientPing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("ping hit %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(HealthResponse{Status: "ok", AllowScripts: false, AuthRequired: true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	health, err := c.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	if health.Status != "ok" || !health.AuthRequired {
		t.Fatalf("unexpected health %#v", health)
	}
}
