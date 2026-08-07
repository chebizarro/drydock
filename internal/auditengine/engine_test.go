package auditengine

import (
	"context"
	"path/filepath"
	"testing"

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
