package nostrscan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/codemap"
	"drydock/internal/securityscan"
	"drydock/internal/securityscan/surface"
)

func TestAbsenceV2WrapperDominatesUse(t *testing.T) {
	result := analyzeAbsenceFixture(t, "v2", "fixed.go")
	if findingByRule(result.Findings, "NOSTR-V2") != nil {
		t.Fatalf("wrapper verification should dominate store path: %#v", result.Findings)
	}
}

func TestAbsenceV2OneOfThreePathsUnchecked(t *testing.T) {
	result := analyzeAbsenceFixture(t, "v2", "vulnerable.go")
	finding := findingByRule(result.Findings, "NOSTR-V2")
	if finding == nil {
		t.Fatalf("unchecked third path did not produce NOSTR-V2: %#v", result.Findings)
	}
	if !strings.Contains(finding.Evidence, "unsafePathThree") || !strings.Contains(finding.Evidence, "displayEventThree") {
		t.Fatalf("finding does not identify unchecked graph path: %q", finding.Evidence)
	}
}

func TestAbsenceRulesVulnerableAndFixedVariants(t *testing.T) {
	tests := []struct {
		role   string
		ruleID string
	}{
		{role: "v1", ruleID: "NOSTR-V1"},
		{role: "v7", ruleID: "NOSTR-V7"},
		{role: "r1", ruleID: "NOSTR-R1"},
		{role: "r2", ruleID: "NOSTR-R2"},
	}
	for _, tt := range tests {
		t.Run(tt.ruleID+"/vulnerable", func(t *testing.T) {
			result := analyzeAbsenceFixture(t, tt.role, "vulnerable.go")
			if findingByRule(result.Findings, tt.ruleID) == nil {
				t.Fatalf("%s did not flag vulnerable fixture: %#v", tt.ruleID, result.Findings)
			}
		})
		t.Run(tt.ruleID+"/fixed", func(t *testing.T) {
			result := analyzeAbsenceFixture(t, tt.role, "fixed.go")
			if findingByRule(result.Findings, tt.ruleID) != nil {
				t.Fatalf("%s flagged fixed false-positive fixture: %#v", tt.ruleID, result.Findings)
			}
		})
	}
}

func TestAbsenceConfidenceIsCappedAndEvidenceCarriesPath(t *testing.T) {
	result := analyzeAbsenceFixture(t, "v2", "vulnerable.go")
	if len(result.Findings) == 0 {
		t.Fatal("expected absence findings")
	}
	for _, finding := range result.Findings {
		if finding.Confidence != AbsenceConfidence || finding.Confidence >= 0.8 {
			t.Errorf("%s confidence = %v, must remain below gate", finding.RuleID, finding.Confidence)
		}
		if !strings.Contains(finding.Evidence, " -> ") {
			t.Errorf("%s evidence lacks ingest-to-use graph path: %q", finding.RuleID, finding.Evidence)
		}
	}
}

func TestAbsenceUsesNostrSurfaceTags(t *testing.T) {
	repo := t.TempDir()
	source := "package fixture\nfunc receive(raw string) { consume(raw) }\nfunc consume(raw string) { persistEvent(raw) }\nfunc persistEvent(string) {}\n"
	if err := os.WriteFile(filepath.Join(repo, "app.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	codeMap := &codemap.Map{
		Files: map[string]codemap.File{"app.go": {
			Path: "app.go",
			Symbols: []codemap.Symbol{
				{ID: "receive", Name: "receive", Path: "app.go", StartLine: 2, EndLine: 2},
				{ID: "consume", Name: "consume", Path: "app.go", StartLine: 3, EndLine: 3},
				{ID: "persist", Name: "persistEvent", Path: "app.go", StartLine: 4, EndLine: 4},
			},
		}},
		CallGraph: map[string][]string{"receive": {"consume"}, "consume": {"persist"}},
	}
	tagged := surface.Result{Locations: []surface.Location{{Tag: "nostr-event-ingest", File: "app.go", Line: 3}}}
	result := AnalyzeAbsences(context.Background(), repo, codeMap, tagged)
	finding := findingByRule(result.Findings, "NOSTR-V2")
	if finding == nil || !strings.Contains(finding.Evidence, "consume") {
		t.Fatalf("surface-tagged ingest did not seed call-graph walk: %#v", result.Findings)
	}
}

func analyzeAbsenceFixture(t *testing.T, role, variant string) securityscan.ScanResult {
	t.Helper()
	repo := t.TempDir()
	data, err := os.ReadFile(filepath.Join("testdata", "absence", role, variant))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "app.go"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	runAbsenceGit(t, repo, "init")
	runAbsenceGit(t, repo, "config", "user.email", "nostrscan@example.com")
	runAbsenceGit(t, repo, "config", "user.name", "Nostrscan Test")
	runAbsenceGit(t, repo, "add", "app.go")
	runAbsenceGit(t, repo, "commit", "-m", "fixture")

	codeMap, err := codemap.New(codemap.WithCacheDir(t.TempDir())).Build(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return AnalyzeAbsences(context.Background(), repo, codeMap, surface.Result{})
}

func runAbsenceGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func findingByRule(findings []securityscan.SecurityFinding, ruleID string) *securityscan.SecurityFinding {
	for i := range findings {
		if findings[i].RuleID == ruleID {
			return &findings[i]
		}
	}
	return nil
}
