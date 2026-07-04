package lspbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_Analyze(t *testing.T) {
	var gotReq AnalyzeRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/analyze" {
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotReq)
		json.NewEncoder(w).Encode(AnalyzeResponse{
			Definitions: []SymbolInfo{
				{Name: "Foo", Kind: "function", File: "main.go", Line: 10, Language: "go"},
			},
			References: []Reference{
				{Symbol: "Foo", File: "main_test.go", Line: 5, Column: 3},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	resp, err := c.Analyze(context.Background(), AnalyzeRequest{
		RepoPath:     "/tmp/repo",
		ChangedFiles: []string{"main.go"},
		Symbols:      []string{"Foo"},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if gotReq.RepoPath != "/tmp/repo" {
		t.Errorf("expected repo_path /tmp/repo, got %s", gotReq.RepoPath)
	}
	if len(resp.Definitions) != 1 || resp.Definitions[0].Name != "Foo" {
		t.Errorf("unexpected definitions: %+v", resp.Definitions)
	}
	if len(resp.References) != 1 || resp.References[0].File != "main_test.go" {
		t.Errorf("unexpected references: %+v", resp.References)
	}
}

func TestClient_Analyze_SendsBearerToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("expected bearer token, got %q", got)
		}
		json.NewEncoder(w).Encode(AnalyzeResponse{Status: "ok", LSPAvailable: true})
	}))
	defer srv.Close()

	c := NewClientWithToken(srv.URL, "secret")
	if _, err := c.Analyze(context.Background(), AnalyzeRequest{RepoPath: "/tmp/repo"}); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
}

func TestClient_Analyze_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.Analyze(context.Background(), AnalyzeRequest{RepoPath: "/tmp"})
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestClient_Ping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/healthz" {
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(HealthResponse{Status: "ok"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestClient_Ping_Failure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("expected error on 503")
	}
}

func TestClient_Ping_ConnectionRefused(t *testing.T) {
	c := NewClient("http://127.0.0.1:1") // nothing listening
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("expected error when nothing is listening")
	}
}
