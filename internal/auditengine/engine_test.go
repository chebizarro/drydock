package auditengine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"drydock/internal/codemap"
	"drydock/internal/db"
	"drydock/internal/reviewengine"
	"drydock/internal/securityverify"
	"drydock/internal/testutil"
)

func TestBudgetForDepth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		depth       Depth
		modelReview bool
		votes       int
		minSeverity string
	}{
		{DepthQuick, false, 1, "high"},
		{DepthStandard, true, 1, "info"},
		{DepthDeep, true, 3, "info"},
	}
	var previousUnits int
	for _, test := range tests {
		t.Run(string(test.depth), func(t *testing.T) {
			budget, err := BudgetForDepth(test.depth)
			if err != nil {
				t.Fatalf("BudgetForDepth(%q): %v", test.depth, err)
			}
			if budget.ModelReview != test.modelReview || budget.VerifyVotes != test.votes || budget.MinSeverity != test.minSeverity {
				t.Fatalf("budget = %#v", budget)
			}
			if budget.MaxUnits <= previousUnits {
				t.Fatalf("MaxUnits = %d, want > %d", budget.MaxUnits, previousUnits)
			}
			previousUnits = budget.MaxUnits
		})
	}
	if _, err := BudgetForDepth("unknown"); err == nil {
		t.Fatal("invalid depth accepted")
	}
}

func TestSingleVoteVerifierWithFakeLLM(t *testing.T) {
	client := &testutil.FakeLLM{Responses: []string{
		`{"refuted":false,"certain":true,"reason":"reachable"}`,
		`{"cwe":"CWE-78","severity":"high","confidence":0.95,"remediation":"validate input"}`,
	}}
	engine := securityverify.New(client, securityverify.Config{
		VerifyVotes:      1,
		VerifyEndpoint:   reviewengine.ModelEndpoint{Model: "sec70b"},
		ClassifyEndpoint: reviewengine.ModelEndpoint{Model: "secclassify"},
	})
	findings, err := engine.Run(context.Background(), []reviewengine.Finding{{
		Severity: "high", Category: "security", File: "main.go", Line: 7,
		Evidence: "exec.Command(input)", Explanation: "command injection", Confidence: 0.9,
	}})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(findings) != 1 || findings[0].Category != "CWE-78" {
		t.Fatalf("findings = %#v", findings)
	}
	if len(client.Requests) != 2 {
		t.Fatalf("LLM requests = %d, want verifier + classifier", len(client.Requests))
	}
}

func TestAntaresLocalizerWithFakeLLM(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     []string
	}{
		{"files", `{"files":["z.go","a.go","a.go"]}`, []string{"a.go", "z.go"}},
		{"candidate_files", `{"candidate_files":["pkg/auth.go"]}`, []string{"pkg/auth.go"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &testutil.FakeLLM{Responses: []string{test.response}}
			localizer := NewAntaresLocalizer(client, reviewengine.ModelEndpoint{BaseURL: "http://antares", Model: "antares"})
			got, err := localizer.Localize(context.Background(), "CWE-78 command injection", []codemap.RankedSymbol{{Path: "a.go", Line: 1, Name: "main", Kind: "function"}})
			if err != nil {
				t.Fatalf("Localize: %v", err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("files = %#v, want %#v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("files = %#v, want %#v", got, test.want)
				}
			}
			if len(client.Requests) != 1 || client.Requests[0].BaseURL != "http://antares" || !client.Requests[0].JSONMode {
				t.Fatalf("request = %#v", client.Requests)
			}
		})
	}
}

type fakeLocalizer struct {
	files []string
	err   error
	calls int
}

func (f *fakeLocalizer) Localize(context.Context, string, []codemap.RankedSymbol) ([]string, error) {
	f.calls++
	return f.files, f.err
}

func TestModelLocalizationFallback(t *testing.T) {
	codeMap := &codemap.Map{
		Files:   map[string]codemap.File{"a.go": {Path: "a.go"}, "b.go": {Path: "b.go"}},
		RepoMap: []codemap.RankedSymbol{{Path: "a.go", Name: "A"}, {Path: "b.go", Name: "B"}},
	}
	heuristic := []candidateUnit{{File: "a.go", Score: 100}}
	deterministic := []reviewengine.Finding{{File: "a.go", Category: "security", Evidence: "[CWE-78] sink"}}
	tests := []struct {
		name      string
		strategy  string
		localizer *fakeLocalizer
		wantFirst string
		wantLen   int
	}{
		{"disabled", "heuristic", &fakeLocalizer{files: []string{"b.go"}}, "a.go", 1},
		{"unconfigured", "antares", nil, "a.go", 1},
		{"failure", "antares", &fakeLocalizer{err: errors.New("offline")}, "a.go", 1},
		{"configured", "antares", &fakeLocalizer{files: []string{"b.go"}}, "b.go", 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var deps Dependencies
			if test.localizer != nil {
				deps.Localizer = test.localizer
			}
			engine := New(Config{}, deps, nil)
			got := engine.applyModelLocalization(context.Background(), test.strategy, codeMap, []string{"a.go", "b.go"}, deterministic, heuristic)
			if len(got) != test.wantLen || got[0].File != test.wantFirst {
				t.Fatalf("units = %#v", got)
			}
		})
	}
}

func TestNewAntaresLocalizerRequiresConfiguration(t *testing.T) {
	client := &testutil.FakeLLM{}
	if got := NewAntaresLocalizer(client, reviewengine.ModelEndpoint{Model: "antares"}); got != nil {
		t.Fatalf("unconfigured localizer = %#v", got)
	}
	if got := NewAntaresLocalizer(client, reviewengine.ModelEndpoint{BaseURL: "http://antares"}); got != nil {
		t.Fatalf("model-less localizer = %#v", got)
	}
}

func TestAuditStateMachineAndStuckRecovery(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	id, err := store.CreateSecurityAudit(ctx, "repo", "HEAD", "deep", "requester")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.StartSecurityAudit(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := store.StartSecurityAudit(ctx, id); err == nil {
		t.Fatal("duplicate running transition accepted")
	}
	reset, err := store.ResetStuckAudits(ctx)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if reset != 1 {
		t.Fatalf("reset = %d, want 1", reset)
	}
	if err := store.StartSecurityAudit(ctx, id); err != nil {
		t.Fatalf("restart after recovery: %v", err)
	}
	if err := store.PublishSecurityAudit(ctx, id, "report", "sarif"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := store.FailSecurityAudit(ctx, id); err == nil {
		t.Fatal("published -> failed transition accepted")
	}

	var state, reportID, sarifHash string
	if err := store.DB().QueryRowContext(ctx, "SELECT state, report_event_id, sarif_hash FROM security_audits WHERE id=?", id).Scan(&state, &reportID, &sarifHash); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if state != "published" || reportID != "report" || sarifHash != "sarif" {
		t.Fatalf("state = %q, report = %q, sarif = %q", state, reportID, sarifHash)
	}
}
