package eval

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	evaldata "git.sharegap.net/cascadia/drydock/internal/eval"
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
	"git.sharegap.net/cascadia/drydock/internal/securityscan"
)

// TestSecurityScannerHeldoutMetrics runs the production deterministic scanner
// (securityscan.New) over the held-out patch corpus and scores its findings
// with the canonical eval.Harness metric definition — the same code path the
// monthly eval uses. There is no scripted model and no answer-key synthesis:
// the numbers move if a rule regresses.
//
// The deterministic scanner only covers regex-detectable vulnerability classes
// (hardcoded secrets, SQL/command injection, weak hashes, disabled TLS
// verification, …). Semantic issues in the corpus (SSRF, path traversal, XSS,
// missing authorization) are out of its reach by design and count as honest
// false negatives — hence the modest recall floor below. The precision floor is
// the load-bearing gate: the scanner must not flag the clean fixtures.
func TestSecurityScannerHeldoutMetrics(t *testing.T) {
	dataset, err := evaldata.LoadDataset("heldout-sample.json")
	if err != nil {
		t.Fatalf("load security eval dataset: %v", err)
	}

	cases := labeledSecurityCases(dataset.Cases)
	positiveCases, cleanCases := 0, 0
	for _, c := range cases {
		if len(securityExpected(c)) == 0 {
			cleanCases++
		} else {
			positiveCases++
		}
	}
	if positiveCases == 0 || cleanCases == 0 {
		t.Fatalf("dataset must contain vulnerability and clean-code labels: positives=%d clean=%d", positiveCases, cleanCases)
	}

	harness := evaldata.Harness{
		Runner:        &scannerRunner{scanner: securityscan.New()},
		LineTolerance: evaldata.DefaultLineTolerance,
	}
	metrics, err := harness.RunMonthly(context.Background(), evaldata.Dataset{
		ID:    "heldout-security-scanner",
		Cases: securityDataset(cases),
	})
	if err != nil {
		t.Fatalf("run security scanner eval: %v", err)
	}

	precision := 1 - metrics.FalsePositiveRate
	t.Logf("deterministic security scanner: precision=%.3f recall=%.3f false-positive-rate=%.3f (tp=%d fp=%d fn=%d over %d cases)",
		precision, metrics.Recall, metrics.FalsePositiveRate,
		metrics.TruePositives, metrics.FalsePositives, metrics.FalseNegatives, metrics.TotalCases)

	if metrics.TruePositives == 0 {
		t.Fatal("deterministic scanner detected none of the labeled vulnerabilities")
	}
	if metrics.Recall < 0.25 {
		t.Fatalf("deterministic scanner recall %.3f fell below the regex-coverage floor 0.25", metrics.Recall)
	}
	if precision < 0.75 {
		t.Fatalf("deterministic scanner precision %.3f fell below floor 0.75 (fp=%d)", precision, metrics.FalsePositives)
	}
}

// scannerRunner adapts securityscan.Scanner to the eval.ReviewRunner interface
// so the harness scores real scanner findings. It materializes each patch's
// post-image on disk (the scanner reads files, diff-filtered to added lines)
// and returns the scanner's findings verbatim.
type scannerRunner struct {
	scanner *securityscan.Scanner
}

func (r *scannerRunner) ReviewCase(ctx context.Context, in evaldata.RunCaseInput) (reviewengine.ReviewerOutput, error) {
	dir, err := os.MkdirTemp("", "secscan-eval")
	if err != nil {
		return reviewengine.ReviewerOutput{}, err
	}
	defer os.RemoveAll(dir)

	files, err := writePatchPostImage(dir, in.PatchDiff)
	if err != nil {
		return reviewengine.ReviewerOutput{}, err
	}

	// Whole-file scan of the reconstructed post-image, matching the call
	// auditengine makes (ScanFiles(..., "")). We do not pass the diff: 29 of the
	// 31 corpus patches have malformed hunk headers that go-gitdiff — and thus
	// securityscan's own diff-aware line filter — reject (DRYDOCK-dfl3). An empty
	// diff cannot fail to parse, so the returned error is always nil here.
	scan, err := r.scanner.ScanFiles(ctx, dir, files, "")
	if err != nil {
		return reviewengine.ReviewerOutput{}, err
	}
	out := reviewengine.ReviewerOutput{Summary: "deterministic security scan"}
	for _, f := range scan.Findings {
		out.Findings = append(out.Findings, reviewengine.Finding{
			Severity:    f.Severity,
			Category:    f.Category,
			File:        f.File,
			Line:        f.Line,
			Evidence:    f.Evidence,
			Explanation: f.Description,
			Suggestion:  f.Suggestion,
			Confidence:  f.Confidence,
		})
	}
	return out, nil
}

// writePatchPostImage reconstructs the post-image of each file in a unified
// diff and writes it under dir, returning the changed-file paths. Added and
// context lines are placed at their hunk's post-image line number so scanner
// findings carry line numbers comparable to the corpus's expected labels.
//
// This walks the diff directly rather than via go-gitdiff because 29 of the 31
// corpus patches carry malformed hunk-header counts that go-gitdiff rejects
// (DRYDOCK-dfl3 — the same defect that let an unparseable diff read as a clean
// scan on the secret-reporting path). Only the @@ start offset and the
// +/-/space line prefixes are trusted — never the header counts — so a wrong
// count cannot break reconstruction. This workaround is deliberately confined
// to the test corpus; once DRYDOCK-dfl3 repairs the headers at source, replace
// it with gitdiff.Parse rather than carrying it forward.
func writePatchPostImage(dir, diff string) ([]string, error) {
	hunkHeader := regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)`)
	fileLines := map[string]map[int]string{}
	fileMax := map[string]int{}
	var order []string

	var current string
	lineNum := 0
	for _, raw := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(raw, "+++ "):
			name := strings.TrimPrefix(raw, "+++ ")
			name = strings.TrimPrefix(name, "b/")
			current = strings.TrimSpace(name)
			if _, ok := fileLines[current]; !ok && current != "" && current != "/dev/null" {
				fileLines[current] = map[int]string{}
				order = append(order, current)
			}
		case strings.HasPrefix(raw, "--- "):
			// old-file header; ignore.
		case strings.HasPrefix(raw, "@@"):
			if m := hunkHeader.FindStringSubmatch(raw); m != nil {
				lineNum, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(raw, "+"):
			if current != "" && current != "/dev/null" {
				fileLines[current][lineNum] = raw[1:]
				if lineNum > fileMax[current] {
					fileMax[current] = lineNum
				}
			}
			lineNum++
		case strings.HasPrefix(raw, "-"):
			// removed line: absent from the post-image.
		case strings.HasPrefix(raw, "diff "), strings.HasPrefix(raw, "\\"):
			// file separators / "\ No newline" markers.
		default:
			// context line (leading space or blank).
			if current != "" && current != "/dev/null" {
				text := raw
				if strings.HasPrefix(raw, " ") {
					text = raw[1:]
				}
				fileLines[current][lineNum] = text
				if lineNum > fileMax[current] {
					fileMax[current] = lineNum
				}
			}
			lineNum++
		}
	}

	changed := make([]string, 0, len(order))
	for _, name := range order {
		var buf strings.Builder
		for i := 1; i <= fileMax[name]; i++ {
			buf.WriteString(fileLines[name][i])
			buf.WriteByte('\n')
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, []byte(buf.String()), 0o644); err != nil {
			return nil, err
		}
		changed = append(changed, name)
	}
	return changed, nil
}

// labeledSecurityCases keeps the security-relevant slice of the corpus: cases
// that carry a security label plus the clean-code cases (no expected findings),
// which measure the scanner's false-positive behaviour.
func labeledSecurityCases(cases []evaldata.PatchCase) []evaldata.PatchCase {
	selected := make([]evaldata.PatchCase, 0, len(cases))
	for _, c := range cases {
		if len(c.ExpectedFindings) == 0 || len(securityExpected(c)) > 0 {
			selected = append(selected, c)
		}
	}
	return selected
}

// securityDataset drops non-security expected labels so the security-lens
// metrics are scored only against security findings (the scanner emits nothing
// else). Clean cases are preserved unchanged.
func securityDataset(cases []evaldata.PatchCase) []evaldata.PatchCase {
	out := make([]evaldata.PatchCase, 0, len(cases))
	for _, c := range cases {
		c.ExpectedFindings = securityExpected(c)
		out = append(out, c)
	}
	return out
}

func securityExpected(c evaldata.PatchCase) []evaldata.ExpectedFinding {
	var expected []evaldata.ExpectedFinding
	for _, finding := range c.ExpectedFindings {
		if strings.EqualFold(finding.Category, "security") {
			expected = append(expected, finding)
		}
	}
	return expected
}
