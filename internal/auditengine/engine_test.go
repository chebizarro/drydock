package auditengine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/codemap"
	"drydock/internal/db"
	"drydock/internal/nostrscan"
	"drydock/internal/publisher"
	"drydock/internal/repoconfig"
	"drydock/internal/reviewengine"
	"drydock/internal/securityscan"
	"drydock/internal/securityscan/surface"
	"drydock/internal/securityverify"
	"drydock/internal/testutil"
)

func TestAuditCandidateLocalizationContract(t *testing.T) {
	engine := New(Config{}, Dependencies{}, nil)
	units := engine.localize(
		context.Background(),
		"",
		&codemap.Map{},
		[]string{"a.go", "b.go"},
		[]reviewengine.Finding{
			{File: "a.go", Severity: "high"},
			{File: "outside.go", Severity: "critical"},
		},
		surface.Result{Locations: []surface.Location{{File: "b.go"}}},
		"",
	)
	if len(units) != 2 {
		t.Fatalf("candidate units = %+v, want two allowed files", units)
	}
	if units[0].File != "a.go" || units[0].Score != 100 || units[1].File != "b.go" || units[1].Score != 50 {
		t.Fatalf("candidate ordering = %+v, want deterministic findings before surfaces", units)
	}
	if len(units[0].Findings) != 0 || units[0].Packet != "" {
		t.Fatalf("localization must only rank units; assembly happens later: %+v", units[0])
	}

	// unitResult is the worker boundary: findings and error travel together,
	// while Result is the exported aggregate returned by Run.
	worker := unitResult{findings: []reviewengine.Finding{{File: "a.go"}}, err: errors.New("review failed")}
	if len(worker.findings) != 1 || worker.err == nil {
		t.Fatalf("unit result contract changed: %+v", worker)
	}
	var aggregate Result
	aggregate.Findings = worker.findings
	if len(aggregate.Findings) != 1 {
		t.Fatalf("aggregate result contract changed: %+v", aggregate)
	}
}

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

func TestNostrAuditFindingsKeepRuleIDAndCWE(t *testing.T) {
	cfg := repoconfig.Default().Security.Nostr
	roles := []nostrscan.Role{nostrscan.RoleRelay}
	findings := auditNostrFindings([]securityscan.SecurityFinding{
		{RuleID: "NOSTR-V2", File: "relay.go", Evidence: "ingest -> store", Category: "security"},
		{RuleID: "NOSTR-V6", File: "relay.go", Evidence: "preview", Category: "security"},
	}, []string{"relay.go"}, roles, cfg)
	if len(findings) != 1 || findings[0].RuleID != "NOSTR-V2" {
		t.Fatalf("role-gated findings = %#v", findings)
	}
	converted := scanFindings(findings)
	if len(converted) != 1 || converted[0].Category != "security" || findingCWE(converted[0]) != "CWE-347" {
		t.Fatalf("converted findings = %#v", converted)
	}
	if converted[0].Evidence != "[CWE-347] [NOSTR-V2] ingest -> store" {
		t.Fatalf("evidence = %q", converted[0].Evidence)
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

type coverageAuditStore struct {
	coverage  []db.SecurityAuditCoverage
	failed    bool
	published bool
}

func (s *coverageAuditStore) CreateSecurityAudit(context.Context, string, string, string, string) (int64, error) {
	return 77, nil
}
func (s *coverageAuditStore) StartSecurityAudit(context.Context, int64) error { return nil }
func (s *coverageAuditStore) UpdateSecurityAuditCoverage(_ context.Context, _ int64, coverage db.SecurityAuditCoverage) error {
	s.coverage = append(s.coverage, coverage)
	return nil
}
func (s *coverageAuditStore) PublishSecurityAudit(context.Context, int64, string, string) error {
	s.published = true
	return nil
}
func (s *coverageAuditStore) FailSecurityAudit(context.Context, int64) error {
	s.failed = true
	return nil
}
func (s *coverageAuditStore) ReplaceSecurityAuditFindings(context.Context, int64, []db.SecurityAuditFinding) error {
	return nil
}
func (s *coverageAuditStore) SecurityBaselineFingerprints(context.Context, string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

type fixedAuditRepo struct{ path string }

func (r fixedAuditRepo) EnsureCanonicalRepo(context.Context, string, []string) (string, error) {
	return r.path, nil
}
func (fixedAuditRepo) EnsureCommitAvailable(context.Context, string, string, string, []string) error {
	return nil
}
func (fixedAuditRepo) CheckoutCommitOnBranch(context.Context, string, string, string) error {
	return nil
}

type fixedAuditCodeMap struct{}

func (fixedAuditCodeMap) Build(context.Context, string, string) (*codemap.Map, error) {
	return &codemap.Map{Files: map[string]codemap.File{"main.go": {Path: "main.go"}}}, nil
}

type coverageErrorScanner struct{}

func (coverageErrorScanner) ScanFiles(context.Context, string, []string, string) securityscan.ScanResult {
	return securityscan.ScanResult{FilesScanned: 2, FilesSkipped: 1, FilesErrored: 1}
}
func (coverageErrorScanner) LocateSurface(context.Context, string, []string) surface.Result {
	return surface.Result{FilesScanned: 3, FilesSkipped: 2, FilesErrored: 2}
}

type noOpAuditReviewer struct{}

func (noOpAuditReviewer) Run(context.Context, reviewengine.RunInput) (reviewengine.RunOutput, error) {
	return reviewengine.RunOutput{}, nil
}

type passAuditVerifier struct{}

func (passAuditVerifier) Run(_ context.Context, findings []reviewengine.Finding) ([]reviewengine.Finding, error) {
	return findings, nil
}

type recordingAuditPublisher struct{ called bool }

func (p *recordingAuditPublisher) PublishSecurityAudit(context.Context, publisher.PublishSecurityAuditInput) (publisher.PublishSecurityAuditResult, error) {
	p.called = true
	return publisher.PublishSecurityAuditResult{}, nil
}

func TestRunFailsAndPersistsCoverageOnScanErrors(t *testing.T) {
	repoPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoPath, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitForAuditTest(t, repoPath, "init", "-q")
	runGitForAuditTest(t, repoPath, "add", "main.go")
	runGitForAuditTest(t, repoPath, "-c", "user.name=drydock-test", "-c", "user.email=drydock@example.test", "commit", "-qm", "initial")

	store := &coverageAuditStore{}
	pub := &recordingAuditPublisher{}
	engine := New(Config{Workers: 1, NostrEnabled: "false"}, Dependencies{
		Repos: fixedAuditRepo{path: repoPath}, Store: store, CodeMap: fixedAuditCodeMap{},
		Scanner: coverageErrorScanner{}, Reviewer: noOpAuditReviewer{},
		VerifierFactory: func(int) Verifier { return passAuditVerifier{} },
		Publisher:       pub,
	}, nil)
	result, err := engine.Run(context.Background(), Request{
		RepoID: "repo", CloneURLs: []string{"https://example.test/repo.git"},
		Depth: DepthQuick, RequestedBy: "requester",
	})
	if err == nil || !strings.Contains(err.Error(), "3 file scan operation(s) errored") {
		t.Fatalf("Run() error = %v", err)
	}
	if pub.called || store.published {
		t.Fatal("audit with scan errors was published")
	}
	if !store.failed {
		t.Fatal("audit with scan errors was not marked failed")
	}
	if len(store.coverage) != 1 {
		t.Fatalf("coverage updates = %d, want 1", len(store.coverage))
	}
	want := db.SecurityAuditCoverage{ScanOperationsScanned: 5, ScanOperationsSkipped: 3, ScanOperationsErrored: 3}
	if store.coverage[0] != want || result.Coverage != want {
		t.Fatalf("coverage persisted=%+v result=%+v want=%+v", store.coverage[0], result.Coverage, want)
	}
}

func runGitForAuditTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
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
	if err := store.StartSecurityAudit(ctx, id); err == nil {
		t.Fatal("pre-outbox crash was left retryable without a durable request")
	}
	var failedState string
	if err := store.DB().QueryRowContext(ctx, "SELECT state FROM security_audits WHERE id=?", id).Scan(&failedState); err != nil {
		t.Fatalf("read recovered audit: %v", err)
	}
	if failedState != "failed" {
		t.Fatalf("recovered pre-outbox audit state = %q, want failed", failedState)
	}

	publishID, err := store.CreateSecurityAudit(ctx, "repo", "HEAD", "deep", "requester")
	if err != nil {
		t.Fatalf("create publish audit: %v", err)
	}
	if err := store.StartSecurityAudit(ctx, publishID); err != nil {
		t.Fatalf("start publish audit: %v", err)
	}
	if err := store.PublishSecurityAudit(ctx, publishID, "report", "sarif"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := store.FailSecurityAudit(ctx, publishID); err == nil {
		t.Fatal("published -> failed transition accepted")
	}

	var state, reportID, sarifHash string
	if err := store.DB().QueryRowContext(ctx, "SELECT state, report_event_id, sarif_hash FROM security_audits WHERE id=?", publishID).Scan(&state, &reportID, &sarifHash); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if state != "published" || reportID != "report" || sarifHash != "sarif" {
		t.Fatalf("state = %q, report = %q, sarif = %q", state, reportID, sarifHash)
	}
}
