package contextbuilder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"drydock/internal/embedding"
	"drydock/internal/vectorstore"
)

func TestNewQdrantProvider_NilClients(t *testing.T) {
	// Both nil → nil provider.
	p := NewQdrantProvider(nil, nil)
	if p != nil {
		t.Error("expected nil provider with nil clients")
	}

	// One nil → nil provider.
	p = NewQdrantProvider(&vectorstore.Client{}, nil)
	if p != nil {
		t.Error("expected nil provider with nil embedder")
	}

	p = NewQdrantProvider(nil, &embedding.Client{})
	if p != nil {
		t.Error("expected nil provider with nil qdrant")
	}
}

func TestQdrantProvider_LayerMeta(t *testing.T) {
	p := &QdrantProvider{}
	if p.LayerName() != "qdrant-docs" {
		t.Errorf("unexpected layer name: %s", p.LayerName())
	}
	if p.Priority() != 8 {
		t.Errorf("unexpected priority: %d", p.Priority())
	}
}

func TestQdrantProvider_EmptyPatch(t *testing.T) {
	p := &QdrantProvider{}
	result, err := p.Build(context.Background(), BuildInput{PatchEventContent: ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result for empty patch, got %q", result)
	}
}

func TestQdrantProvider_NostrRelated(t *testing.T) {
	// Mock embedding server.
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3}, "index": 0},
			},
		})
	}))
	defer embedSrv.Close()

	nipSearchCalled := false
	docsSearchCalled := false

	// Mock Qdrant server.
	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "nip_specs") {
			nipSearchCalled = true
			json.NewEncoder(w).Encode(map[string]any{
				"result": []map[string]any{
					{
						"id":    "abc",
						"score": 0.92,
						"payload": map[string]any{
							"nip_id":        "34",
							"section_title": "Patch Events",
							"content":       "Kind 1617 events represent patches.",
						},
					},
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "project_docs") {
			docsSearchCalled = true
			json.NewEncoder(w).Encode(map[string]any{
				"result": []map[string]any{
					{
						"id":    "def",
						"score": 0.85,
						"payload": map[string]any{
							"section_title": "Contributing Guide",
							"content":       "Run tests before submitting patches.",
						},
					},
				},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"result": []any{}})
	}))
	defer qdrantSrv.Close()

	embedClient := embedding.NewClient(embedSrv.URL, "", "test")
	qdrantClient := vectorstore.NewClient(qdrantSrv.URL, "")
	p := NewQdrantProvider(qdrantClient, embedClient)

	result, err := p.Build(context.Background(), BuildInput{
		PatchEventContent: "+import \"fiatjaf.com/nostr\"\n+func handleEvent() {}",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if !nipSearchCalled {
		t.Error("expected nip_specs search for Nostr-related patch")
	}
	if !docsSearchCalled {
		t.Error("expected project_docs search")
	}
	if !strings.Contains(result, "NIP-34") {
		t.Errorf("expected NIP result in output, got: %s", result)
	}
	if !strings.Contains(result, "Contributing Guide") {
		t.Errorf("expected project docs in output, got: %s", result)
	}
}

func TestQdrantProvider_NonNostr(t *testing.T) {
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3}, "index": 0},
			},
		})
	}))
	defer embedSrv.Close()

	nipSearchCalled := false

	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "nip_specs") {
			nipSearchCalled = true
		}
		json.NewEncoder(w).Encode(map[string]any{"result": []any{}})
	}))
	defer qdrantSrv.Close()

	embedClient := embedding.NewClient(embedSrv.URL, "", "test")
	qdrantClient := vectorstore.NewClient(qdrantSrv.URL, "")
	p := NewQdrantProvider(qdrantClient, embedClient)

	_, err := p.Build(context.Background(), BuildInput{
		PatchEventContent: "+func calculateTax(amount float64) float64 { return amount * 0.15 }",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if nipSearchCalled {
		t.Error("should NOT search nip_specs for non-Nostr patch")
	}
}

func TestLooksNostrRelated(t *testing.T) {
	tests := []struct {
		content string
		want    bool
	}{
		{"import fiatjaf.com/nostr", true},
		{"kind 1617 patch event", true},
		{"npub1abc...", true},
		{"bunker://pubkey?relay=wss://relay.example.com", true},
		{"func calculateTax() {}", false},
		{"import net/http", false},
		{"wss://relay.damus.io", true},
		{"NIP-46 bunker signer", true},
	}
	for _, tt := range tests {
		got := looksNostrRelated(tt.content)
		if got != tt.want {
			t.Errorf("looksNostrRelated(%q) = %v, want %v", tt.content, got, tt.want)
		}
	}
}
