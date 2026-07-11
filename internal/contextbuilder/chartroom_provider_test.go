package contextbuilder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeAuditEmitter struct {
	action  string
	subject string
	tags    map[string]string
}

func (f *fakeAuditEmitter) EmitAudit(_ context.Context, action, subject string, tags map[string]string) error {
	f.action = action
	f.subject = subject
	f.tags = tags
	return nil
}

func TestChartRoomProviderSearchAndAudit(t *testing.T) {
	audit := &fakeAuditEmitter{}
	var gotAuth string
	var gotReq chartroomSearchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Fatalf("path = %s, want /search", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(chartroomSearchResponse{
			Mode:  "hybrid",
			Query: gotReq.Query,
			Results: []chartroomResult{{
				Document: chartroomResultDocument{ID: "doc-1", Title: "NIP-34", CanonicalURI: "nips/34.md"},
				Chunk: chartroomResultChunk{
					ID:                "chunk-1",
					HeadingPath:       []string{"Patch Events"},
					Snippet:           "Kind 1617 events represent patches.",
					CitationURI:       "nips/34.md",
					CitationStartLine: 10,
					CitationEndLine:   12,
				},
				Score: 0.91,
			}},
		})
	}))
	defer srv.Close()

	p := NewChartRoomProvider(ChartRoomConfig{
		BaseURL:     srv.URL,
		BearerToken: "secret-token",
		CorpusIDs:   []string{"corpus-1"},
		SourceIDs:   []string{"source-1"},
		Limit:       4,
		Audit:       audit,
	})
	if p == nil {
		t.Fatal("expected provider")
	}
	out, err := p.Build(context.Background(), BuildInput{
		PatchEventContent: "+func handlePatch() {}",
		RepoID:            "repo-1",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotReq.Mode != "hybrid" || gotReq.Query == "" || gotReq.Limit != 4 {
		t.Fatalf("unexpected request: %+v", gotReq)
	}
	if len(gotReq.CorpusIDs) != 1 || gotReq.CorpusIDs[0] != "corpus-1" {
		t.Fatalf("corpus ids = %#v", gotReq.CorpusIDs)
	}
	if len(gotReq.Filters.SourceIDs) != 1 || gotReq.Filters.SourceIDs[0] != "source-1" {
		t.Fatalf("source ids = %#v", gotReq.Filters.SourceIDs)
	}
	if !strings.Contains(out, "Chartroom Retrieved Context") || !strings.Contains(out, "Kind 1617 events") || !strings.Contains(out, "nips/34.md:10-12") {
		t.Fatalf("unexpected output: %s", out)
	}
	if audit.action != "chartroom-context-retrieved" || !strings.HasSuffix(audit.subject, "/search") || audit.tags["repo_id"] != "repo-1" {
		t.Fatalf("unexpected audit: %#v", audit)
	}
}

func TestChartRoomProviderEmptyPatch(t *testing.T) {
	p := NewChartRoomProvider(ChartRoomConfig{BaseURL: "http://chartroom.test"})
	out, err := p.Build(context.Background(), BuildInput{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty output, got %q", out)
	}
}

func TestNewChartRoomProviderNoBaseURL(t *testing.T) {
	if p := NewChartRoomProvider(ChartRoomConfig{}); p != nil {
		t.Fatal("expected nil provider without base URL")
	}
}
