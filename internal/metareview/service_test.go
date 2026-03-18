package metareview

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"drydock/internal/db"
	"drydock/internal/reviewengine"
)

type fakeClient struct {
	calls int
	resp  string
}

func (f *fakeClient) ChatCompletion(context.Context, reviewengine.ChatRequest) (string, error) {
	f.calls++
	return f.resp, nil
}

func TestMetaReviewTriggersOnLowConfidenceAndStoresLog(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	client := &fakeClient{
		resp: `{"missed_findings":[{"type":"correctness","description":"x","evidence":"y","why_missed":"prompt_gap"}],"false_positives":[],"reasoning_quality":0.8,"context_utilization":0.7,"prompt_gaps":["add guard"],"suggested_few_shot":true}`,
	}
	svc := New(Config{
		Endpoint:         reviewengine.ModelEndpoint{BaseURL: "http://meta", Model: "gpt-5-codex"},
		RandomSampleRate: 0,
	}, store, client, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	result, err := svc.Run(ctx, Input{
		PatchEventID:  "patch-1",
		RepoID:        "repo-1",
		PatchDiff:     "+line a\n-line b\n",
		ContextBundle: "ctx",
		ContextHash:   "hash-1",
		ChangedFiles:  []string{"internal/x.go"},
		LocalReview: reviewengine.ReviewerOutput{
			Summary: "s",
			Findings: []reviewengine.Finding{
				{Severity: "low", Category: "style", File: "x.go", Line: 1, Evidence: "e", Explanation: "ex", Suggestion: "s", Confidence: 0.6},
			},
		},
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if !result.Triggered {
		t.Fatalf("expected meta-review to trigger")
	}
	if client.calls != 1 {
		t.Fatalf("expected one meta-review call, got %d", client.calls)
	}
}

func TestMetaReviewReusesByContextHashAndSimilarity(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	prev := `{"missed_findings":[],"false_positives":[],"reasoning_quality":0.9,"context_utilization":0.8,"prompt_gaps":[],"suggested_few_shot":false}`
	if err := store.InsertMetaReviewLog(ctx, "p-old", "repo-1", "hash-1", []string{"foo", "bar"}, "low-confidence", prev); err != nil {
		t.Fatalf("seed meta log: %v", err)
	}

	client := &fakeClient{resp: `{"missed_findings":[],"false_positives":[],"reasoning_quality":0.1,"context_utilization":0.1,"prompt_gaps":[],"suggested_few_shot":false}`}
	svc := New(Config{
		Endpoint:         reviewengine.ModelEndpoint{BaseURL: "http://meta", Model: "gpt-5-codex"},
		RandomSampleRate: 0,
		MinReuseJaccard:  0.85,
	}, store, client, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	result, err := svc.Run(ctx, Input{
		PatchEventID:  "p-new",
		RepoID:        "repo-1",
		PatchDiff:     "+foo\n+bar\n",
		ContextBundle: "ctx",
		ContextHash:   "hash-1",
		ChangedFiles:  []string{"security/auth.go"},
		LocalReview: reviewengine.ReviewerOutput{
			Summary:   "s",
			Findings:  []reviewengine.Finding{{Severity: "high", Category: "security", File: "a.go", Line: 1, Evidence: "e", Explanation: "x", Suggestion: "s", Confidence: 0.95}},
			NeedsMoreContext: nil,
		},
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if !result.Reused {
		t.Fatalf("expected reused meta-review response")
	}
	if client.calls != 0 {
		t.Fatalf("expected zero client calls when reuse matches, got %d", client.calls)
	}
}

func mustStore(t *testing.T, ctx context.Context) *db.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "meta-test.db")
	store, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

