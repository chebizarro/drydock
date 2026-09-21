package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluekeyes/go-gitdiff/gitdiff"

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

	// Diff-aware scan of the reconstructed post-image: passing the (now-valid,
	// DRYDOCK-dfl3) patch restricts findings to the lines the patch adds — the
	// same line filter the production secret/scan path applies — instead of a
	// whole-file scan. An unparseable diff would surface as an error here rather
	// than a silent empty added-line map.
	scan, err := r.scanner.ScanFiles(ctx, dir, files, in.PatchDiff)
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
// diff via go-gitdiff — the pipeline's authoritative patch parser — and writes
// it under dir, returning the changed-file paths. Added and context lines are
// placed at their post-image line number so scanner findings carry line numbers
// comparable to the corpus's expected labels. The corpus is valid unified diff
// (DRYDOCK-dfl3 repaired the hunk-header counts), so no tolerant walk is needed.
func writePatchPostImage(dir, diff string) ([]string, error) {
	files, _, err := gitdiff.Parse(strings.NewReader(diff))
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, file := range files {
		if file.NewName == "" || file.IsDelete {
			continue
		}
		lineAt := map[int]string{}
		maxLine := 0
		for _, fragment := range file.TextFragments {
			lineNum := int(fragment.NewPosition)
			for _, line := range fragment.Lines {
				switch line.Op {
				case gitdiff.OpAdd, gitdiff.OpContext:
					lineAt[lineNum] = strings.TrimSuffix(line.Line, "\n")
					if lineNum > maxLine {
						maxLine = lineNum
					}
					lineNum++
				case gitdiff.OpDelete:
					// Removed line: absent from the post-image.
				}
			}
		}
		var buf strings.Builder
		for i := 1; i <= maxLine; i++ {
			buf.WriteString(lineAt[i])
			buf.WriteByte('\n')
		}
		target := filepath.Join(dir, filepath.FromSlash(file.NewName))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, []byte(buf.String()), 0o644); err != nil {
			return nil, err
		}
		changed = append(changed, file.NewName)
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
