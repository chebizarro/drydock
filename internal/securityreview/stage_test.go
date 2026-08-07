package securityreview

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"drydock/internal/contextbuilder"
	"drydock/internal/metrics"
	"drydock/internal/repoconfig"
	"drydock/internal/reviewengine"
	"drydock/internal/testutil"
)

func TestStageRunExtractsEvidenceRoutesVerifiesAndClassifies(t *testing.T) {
	fake := &testutil.FakeLLM{Responses: []string{
		`{"change_type":"security","risk_areas":["auth"],"needed_context":[],"review_focus":"security","model_route":"coder14b"}`,
		`{"summary":"security review","findings":[{"severity":"medium","category":"security","file":"auth.go","line":12,"evidence":"user input reaches query","explanation":"SQL injection","suggestion":"parameterize","confidence":0.8}],"needs_more_context":[]}`,
		`{"refuted":false,"certain":true,"reason":"reachable"}`,
		`{"cwe":"CWE-89","severity":"high","confidence":0.96,"remediation":"Use parameters."}`,
	}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "http://planner", Model: "planner"},
		Coder14B: reviewengine.ModelEndpoint{BaseURL: "http://coder", Model: "coder"},
		Sec70B:   reviewengine.ModelEndpoint{BaseURL: "http://security", Model: "sec-reviewer"},
	}, fake, logger)
	stage := New(
		engine,
		fake,
		reviewengine.ModelEndpoint{BaseURL: "http://verify", Model: "sec-verify"},
		reviewengine.ModelEndpoint{BaseURL: "http://classify", Model: "sec-classify"},
	)
	bundle := contextbuilder.ContextBundle{
		Content:      "## security-scan\nSAST hit\n\n## taint\nsource -> sink\n\n## security-surface\n[auth] auth.go:12 boundary",
		ChangedFiles: []string{"auth.go"},
	}
	cfg := repoconfig.Default().Security
	findingsBefore := metrics.SecurityFindings.With("CWE-89", "high").Value()
	result := stage.Run(context.Background(), bundle, t.TempDir(), cfg)
	if result.Error != nil {
		t.Fatalf("Run() error = %v", result.Error)
	}
	if result.Evidence.SAST != "SAST hit" || result.Evidence.TaintPaths != "source -> sink" || !strings.Contains(result.Evidence.SecuritySurface, "boundary") {
		t.Fatalf("unexpected evidence: %#v", result.Evidence)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(result.Findings))
	}
	if metric := metrics.SecurityFindings.With("CWE-89", "high").Value(); metric != findingsBefore+1 {
		t.Fatalf("security findings metric = %d, want %d", metric, findingsBefore+1)
	}
	finding := result.Findings[0]
	if finding.Category != "security" || finding.Severity != "high" || finding.Confidence != 0.96 {
		t.Fatalf("unexpected classified finding: %#v", finding)
	}
	if !strings.Contains(finding.Evidence, "CWE-89") || strings.Contains(finding.Evidence, "Security evidence packet") {
		t.Fatalf("published evidence should retain CWE without the verification packet: %q", finding.Evidence)
	}
	if len(fake.Requests) != 4 {
		t.Fatalf("requests = %d, want planner + reviewer + verifier + classifier", len(fake.Requests))
	}
	if fake.Requests[1].Model != "sec-reviewer" {
		t.Fatalf("reviewer model = %q, want sec-reviewer", fake.Requests[1].Model)
	}
	if !strings.Contains(fake.Requests[1].System, "trust boundary") || !strings.Contains(fake.Requests[1].System, "CWE") {
		t.Fatalf("reviewer is missing security preamble: %q", fake.Requests[1].System)
	}
	if fake.Requests[2].Model != "sec-verify" || !strings.Contains(fake.Requests[2].User, "Taint paths") {
		t.Fatalf("verifier did not receive routed evidence: %#v", fake.Requests[2])
	}
	if fake.Requests[3].Model != "sec-classify" {
		t.Fatalf("classifier model = %q, want sec-classify", fake.Requests[3].Model)
	}
}

func TestExtractEvidenceHonorsProviderToggles(t *testing.T) {
	bundle := contextbuilder.ContextBundle{
		Content: "## security-scan\nsast\n\n## taint\ntaint\n\n## security-surface\nsurface",
	}
	cfg := repoconfig.Default().Security
	cfg.Taint = false
	got := ExtractEvidence(bundle, cfg)
	if got.SAST != "sast" || got.TaintPaths != "" || got.SecuritySurface != "surface" {
		t.Fatalf("unexpected evidence: %#v", got)
	}
}
