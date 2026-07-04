package reviewengine

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
)

// mockEnsembleClient returns predefined responses for different models.
type mockEnsembleClient struct {
	plannerResp     string
	model1Resp      string
	model2Resp      string
	walkthroughResp string
	callCount       int
}

func (m *mockEnsembleClient) ChatCompletion(_ context.Context, req ChatRequest) (string, error) {
	m.callCount++

	// Planner call (first call or model matches planner)
	if strings.Contains(req.System, "route the review") || strings.Contains(req.System, "planner") {
		return m.plannerResp, nil
	}

	// Walkthrough call
	if strings.Contains(req.System, "walkthrough") {
		return m.walkthroughResp, nil
	}

	// Reviewer calls - alternate between model responses
	if m.model2Resp != "" && m.callCount > 2 {
		return m.model2Resp, nil
	}
	return m.model1Resp, nil
}

func TestMergeFindings_Deduplication(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Same finding from two models
	reviews := []modelResult{
		{
			Route: RouteCoder32B,
			Review: ReviewerOutput{
				Summary: "Review from model 1",
				Findings: []Finding{
					{
						Severity:   "high",
						Category:   "security",
						File:       "main.go",
						Line:       10,
						Evidence:   "SQL injection",
						Confidence: 0.85,
					},
				},
			},
		},
		{
			Route: RouteLLM70B,
			Review: ReviewerOutput{
				Summary: "Review from model 2",
				Findings: []Finding{
					{
						Severity:   "high",
						Category:   "security",
						File:       "main.go",
						Line:       10,
						Evidence:   "SQL injection vulnerability",
						Confidence: 0.80,
					},
				},
			},
		},
	}

	cfg := EnsembleConfig{
		ConsensusBoost: 0.10,
	}

	merged := mergeFindings(reviews, cfg, logger)

	if len(merged) != 1 {
		t.Errorf("expected 1 merged finding, got %d", len(merged))
	}

	// Should use higher confidence base + boost
	expectedConf := 0.85 + 0.10 // base + 1 boost
	if math.Abs(merged[0].Confidence-expectedConf) > 0.001 {
		t.Errorf("expected confidence ~%f, got %f", expectedConf, merged[0].Confidence)
	}
}

func TestMergeFindings_ConsensusBoost(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Finding from 3 models
	reviews := []modelResult{
		{Route: RouteCoder32B, Review: ReviewerOutput{Summary: "1", Findings: []Finding{
			{Severity: "medium", Category: "correctness", File: "a.go", Line: 5, Confidence: 0.70},
		}}},
		{Route: RouteLLM70B, Review: ReviewerOutput{Summary: "2", Findings: []Finding{
			{Severity: "medium", Category: "correctness", File: "a.go", Line: 5, Confidence: 0.65},
		}}},
		{Route: RouteCoder14B, Review: ReviewerOutput{Summary: "3", Findings: []Finding{
			{Severity: "medium", Category: "correctness", File: "a.go", Line: 5, Confidence: 0.68},
		}}},
	}

	cfg := EnsembleConfig{
		ConsensusBoost: 0.10,
	}

	merged := mergeFindings(reviews, cfg, logger)

	if len(merged) != 1 {
		t.Errorf("expected 1 merged finding, got %d", len(merged))
	}

	// 3 models = 2 boosts: 0.70 + 0.20 = 0.90
	expectedConf := 0.90
	if math.Abs(merged[0].Confidence-expectedConf) > 0.001 {
		t.Errorf("expected confidence ~%f, got %f", expectedConf, merged[0].Confidence)
	}
}

func TestMergeFindings_RequireConsensus(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	reviews := []modelResult{
		{Route: RouteCoder32B, Review: ReviewerOutput{Summary: "1", Findings: []Finding{
			{Severity: "high", Category: "security", File: "a.go", Line: 5, Confidence: 0.90},
			{Severity: "low", Category: "style", File: "b.go", Line: 10, Confidence: 0.70},
		}}},
		{Route: RouteLLM70B, Review: ReviewerOutput{Summary: "2", Findings: []Finding{
			{Severity: "high", Category: "security", File: "a.go", Line: 5, Confidence: 0.85},
			// Does not report the style issue
		}}},
	}

	cfg := EnsembleConfig{
		ConsensusBoost:   0.10,
		RequireConsensus: true,
	}

	merged := mergeFindings(reviews, cfg, logger)

	// Only the security finding should remain (reported by both)
	if len(merged) != 1 {
		t.Errorf("expected 1 finding (consensus required), got %d", len(merged))
	}
	if merged[0].Category != "security" {
		t.Errorf("expected security finding, got %s", merged[0].Category)
	}
}

func TestMergeFindings_UniqueFindings(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	reviews := []modelResult{
		{Route: RouteCoder32B, Review: ReviewerOutput{Summary: "1", Findings: []Finding{
			{Severity: "high", Category: "security", File: "a.go", Line: 5, Confidence: 0.90},
		}}},
		{Route: RouteLLM70B, Review: ReviewerOutput{Summary: "2", Findings: []Finding{
			{Severity: "medium", Category: "correctness", File: "b.go", Line: 10, Confidence: 0.80},
		}}},
	}

	cfg := EnsembleConfig{
		ConsensusBoost:   0.10,
		RequireConsensus: false,
	}

	merged := mergeFindings(reviews, cfg, logger)

	// Both unique findings should be preserved
	if len(merged) != 2 {
		t.Errorf("expected 2 findings, got %d", len(merged))
	}
}

func TestMergeFindings_SortOrder(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	reviews := []modelResult{
		{Route: RouteCoder32B, Review: ReviewerOutput{Summary: "1", Findings: []Finding{
			{Severity: "low", Category: "style", File: "c.go", Line: 30, Confidence: 0.90},
			{Severity: "high", Category: "security", File: "a.go", Line: 10, Confidence: 0.85},
			{Severity: "medium", Category: "correctness", File: "b.go", Line: 20, Confidence: 0.88},
		}}},
	}

	cfg := EnsembleConfig{}

	merged := mergeFindings(reviews, cfg, logger)

	// Should be sorted: high > medium > low
	if len(merged) != 3 {
		t.Fatalf("expected 3 findings, got %d", len(merged))
	}
	if merged[0].Severity != "high" {
		t.Errorf("first finding should be high, got %s", merged[0].Severity)
	}
	if merged[1].Severity != "medium" {
		t.Errorf("second finding should be medium, got %s", merged[1].Severity)
	}
	if merged[2].Severity != "low" {
		t.Errorf("third finding should be low, got %s", merged[2].Severity)
	}
}

func TestMergeFindings_ConfidenceCap(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Same finding from many models - should cap at 1.0
	reviews := []modelResult{
		{Route: RouteCoder32B, Review: ReviewerOutput{Summary: "1", Findings: []Finding{
			{Severity: "high", Category: "security", File: "a.go", Line: 5, Confidence: 0.95},
		}}},
		{Route: RouteLLM70B, Review: ReviewerOutput{Summary: "2", Findings: []Finding{
			{Severity: "high", Category: "security", File: "a.go", Line: 5, Confidence: 0.90},
		}}},
		{Route: RouteCoder14B, Review: ReviewerOutput{Summary: "3", Findings: []Finding{
			{Severity: "high", Category: "security", File: "a.go", Line: 5, Confidence: 0.92},
		}}},
	}

	cfg := EnsembleConfig{
		ConsensusBoost: 0.20, // Would be 0.95 + 0.40 = 1.35
	}

	merged := mergeFindings(reviews, cfg, logger)

	if merged[0].Confidence > 1.0 {
		t.Errorf("confidence should be capped at 1.0, got %f", merged[0].Confidence)
	}
	if merged[0].Confidence != 1.0 {
		t.Errorf("expected confidence 1.0 (capped), got %f", merged[0].Confidence)
	}
}

func TestCollectNeedsMoreContext(t *testing.T) {
	reviews := []modelResult{
		{Review: ReviewerOutput{NeedsMoreContext: []string{"config.yaml", "utils.go"}}},
		{Review: ReviewerOutput{NeedsMoreContext: []string{"config.yaml", "main.go"}}}, // config.yaml is duplicate
	}

	result := collectNeedsMoreContext(reviews)

	// Should deduplicate
	if len(result) != 3 {
		t.Errorf("expected 3 unique context requests, got %d: %v", len(result), result)
	}
}

func TestFindingKey(t *testing.T) {
	f1 := Finding{File: "Main.go", Line: 10, Category: "SECURITY"}
	f2 := Finding{File: "main.go", Line: 10, Category: "security"}

	k1 := findingKey(f1)
	k2 := findingKey(f2)

	// Keys should match (case-insensitive)
	if k1 != k2 {
		t.Errorf("keys should match: %s vs %s", k1, k2)
	}
}

type failingEnsembleReviewerClient struct{}

func (f *failingEnsembleReviewerClient) ChatCompletion(_ context.Context, req ChatRequest) (string, error) {
	if strings.Contains(req.System, "planner") {
		return `{"change_type":"feature","risk_areas":[],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`, nil
	}
	if req.Model == "llm70b" {
		return `{"summary":`, nil
	}
	return `{"summary":"single model success","findings":[],"needs_more_context":[]}`, nil
}

func TestRunEnsembleFailsClosedWhenRequiredReviewerFails(t *testing.T) {
	engine := New(Config{
		Planner:  ModelEndpoint{Model: "planner"},
		Coder32B: ModelEndpoint{Model: "coder32b"},
		LLM70B:   ModelEndpoint{Model: "llm70b"},
	}, &failingEnsembleReviewerClient{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := engine.RunEnsemble(context.Background(), RunInput{
		ContextBundle:   "ctx",
		ChangedFiles:    []string{"main.go"},
		SkipWalkthrough: true,
	}, EnsembleConfig{Enabled: true, Models: []ModelRoute{RouteCoder32B, RouteLLM70B}})
	if err == nil {
		t.Fatal("expected ensemble to fail closed when a required reviewer fails")
	}
	if !strings.Contains(err.Error(), "ensemble failed closed") || !strings.Contains(err.Error(), "llm70b") {
		t.Fatalf("expected clear fail-closed error naming failed reviewer, got: %v", err)
	}
}

func TestDefaultEnsembleConfig(t *testing.T) {
	cfg := DefaultEnsembleConfig()

	if cfg.Enabled {
		t.Error("default ensemble should be disabled")
	}
	if len(cfg.Models) != 2 {
		t.Errorf("expected 2 default models, got %d", len(cfg.Models))
	}
	if cfg.ConsensusBoost != 0.10 {
		t.Errorf("expected consensus boost 0.10, got %f", cfg.ConsensusBoost)
	}
}

// mockEnsembleEngine wraps the engine for testing
func setupEnsembleTest(t *testing.T) (*Engine, *mockEnsembleClient) {
	t.Helper()

	plannerResp, _ := json.Marshal(PlannerOutput{
		ChangeType:  "feature",
		RiskAreas:   []string{"security"},
		ReviewFocus: "test focus",
		ModelRoute:  RouteCoder32B,
	})

	review1, _ := json.Marshal(ReviewerOutput{
		Summary: "Review from model 1",
		Findings: []Finding{
			{Severity: "high", Category: "security", File: "main.go", Line: 10, Evidence: "issue", Explanation: "bad", Confidence: 0.85},
		},
	})

	review2, _ := json.Marshal(ReviewerOutput{
		Summary: "Review from model 2",
		Findings: []Finding{
			{Severity: "high", Category: "security", File: "main.go", Line: 10, Evidence: "same issue", Explanation: "also bad", Confidence: 0.80},
			{Severity: "medium", Category: "correctness", File: "util.go", Line: 20, Evidence: "unique", Explanation: "only in model 2", Confidence: 0.75},
		},
	})

	walkthrough, _ := json.Marshal(WalkthroughOutput{
		Walkthrough: "Test walkthrough",
	})

	client := &mockEnsembleClient{
		plannerResp:     string(plannerResp),
		model1Resp:      string(review1),
		model2Resp:      string(review2),
		walkthroughResp: string(walkthrough),
	}

	engine := New(Config{
		Planner:  ModelEndpoint{Model: "planner"},
		Coder32B: ModelEndpoint{Model: "coder32b"},
		LLM70B:   ModelEndpoint{Model: "llm70b"},
	}, client, slog.New(slog.NewTextHandler(io.Discard, nil)))

	return engine, client
}
