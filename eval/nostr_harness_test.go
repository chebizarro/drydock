package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	evaldata "drydock/internal/eval"
	"drydock/internal/nostrscan"
)

const (
	nostrAbsenceRule  = "absence"
	nostrPresenceRule = "presence"
)

type nostrScenario struct {
	ruleID    string
	ruleClass string
	role      string
	source    string
	line      int
	severity  string
}

var nostrScenarios = []nostrScenario{
	{ruleID: "NOSTR-V1", ruleClass: nostrAbsenceRule, role: "client", source: "client.go", line: 13, severity: "high"},
	{ruleID: "NOSTR-V2", ruleClass: nostrAbsenceRule, role: "client", source: "client.go", line: 12, severity: "high"},
	{ruleID: "NOSTR-V3", ruleClass: nostrPresenceRule, role: "client", source: "client.go", line: 17, severity: "high"},
	{ruleID: "NOSTR-V4", ruleClass: nostrPresenceRule, role: "signer", source: "signer.go", line: 7, severity: "high"},
	{ruleID: "NOSTR-V5", ruleClass: nostrPresenceRule, role: "client", source: "client.go", line: 19, severity: "high"},
	{ruleID: "NOSTR-V6", ruleClass: nostrPresenceRule, role: "client", source: "client.go", line: 18, severity: "critical"},
	{ruleID: "NOSTR-V7", ruleClass: nostrAbsenceRule, role: "client", source: "client.go", line: 14, severity: "critical"},
	{ruleID: "NOSTR-R1", ruleClass: nostrAbsenceRule, role: "relay", source: "relay.go", line: 13, severity: "high"},
	{ruleID: "NOSTR-R2", ruleClass: nostrAbsenceRule, role: "client", source: "client.go", line: 17, severity: "high"},
}

type labeledNostrCase struct {
	patch     evaldata.PatchCase
	ruleID    string
	ruleClass string
	role      string
	variant   string
	fixture   string
}

func TestNostrEvalReportsRuleClassesSeparately(t *testing.T) {
	cases := labeledNostrCases(t)
	assertNostrCoverage(t, cases)

	for _, ruleClass := range []string{nostrAbsenceRule, nostrPresenceRule} {
		t.Run(ruleClass, func(t *testing.T) {
			classCases := filterNostrCases(cases, ruleClass)
			withoutVerify := runNostrEval(t, classCases, false)
			withVerify := runNostrEval(t, classCases, true)

			t.Logf("nostr %s rules without securityverify: precision=%.3f recall=%.3f false-positive-rate=%.3f (tp=%d fp=%d fn=%d)",
				ruleClass, withoutVerify.Precision, withoutVerify.Recall, withoutVerify.FalsePositiveRate,
				withoutVerify.TruePositives, withoutVerify.FalsePositives, withoutVerify.FalseNegatives)
			t.Logf("nostr %s rules with securityverify: precision=%.3f recall=%.3f false-positive-rate=%.3f (tp=%d fp=%d fn=%d)",
				ruleClass, withVerify.Precision, withVerify.Recall, withVerify.FalsePositiveRate,
				withVerify.TruePositives, withVerify.FalsePositives, withVerify.FalseNegatives)

			if withoutVerify.FalsePositiveRate == 0 {
				t.Fatalf("%s fixed fixtures did not exercise false positives", ruleClass)
			}
			if withVerify.FalsePositiveRate >= withoutVerify.FalsePositiveRate {
				t.Fatalf("%s securityverify did not reduce false-positive rate: without=%.3f with=%.3f",
					ruleClass, withoutVerify.FalsePositiveRate, withVerify.FalsePositiveRate)
			}
			if withVerify.Recall < withoutVerify.Recall {
				t.Fatalf("%s securityverify reduced recall: without=%.3f with=%.3f",
					ruleClass, withoutVerify.Recall, withVerify.Recall)
			}
		})
	}
}

func runNostrEval(t *testing.T, cases []labeledNostrCase, withVerify bool) securityMetrics {
	t.Helper()
	ctx := context.Background()
	var total securityMetrics
	for _, c := range cases {
		expected := securityExpected(c.patch)
		predicted := reviewerCandidates(t, ctx, c.patch, expected)
		if withVerify {
			predicted = verifiedCandidates(t, ctx, predicted, expected)
		}
		tp, fp, fn := confusionCounts(expected, predicted)
		total.TruePositives += tp
		total.FalsePositives += fp
		total.FalseNegatives += fn
	}
	total.Precision = metricRatio(total.TruePositives, total.TruePositives+total.FalsePositives)
	total.Recall = metricRatio(total.TruePositives, total.TruePositives+total.FalseNegatives)
	total.FalsePositiveRate = metricRatio(total.FalsePositives, total.TruePositives+total.FalsePositives)
	return total
}

func labeledNostrCases(t *testing.T) []labeledNostrCase {
	t.Helper()
	cases := make([]labeledNostrCase, 0, len(nostrScenarios)*2)
	for _, scenario := range nostrScenarios {
		for _, variant := range []string{"vulnerable", "fixed"} {
			fixture := filepath.Join("testdata", "nostr", scenario.role, variant)
			file := filepath.ToSlash(filepath.Join(scenario.role, scenario.source))
			source, err := os.ReadFile(filepath.Join(fixture, scenario.source))
			if err != nil {
				t.Fatalf("read fixture source %s: %v", fixture, err)
			}
			expected := []evaldata.ExpectedFinding(nil)
			if variant == "vulnerable" {
				expected = []evaldata.ExpectedFinding{{
					Category: "security",
					File:     file,
					Line:     scenario.line,
					Severity: scenario.severity,
				}}
			}
			cases = append(cases, labeledNostrCase{
				ruleID:    scenario.ruleID,
				ruleClass: scenario.ruleClass,
				role:      scenario.role,
				variant:   variant,
				fixture:   fixture,
				patch: evaldata.PatchCase{
					CaseID:       strings.ToLower(fmt.Sprintf("nostr-%s-%s-%s", scenario.ruleID, scenario.role, variant)),
					PatchDiff:    newFilePatch(file, string(source)),
					ChangedFiles: []string{file},
					ContextBundle: fmt.Sprintf("NP25 proof-of-concept fixture: rule=%s class=%s role=%s variant=%s repository=%s",
						scenario.ruleID, scenario.ruleClass, scenario.role, variant, filepath.ToSlash(fixture)),
					ExpectedFindings: expected,
				},
			})
		}
	}
	return cases
}

func newFilePatch(path, source string) string {
	lines := strings.Split(strings.TrimSuffix(source, "\n"), "\n")
	return fmt.Sprintf("diff --git a/%s b/%s\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n+%s\n",
		path, path, path, len(lines), strings.Join(lines, "\n+"))
}

func filterNostrCases(cases []labeledNostrCase, ruleClass string) []labeledNostrCase {
	var selected []labeledNostrCase
	for _, c := range cases {
		if c.ruleClass == ruleClass {
			selected = append(selected, c)
		}
	}
	return selected
}

func assertNostrCoverage(t *testing.T, cases []labeledNostrCase) {
	t.Helper()
	wantRules := []string{"NOSTR-V1", "NOSTR-V2", "NOSTR-V3", "NOSTR-V4", "NOSTR-V5", "NOSTR-V6", "NOSTR-V7", "NOSTR-R1", "NOSTR-R2"}
	seen := make(map[string]map[string]bool)
	roles := make(map[string]bool)
	for _, c := range cases {
		if seen[c.ruleID] == nil {
			seen[c.ruleID] = make(map[string]bool)
		}
		seen[c.ruleID][c.variant] = true
		roles[c.role] = true
		if _, err := os.Stat(c.fixture); err != nil {
			t.Fatalf("fixture repository %s: %v", c.fixture, err)
		}
	}
	for _, ruleID := range wantRules {
		if !seen[ruleID]["vulnerable"] || !seen[ruleID]["fixed"] {
			t.Errorf("%s lacks vulnerable/fixed labels: %v", ruleID, seen[ruleID])
		}
	}
	for _, role := range []string{"client", "relay", "signer"} {
		if !roles[role] {
			t.Errorf("role %s has no labeled scenario", role)
		}
	}
}

func TestNostrFixtureDetectorGolden(t *testing.T) {
	type goldenProfile struct {
		Fixture string                 `json:"fixture"`
		Profile nostrscan.NostrProfile `json:"profile"`
	}
	var gotProfiles []goldenProfile
	for _, role := range []string{"client", "relay", "signer"} {
		for _, variant := range []string{"fixed", "vulnerable"} {
			fixture := filepath.Join("testdata", "nostr", role, variant)
			repo := initNostrFixtureRepo(t, fixture)
			profile, err := nostrscan.Detect(
				context.Background(),
				repo,
				"HEAD",
				nostrscan.WithCacheDir(t.TempDir()),
				nostrscan.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
			)
			if err != nil {
				t.Fatalf("detect %s: %v", fixture, err)
			}
			if !profile.IsNostr || !profileHasRole(profile, nostrscan.Role(role)) {
				t.Fatalf("fixture %s profile = %+v", fixture, profile)
			}
			gotProfiles = append(gotProfiles, goldenProfile{Fixture: filepath.ToSlash(fixture), Profile: profile})
		}
	}
	sort.Slice(gotProfiles, func(i, j int) bool { return gotProfiles[i].Fixture < gotProfiles[j].Fixture })
	got, err := json.MarshalIndent(gotProfiles, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	golden := filepath.Join("testdata", "nostr-detector.golden.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Nostr fixture detector output mismatch; run UPDATE_GOLDEN=1 go test ./eval")
	}
}

func initNostrFixtureRepo(t *testing.T, fixture string) string {
	t.Helper()
	repo := t.TempDir()
	err := filepath.WalkDir(fixture, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(fixture, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(repo, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, repo, "init", "-q")
	runFixtureGit(t, repo, "config", "user.email", "test@example.test")
	runFixtureGit(t, repo, "config", "user.name", "Test")
	runFixtureGit(t, repo, "add", ".")
	runFixtureGit(t, repo, "commit", "-qm", "fixture")
	return repo
}

func runFixtureGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func profileHasRole(profile nostrscan.NostrProfile, want nostrscan.Role) bool {
	for _, role := range profile.Roles {
		if role == want {
			return true
		}
	}
	return false
}
