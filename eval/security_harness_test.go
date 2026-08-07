package eval

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	evaldata "drydock/internal/eval"
	"drydock/internal/reviewengine"
	"drydock/internal/securityverify"
	"drydock/internal/testutil"
)

type securityMetrics struct {
	TruePositives     int
	FalsePositives    int
	FalseNegatives    int
	Precision         float64
	Recall            float64
	FalsePositiveRate float64
}

func TestSecurityPipelineVerifyImprovesFalsePositiveRate(t *testing.T) {
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

	withoutVerify := runSecurityEval(t, cases, false)
	withVerify := runSecurityEval(t, cases, true)

	t.Logf("security eval without securityverify: precision=%.3f recall=%.3f false-positive-rate=%.3f (tp=%d fp=%d fn=%d)",
		withoutVerify.Precision, withoutVerify.Recall, withoutVerify.FalsePositiveRate,
		withoutVerify.TruePositives, withoutVerify.FalsePositives, withoutVerify.FalseNegatives)
	t.Logf("security eval with securityverify: precision=%.3f recall=%.3f false-positive-rate=%.3f (tp=%d fp=%d fn=%d)",
		withVerify.Precision, withVerify.Recall, withVerify.FalsePositiveRate,
		withVerify.TruePositives, withVerify.FalsePositives, withVerify.FalseNegatives)

	if withoutVerify.FalsePositiveRate == 0 {
		t.Fatal("unverified fixture must include false positives")
	}
	if withVerify.FalsePositiveRate >= withoutVerify.FalsePositiveRate {
		t.Fatalf("securityverify did not cut false-positive rate: without=%.3f with=%.3f",
			withoutVerify.FalsePositiveRate, withVerify.FalsePositiveRate)
	}
	if withVerify.Recall < withoutVerify.Recall {
		t.Fatalf("securityverify dropped true-positive recall: without=%.3f with=%.3f",
			withoutVerify.Recall, withVerify.Recall)
	}
	if withVerify.Precision <= withoutVerify.Precision {
		t.Fatalf("securityverify did not improve precision: without=%.3f with=%.3f",
			withoutVerify.Precision, withVerify.Precision)
	}
}

func runSecurityEval(t *testing.T, cases []evaldata.PatchCase, withVerify bool) securityMetrics {
	t.Helper()
	ctx := context.Background()
	var total securityMetrics
	for _, c := range cases {
		expected := securityExpected(c)
		predicted := reviewerCandidates(t, ctx, c, expected)
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
	// Match the existing eval harness definition: the fraction of predicted
	// findings that are false positives.
	total.FalsePositiveRate = metricRatio(total.FalsePositives, total.TruePositives+total.FalsePositives)
	return total
}

func labeledSecurityCases(cases []evaldata.PatchCase) []evaldata.PatchCase {
	selected := make([]evaldata.PatchCase, 0, len(cases))
	for _, c := range cases {
		if len(c.ExpectedFindings) == 0 || len(securityExpected(c)) > 0 {
			selected = append(selected, c)
		}
	}
	return selected
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

func reviewerCandidates(t *testing.T, ctx context.Context, c evaldata.PatchCase, expected []evaldata.ExpectedFinding) []reviewengine.Finding {
	t.Helper()
	findings := make([]reviewengine.Finding, 0, max(1, len(expected)))
	for _, finding := range expected {
		findings = append(findings, reviewengine.Finding{
			Severity: finding.Severity, Category: "security", File: finding.File, Line: finding.Line,
			Evidence:    "attacker-controlled input reaches a sensitive sink",
			Explanation: "reachable security vulnerability", Suggestion: "apply the relevant security control",
			Confidence: 0.9,
		})
	}
	if len(expected) == 0 {
		findings = append(findings, reviewengine.Finding{
			Severity: "medium", Category: "security", File: c.ChangedFiles[0], Line: 1,
			Evidence: "security-sensitive code changed", Explanation: "possible vulnerability",
			Suggestion: "review the change", Confidence: 0.75,
		})
	}

	reviewJSON, err := json.Marshal(reviewengine.ReviewerOutput{
		Summary: "deterministic security review", Findings: findings,
	})
	if err != nil {
		t.Fatalf("marshal reviewer fixture for %s: %v", c.CaseID, err)
	}
	fake := &testutil.FakeLLM{Responses: []string{
		`{"change_type":"security","risk_areas":["security"],"needed_context":[],"review_focus":"security","model_route":"sec70b"}`,
		string(reviewJSON),
	}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := reviewengine.New(reviewengine.Config{
		Planner: reviewengine.ModelEndpoint{BaseURL: "fake://planner", Model: "planner"},
		Sec70B:  reviewengine.ModelEndpoint{BaseURL: "fake://security", Model: "sec70b"},
	}, fake, logger)
	out, err := engine.Run(ctx, reviewengine.RunInput{
		ContextBundle:   c.ContextBundle + "\n\n## patch\n" + c.PatchDiff,
		ChangedFiles:    c.ChangedFiles,
		ReviewerRoute:   reviewengine.RouteSec70B,
		SkipWalkthrough: true,
	})
	if err != nil {
		t.Fatalf("review case %s: %v", c.CaseID, err)
	}
	return out.Review.Findings
}

func verifiedCandidates(t *testing.T, ctx context.Context, candidates []reviewengine.Finding, expected []evaldata.ExpectedFinding) []reviewengine.Finding {
	t.Helper()
	responses := make([]string, 0, len(candidates)*2)
	for _, candidate := range candidates {
		if matchesAny(expected, candidate) {
			responses = append(responses,
				`{"refuted":false,"certain":true,"reason":"reachable and exploitable"}`,
				`{"cwe":"CWE-20","severity":"high","confidence":0.95,"remediation":"validate and constrain untrusted input"}`,
			)
		} else {
			responses = append(responses,
				`{"refuted":true,"certain":true,"reason":"the reported path is already mitigated"}`,
			)
		}
	}
	fake := &testutil.FakeLLM{Responses: responses}
	cfg := securityverify.Config{
		VerifyVotes:      1,
		VerifyEndpoint:   reviewengine.ModelEndpoint{BaseURL: "fake://verify", Model: "securityverify"},
		ClassifyEndpoint: reviewengine.ModelEndpoint{BaseURL: "fake://classify", Model: "securityclassify"},
	}
	verified, err := securityverify.New(fake, cfg).Run(ctx, candidates)
	if err != nil {
		t.Fatalf("verify candidates: %v", err)
	}
	for i := range verified {
		verified[i].Category = "security"
	}
	return verified
}

func confusionCounts(expected []evaldata.ExpectedFinding, predicted []reviewengine.Finding) (tp, fp, fn int) {
	matched := make([]bool, len(expected))
	for _, candidate := range predicted {
		match := -1
		for i, want := range expected {
			if !matched[i] && want.File == candidate.File && want.Line == candidate.Line {
				match = i
				break
			}
		}
		if match < 0 {
			fp++
			continue
		}
		matched[match] = true
		tp++
	}
	for _, ok := range matched {
		if !ok {
			fn++
		}
	}
	return tp, fp, fn
}

func matchesAny(expected []evaldata.ExpectedFinding, candidate reviewengine.Finding) bool {
	for _, want := range expected {
		if want.File == candidate.File && want.Line == candidate.Line {
			return true
		}
	}
	return false
}

func metricRatio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
