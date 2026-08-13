package metareview

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"drydock/internal/db"
	"drydock/internal/metrics"
	"drydock/internal/reviewengine"
)

type fakeClient struct {
	calls    int
	resp     string
	err      error
	requests []reviewengine.ChatRequest
}

func (f *fakeClient) ChatCompletion(_ context.Context, req reviewengine.ChatRequest) (reviewengine.ChatResult, error) {
	f.calls++
	f.requests = append(f.requests, req)
	return reviewengine.ChatResult{Content: f.resp}, f.err
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
	successBefore := metrics.MetaReviewOutcomes.With("success").Value()

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
	attempt, ok, err := store.LatestMetaReviewAttempt(ctx, "patch-1", "repo-1")
	if err != nil || !ok || attempt.Status != "success" || attempt.FailureStage != "" {
		t.Fatalf("unexpected success attempt audit: attempt=%+v ok=%v err=%v", attempt, ok, err)
	}
	if got := metrics.MetaReviewOutcomes.With("success").Value(); got != successBefore+1 {
		t.Fatalf("success outcome metric = %d, want %d", got, successBefore+1)
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
			Summary:          "s",
			Findings:         []reviewengine.Finding{{Severity: "high", Category: "security", File: "a.go", Line: 1, Evidence: "e", Explanation: "x", Suggestion: "s", Confidence: 0.95}},
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

func TestMetaReviewAnalyzesSecurityVerifyOutcomes(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	client := &fakeClient{
		resp: `{"missed_findings":[],"false_positives":[],"reasoning_quality":0.8,"context_utilization":0.7,"prompt_gaps":["reduce CWE-79 false positives"],"suggested_few_shot":false}`,
	}
	svc := New(Config{
		Endpoint:         reviewengine.ModelEndpoint{BaseURL: "http://meta", Model: "meta"},
		RandomSampleRate: 0.000000001,
	}, store, client, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	result, err := svc.Run(ctx, Input{
		PatchEventID: "security-patch",
		RepoID:       "repo-1",
		PatchDiff:    "+render(userInput)",
		ContextHash:  "security-hash",
		ChangedFiles: []string{"internal/render.go"},
		LocalReview: reviewengine.ReviewerOutput{
			Summary: "no local findings",
		},
		SecurityFindings: []SecurityFinding{
			{CWE: "cwe-79", VerifyOutcome: SecurityConfirmed, Evidence: "reachable sink"},
			{CWE: "CWE-79", VerifyOutcome: SecurityRefuted, Evidence: "escaped output", RefuteVotes: 2, VerifyVotes: 3},
		},
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if !result.Triggered || !slices.Contains(result.Reasons, "security-verify-outcomes") {
		t.Fatalf("security outcomes did not trigger meta-review: %+v", result)
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(client.requests))
	}
	prompt := client.requests[0].User
	for _, want := range []string{`"category":"security"`, `"cwe":"CWE-79"`, `"verify_outcome":"confirmed"`, `"verify_outcome":"refuted"`, `"refute_rate":0.5`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("security analysis prompt missing %s: %s", want, prompt)
		}
	}
}

func TestMetaReviewPromptIncludesAgentTrace(t *testing.T) {
	prompt := metaReviewUserPrompt(Input{
		PatchDiff: "patch", ContextBundle: "context",
		DiscoveryTrace: map[string]any{"fallback_reason": "loop_exhaustion"},
		AgentTrace: reviewengine.ReviewerTrace{
			Turns: 4, ToolCalls: 7, EvidenceToolCallIDs: []string{"evidence-1"},
			StopReason: "review_submitted",
		},
	}, 4096)
	for _, want := range []string{"Agent trace metadata JSON", `"turns":4`, `"tool_calls":7`, `"evidence_tool_call_ids":["evidence-1"]`, `"stop_reason":"review_submitted"`, `"fallback_reason":"loop_exhaustion"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("agent trace prompt missing %s: %s", want, prompt)
		}
	}
}

func TestMetaReviewPromptBudgetsRealSizeInputs(t *testing.T) {
	diff := strings.Repeat("+large patch line\n", 200_000) + "PATCH_END"
	bundle := strings.Repeat("context layer\n", 100_000) + "CONTEXT_END"

	budgetedDiff, budgetedBundle := budgetMetaReviewInputs(diff, bundle, 4096)
	if got := len(budgetedDiff) + len(budgetedBundle); got > 4096 {
		t.Fatalf("budgeted inputs use %d bytes, want at most 4096", got)
	}
	for _, want := range []string{"patch truncated", "context truncated", "PATCH_END", "CONTEXT_END"} {
		if !strings.Contains(budgetedDiff+budgetedBundle, want) {
			t.Fatalf("budgeted prompt missing %q", want)
		}
	}

	in := Input{PatchDiff: diff, ContextBundle: bundle}
	first := metaReviewUserPrompt(in, 4096)
	second := metaReviewUserPrompt(in, 4096)
	if first != second {
		t.Fatal("meta-review prompt budgeting is not deterministic")
	}
	if len(first) > 8192 {
		t.Fatalf("bounded meta-review prompt unexpectedly large: %d bytes", len(first))
	}
}

func TestMetaReviewAuditsSemaphoreAcquireFailure(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	svc := New(Config{
		Endpoint:      reviewengine.ModelEndpoint{BaseURL: "http://meta", Model: "frontier"},
		MaxConcurrent: 1,
	}, store, &fakeClient{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	if err := svc.sem.Acquire(ctx, 1); err != nil {
		t.Fatalf("occupy semaphore: %v", err)
	}
	defer svc.sem.Release(1)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	result, err := svc.Run(cancelled, Input{
		PatchEventID: "queued-patch",
		RepoID:       "repo-1",
		LocalReview: reviewengine.ReviewerOutput{
			Findings: []reviewengine.Finding{{Confidence: 0.2}},
		},
	})
	if err == nil || !result.Triggered {
		t.Fatalf("expected audited semaphore failure, result=%+v err=%v", result, err)
	}
	attempt, ok, auditErr := store.LatestMetaReviewAttempt(ctx, "queued-patch", "repo-1")
	if auditErr != nil || !ok || attempt.Status != "failed" || attempt.FailureStage != "semaphore_acquire" {
		t.Fatalf("unexpected semaphore attempt audit: attempt=%+v ok=%v err=%v", attempt, ok, auditErr)
	}
}

func TestMetaReviewPersistsFailedAttemptReason(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	client := &fakeClient{err: errors.New("provider context limit exceeded")}
	svc := New(Config{
		Endpoint: reviewengine.ModelEndpoint{BaseURL: "http://meta", Model: "frontier"},
	}, store, client, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	failureBefore := metrics.MetaReviewOutcomes.With("failed").Value()

	result, err := svc.Run(ctx, Input{
		PatchEventID: "failed-patch",
		RepoID:       "repo-1",
		ContextHash:  "failed-hash",
		LocalReview: reviewengine.ReviewerOutput{
			Findings: []reviewengine.Finding{{Confidence: 0.2}},
		},
	})
	if err == nil || !result.Triggered {
		t.Fatalf("expected triggered completion failure, result=%+v err=%v", result, err)
	}

	attempt, ok, auditErr := store.LatestMetaReviewAttempt(ctx, "failed-patch", "repo-1")
	if auditErr != nil {
		t.Fatalf("load attempt audit: %v", auditErr)
	}
	if !ok || attempt.Status != "failed" || attempt.FailureStage != "completion" {
		t.Fatalf("unexpected attempt audit: %+v", attempt)
	}
	if !strings.Contains(attempt.FailureReason, "context limit exceeded") || attempt.Model != "frontier" {
		t.Fatalf("attempt did not retain failure evidence: %+v", attempt)
	}
	if got := metrics.MetaReviewOutcomes.With("failed").Value(); got != failureBefore+1 {
		t.Fatalf("failure outcome metric = %d, want %d", got, failureBefore+1)
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
