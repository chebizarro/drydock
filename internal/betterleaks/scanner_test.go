package betterleaks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"drydock/internal/contextbuilder"
	"drydock/internal/securityscan"
)

type commandCall struct {
	dir  string
	name string
	args []string
}

type fakeRunner struct {
	lookFn func(string) (string, error)
	runFn  func(context.Context, string, string, ...string) ([]byte, error)
	calls  []commandCall
	looks  []string
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	f.looks = append(f.looks, name)
	if f.lookFn != nil {
		return f.lookFn(name)
	}
	return "/tools/" + name, nil
}

func (f *fakeRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, commandCall{dir: dir, name: name, args: append([]string(nil), args...)})
	if f.runFn == nil {
		return []byte("null"), nil
	}
	return f.runFn(ctx, dir, name, args...)
}

func TestValidationFixturesAndRawFieldsAreDropped(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		wantCount int
		wantFirst string
		wantSev   string
		wantConf  float64
	}{
		{
			name:      "valid",
			fixture:   "validation_valid.json",
			wantCount: 1,
			wantFirst: "aws-access-token",
			wantSev:   "critical",
			wantConf:  0.99,
		},
		{
			name:      "all non-valid and absent states",
			fixture:   "validation_other_states.json",
			wantCount: 6,
			wantFirst: "fixture-needs-validation",
			wantSev:   "high",
			wantConf:  0.90,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			repo := t.TempDir()
			runner := &fakeRunner{runFn: func(_ context.Context, _, name string, _ ...string) ([]byte, error) {
				if name != "/tools/betterleaks" {
					t.Fatalf("unexpected command %q", name)
				}
				return report, nil
			}}

			result, err := NewWithRunner(runner, false).Scan(context.Background(), ScanRequest{
				RepoPath:     repo,
				AllowedFiles: []string{"config/secrets.go"},
			})
			if err != nil {
				t.Fatalf("Scan() error = %v", err)
			}
			if len(result.Findings) != tc.wantCount {
				t.Fatalf("findings = %d, want %d", len(result.Findings), tc.wantCount)
			}
			first := result.Findings[0]
			if first.RuleID != tc.wantFirst || first.Severity != tc.wantSev || first.Confidence != tc.wantConf {
				t.Fatalf("first finding = %+v", first)
			}
			for _, finding := range result.Findings {
				if !finding.Sensitive {
					t.Errorf("finding %q is not sensitive", finding.RuleID)
				}
				if finding.Evidence != canonicalEvidence ||
					finding.Description != canonicalDescription ||
					finding.Suggestion != canonicalSuggestion {
					t.Errorf("finding contains non-canonical text: %+v", finding)
				}
			}

			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			rendered := FormatContext(result)
			if strings.Contains(string(encoded), "SUPER_SECRET") || strings.Contains(rendered, "SUPER_SECRET") {
				t.Fatalf("raw report data escaped redaction: json=%s context=%s", encoded, rendered)
			}
		})
	}
}

func TestSeverityForValidationStatus(t *testing.T) {
	for _, status := range []string{"", "needs_validation", "invalid", "revoked", "unknown", "error", "future_value"} {
		severity, confidence := SeverityForValidationStatus(status)
		if severity != "high" || confidence != 0.90 {
			t.Errorf("status %q = %s/%v, want high/0.90", status, severity, confidence)
		}
	}
	severity, confidence := SeverityForValidationStatus("valid")
	if severity != "critical" || confidence != 0.99 {
		t.Fatalf("valid = %s/%v, want critical/0.99", severity, confidence)
	}
}

func TestNullReportMeansNoFindings(t *testing.T) {
	result, err := scanOutput(t, []byte("null"), ScanRequest{
		RepoPath: t.TempDir(), AllowedFiles: []string{"a.go"},
	})
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("findings = %d, want 0", len(result.Findings))
	}
}

func TestMalformedOrNonArrayReportsFailClosed(t *testing.T) {
	for _, report := range []string{
		"{\"RuleID\":\"x\"}",
		"[",
		"[] {}",
		"\"not-an-array\"",
	} {
		t.Run(report, func(t *testing.T) {
			_, err := scanOutput(t, []byte(report), ScanRequest{
				RepoPath: t.TempDir(), AllowedFiles: []string{"a.go"},
			})
			if err == nil {
				t.Fatal("Scan() error = nil, want parse failure")
			}
		})
	}
}

func TestCommandArgumentsPolicyPrecedenceAndPrivateMaterialization(t *testing.T) {
	repo := t.TempDir()
	var scanDir string
	runner := &fakeRunner{}
	runner.runFn = func(_ context.Context, dir, name string, args ...string) ([]byte, error) {
		switch name {
		case "/tools/git":
			if args[0] == "ls-tree" {
				return []byte(strings.Join(policyPaths, "\n") + "\n"), nil
			}
			if args[0] != "show" {
				t.Fatalf("unexpected git args: %v", args)
			}
			switch args[1] {
			case "base123:" + betterleaksConfig:
				return []byte("betterleaks-config"), nil
			case "base123:" + betterleaksBase:
				return []byte("[{\"baseline\":true}]"), nil
			default:
				t.Fatalf("fallback policy was materialized despite betterleaks precedence: %v", args)
			}
		case "/tools/betterleaks":
			scanDir = dir
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o700 {
				t.Fatalf("temp dir mode = %o, want 700", info.Mode().Perm())
			}
			wantPrefix := []string{
				"dir", "--redact", "--report-format", "json",
				"--report-path", "-", "--exit-code", "0", "--config",
			}
			if len(args) != 14 || !reflect.DeepEqual(args[:len(wantPrefix)], wantPrefix) {
				t.Fatalf("betterleaks args = %#v", args)
			}
			if filepath.Base(args[9]) != betterleaksConfig ||
				args[10] != "--baseline-path" ||
				filepath.Base(args[11]) != betterleaksBase ||
				args[12] != "--validation" ||
				args[13] != repo {
				t.Fatalf("betterleaks args = %#v", args)
			}
			assertPrivateFile(t, args[9], "betterleaks-config")
			assertPrivateFile(t, args[11], "[{\"baseline\":true}]")
			return []byte("null"), nil
		default:
			t.Fatalf("unexpected command %q", name)
		}
		return nil, nil
	}

	_, err := NewWithRunner(runner, true).Scan(context.Background(), ScanRequest{
		RepoPath: repo, PolicyRef: "base123", AllowedFiles: []string{"a.go"},
	})
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if !reflect.DeepEqual(runner.looks, []string{"betterleaks", "git"}) {
		t.Fatalf("PATH lookups = %v", runner.looks)
	}
	if _, err := os.Stat(scanDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private temp dir was not removed: %v", err)
	}

	lsCall := runner.calls[0]
	wantLS := append([]string{"ls-tree", "--name-only", "base123", "--"}, policyPaths...)
	if lsCall.dir != repo || lsCall.name != "/tools/git" || !reflect.DeepEqual(lsCall.args, wantLS) {
		t.Fatalf("ls-tree call = %+v, want args %v", lsCall, wantLS)
	}
}

func TestGitleaksPolicyFallbackAndValidationDisabled(t *testing.T) {
	repo := t.TempDir()
	runner := &fakeRunner{runFn: func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		if name == "/tools/git" {
			if args[0] == "ls-tree" {
				return []byte(gitleaksConfig + "\n" + gitleaksBase + "\n"), nil
			}
			return []byte("fallback"), nil
		}
		for _, arg := range args {
			if arg == "--validation" {
				t.Fatal("--validation present while disabled")
			}
		}
		if filepath.Base(args[9]) != gitleaksConfig || filepath.Base(args[11]) != gitleaksBase {
			t.Fatalf("fallback args = %v", args)
		}
		return []byte("null"), nil
	}}
	_, err := NewWithRunner(runner, false).Scan(context.Background(), ScanRequest{
		RepoPath: repo, PolicyRef: "base", AllowedFiles: []string{"a.go"},
	})
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
}

func TestFilteringNormalizesPathsAndRequiresAllowedAddedLines(t *testing.T) {
	repo := t.TempDir()
	report, err := json.Marshal([]map[string]any{
		{"RuleID": "included-span", "File": filepath.Join(repo, "src/a.go"), "StartLine": 9, "EndLine": 11},
		{"RuleID": "not-added", "File": "src/a.go", "StartLine": 20, "EndLine": 20},
		{"RuleID": "not-allowed", "File": "src/b.go", "StartLine": 10, "EndLine": 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	diff := "diff --git a/src/a.go b/src/a.go\n--- a/src/a.go\n+++ b/src/a.go\n@@ -9,0 +10,2 @@\n+one\n+two\n"
	result, err := scanOutput(t, report, ScanRequest{
		RepoPath:     repo,
		AllowedFiles: []string{"src/a.go"},
		Diff:         diff,
	})
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(result.Findings) != 1 || result.Findings[0].RuleID != "included-span" ||
		result.Findings[0].File != "src/a.go" {
		t.Fatalf("filtered findings = %+v", result.Findings)
	}
}

func TestAuditFilteringUsesAllowedFilesWithoutDiff(t *testing.T) {
	report := []byte(`[
		{"RuleID":"allowed","File":"src/a.go","StartLine":2},
		{"RuleID":"excluded","File":"src/b.go","StartLine":2}
	]`)
	result, err := scanOutput(t, report, ScanRequest{
		RepoPath: t.TempDir(), AllowedFiles: []string{"src/a.go"},
	})
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(result.Findings) != 1 || result.Findings[0].RuleID != "allowed" {
		t.Fatalf("findings = %+v", result.Findings)
	}
	if result.Findings[0].EndLine != result.Findings[0].Line {
		t.Fatalf("zero EndLine was not normalized: %+v", result.Findings[0])
	}
}

func TestEscapingReportedPathFailsClosed(t *testing.T) {
	report := []byte(`[{"RuleID":"escape","File":"../outside","StartLine":1}]`)
	_, err := scanOutput(t, report, ScanRequest{
		RepoPath: t.TempDir(), AllowedFiles: []string{"safe.go"},
	})
	if err == nil || !strings.Contains(err.Error(), "escapes repository") {
		t.Fatalf("Scan() error = %v, want repository escape", err)
	}
}

func TestEscapingAllowedPathFailsBeforeCommand(t *testing.T) {
	runner := &fakeRunner{}
	_, err := NewWithRunner(runner, false).Scan(context.Background(), ScanRequest{
		RepoPath: t.TempDir(), AllowedFiles: []string{"../outside"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid allowed file") {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("commands ran for invalid allowlist: %+v", runner.calls)
	}
}

func TestCancellation(t *testing.T) {
	t.Run("before lookup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		runner := &fakeRunner{}
		_, err := NewWithRunner(runner, false).Scan(ctx, ScanRequest{RepoPath: t.TempDir()})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Scan() error = %v", err)
		}
		if len(runner.looks) != 0 || len(runner.calls) != 0 {
			t.Fatalf("runner called after cancellation: looks=%v calls=%v", runner.looks, runner.calls)
		}
	})
	t.Run("during command", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		runner := &fakeRunner{runFn: func(_ context.Context, _, _ string, _ ...string) ([]byte, error) {
			cancel()
			return nil, errors.New("killed")
		}}
		_, err := NewWithRunner(runner, false).Scan(ctx, ScanRequest{
			RepoPath: t.TempDir(), AllowedFiles: []string{"a.go"},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Scan() error = %v", err)
		}
	})
}

func TestFailClosedErrors(t *testing.T) {
	t.Run("binary missing", func(t *testing.T) {
		runner := &fakeRunner{lookFn: func(string) (string, error) {
			return "", errors.New("not found")
		}}
		_, err := NewWithRunner(runner, false).Scan(context.Background(), ScanRequest{RepoPath: t.TempDir()})
		if err == nil {
			t.Fatal("Scan() error = nil")
		}
	})
	t.Run("scan command", func(t *testing.T) {
		runner := &fakeRunner{runFn: func(context.Context, string, string, ...string) ([]byte, error) {
			return nil, errors.New("exit 2")
		}}
		_, err := NewWithRunner(runner, false).Scan(context.Background(), ScanRequest{
			RepoPath: t.TempDir(), AllowedFiles: []string{"a.go"},
		})
		if err == nil || !strings.Contains(err.Error(), "command failed") {
			t.Fatalf("Scan() error = %v", err)
		}
	})
	t.Run("policy inspection", func(t *testing.T) {
		runner := &fakeRunner{runFn: func(context.Context, string, string, ...string) ([]byte, error) {
			return nil, errors.New("bad ref")
		}}
		_, err := NewWithRunner(runner, false).Scan(context.Background(), ScanRequest{
			RepoPath: t.TempDir(), PolicyRef: "bad", AllowedFiles: []string{"a.go"},
		})
		if err == nil || !strings.Contains(err.Error(), "inspect policy ref") {
			t.Fatalf("Scan() error = %v", err)
		}
	})
	t.Run("policy show", func(t *testing.T) {
		runner := &fakeRunner{runFn: func(_ context.Context, _ string, _ string, args ...string) ([]byte, error) {
			if args[0] == "ls-tree" {
				return []byte(betterleaksConfig + "\n"), nil
			}
			return nil, errors.New("show failed")
		}}
		_, err := NewWithRunner(runner, false).Scan(context.Background(), ScanRequest{
			RepoPath: t.TempDir(), PolicyRef: "base", AllowedFiles: []string{"a.go"},
		})
		if err == nil || !strings.Contains(err.Error(), "materialize") {
			t.Fatalf("Scan() error = %v", err)
		}
	})
}

func TestProviderPassThroughAndFormatContext(t *testing.T) {
	provider := NewProvider()
	if provider.LayerName() != "secret-scan" || provider.Priority() != 1 {
		t.Fatalf("provider = %s/%d", provider.LayerName(), provider.Priority())
	}
	got, err := provider.Build(context.Background(), contextbuilder.BuildInput{SecretScanContext: "already rendered"})
	if err != nil || got != "already rendered" {
		t.Fatalf("Build() = %q, %v", got, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Build(ctx, contextbuilder.BuildInput{SecretScanContext: "ignored"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Build() error = %v", err)
	}

	result := ScanResult{Findings: []securityscan.SecurityFinding{{
		RuleID: "rule", Severity: "critical", File: "a.go", Line: 2, EndLine: 4,
		Evidence: "SUPER_SECRET_EVIDENCE", Description: "SUPER_SECRET_DESCRIPTION",
		Suggestion: "SUPER_SECRET_SUGGESTION", Sensitive: true,
	}}}
	rendered := FormatContext(result)
	if strings.Contains(rendered, "SUPER_SECRET") {
		t.Fatalf("FormatContext leaked non-canonical finding text: %s", rendered)
	}
	if !strings.Contains(rendered, "[rule] critical | a.go:2-4") ||
		!strings.Contains(rendered, "value redacted") {
		t.Fatalf("FormatContext output = %q", rendered)
	}
	if FormatContext(ScanResult{}) != "" {
		t.Fatal("empty result rendered non-empty context")
	}
}

func scanOutput(t *testing.T, output []byte, req ScanRequest) (ScanResult, error) {
	t.Helper()
	runner := &fakeRunner{runFn: func(_ context.Context, _, name string, _ ...string) ([]byte, error) {
		if name != "/tools/betterleaks" {
			t.Fatalf("unexpected command %q", name)
		}
		return output, nil
	}}
	return NewWithRunner(runner, false).Scan(context.Background(), req)
}

func assertPrivateFile(t *testing.T, path, wantContent string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode = %o, want 600", path, info.Mode().Perm())
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != wantContent {
		t.Fatalf("%s content = %q, want %q", path, content, wantContent)
	}
}

var _ Scanner = (*commandScanner)(nil)
var _ contextbuilder.Provider = (*Provider)(nil)
