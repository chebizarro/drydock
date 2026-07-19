package reviewengine

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func identityTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestModelIdentityResolve(t *testing.T) {
	mi := NewModelIdentity()

	// Unobserved endpoints fall back to the configured name.
	if got := mi.Resolve("http://a", "", "configured"); got != "configured" {
		t.Fatalf("expected configured fallback, got %q", got)
	}

	mi.Observe("http://a", "", "configured", "actually-served")
	if got := mi.Resolve("http://a", "", "configured"); got != "actually-served" {
		t.Fatalf("expected served model, got %q", got)
	}

	// Observations are scoped to (base URL, configured model).
	if got := mi.Resolve("http://b", "", "configured"); got != "configured" {
		t.Fatalf("expected other endpoint to be unaffected, got %q", got)
	}

	// Empty observations are ignored.
	mi.Observe("http://a", "", "configured", "  ")
	if got := mi.Resolve("http://a", "", "configured"); got != "actually-served" {
		t.Fatalf("expected empty observation to be ignored, got %q", got)
	}
}

func TestModelIdentityNilSafety(t *testing.T) {
	var mi *ModelIdentity
	mi.Observe("http://a", "", "configured", "served")
	if got := mi.Resolve("http://a", "", "configured"); got != "configured" {
		t.Fatalf("nil registry should return configured name, got %q", got)
	}
}

func TestOpenAICompatClientObservesServedModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gemma-4-26b","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	mi := NewModelIdentity()
	client := NewOpenAICompatClient()
	client.Identity = mi

	out, err := client.ChatCompletion(context.Background(), ChatRequest{
		BaseURL: srv.URL,
		Model:   "llama-3.3-70b-instruct",
		System:  "s",
		User:    "u",
	})
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	if out.Content != "ok" {
		t.Fatalf("unexpected content %q", out.Content)
	}
	if out.Model != "gemma-4-26b" {
		t.Fatalf("expected served model in result, got %q", out.Model)
	}
	if got := mi.Resolve(srv.URL, "", "llama-3.3-70b-instruct"); got != "gemma-4-26b" {
		t.Fatalf("expected observed served model, got %q", got)
	}
}

func TestVerifyEndpointsSeedsSingleServedModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gemma-4-26b"}]}`))
	}))
	defer srv.Close()

	mi := NewModelIdentity()
	mi.VerifyEndpoints(context.Background(), srv.Client(), identityTestLogger(),
		ModelEndpoint{BaseURL: srv.URL, Model: "llama-3.3-70b-instruct"},
	)
	if got := mi.Resolve(srv.URL, "", "llama-3.3-70b-instruct"); got != "gemma-4-26b" {
		t.Fatalf("expected registry seeded with served model, got %q", got)
	}
}

func TestVerifyEndpointsMultiModelDoesNotSeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
	}))
	defer srv.Close()

	mi := NewModelIdentity()
	mi.VerifyEndpoints(context.Background(), srv.Client(), identityTestLogger(),
		ModelEndpoint{BaseURL: srv.URL, Model: "model-a"},
	)
	// Ambiguous listing: keep the configured name until responses are observed.
	if got := mi.Resolve(srv.URL, "", "model-a"); got != "model-a" {
		t.Fatalf("expected configured name for multi-model endpoint, got %q", got)
	}
}

func TestModelForRoutePrefersObservedIdentity(t *testing.T) {
	engine := New(Config{
		Coder32B: ModelEndpoint{BaseURL: "http://coder", Model: "qwen2.5-coder-32b"},
	}, nil, identityTestLogger())

	// Without a registry, the configured model is reported.
	if got := engine.ModelForRoute(RouteCoder32B); got != "qwen2.5-coder-32b" {
		t.Fatalf("expected configured model, got %q", got)
	}

	mi := NewModelIdentity()
	mi.Observe("http://coder", "", "qwen2.5-coder-32b", "gemma-4-26b")
	engine.UseModelIdentity(mi)
	if got := engine.ModelForRoute(RouteCoder32B); got != "gemma-4-26b" {
		t.Fatalf("expected observed served model, got %q", got)
	}
}
