package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

const DefaultLineTolerance = 2

type Harness struct {
	Runner        ReviewRunner
	Store         *db.Store
	Logger        *slog.Logger
	LineTolerance int
}

func (h *Harness) RunMonthly(ctx context.Context, ds Dataset) (Metrics, error) {
	if h.Runner == nil {
		return Metrics{}, fmt.Errorf("runner is required")
	}

	totalExpected := 0
	totalPredicted := 0
	tp := 0
	fp := 0
	fn := 0
	severityMatches := 0
	severityMismatches := 0
	lineTolerance := h.LineTolerance

	var calibrationAbsSum float64
	calibrationN := 0
	highConfN := 0
	highConfTP := 0

	for _, c := range ds.Cases {
		out, err := h.Runner.ReviewCase(ctx, RunCaseInput{
			PatchDiff:     c.PatchDiff,
			ChangedFiles:  c.ChangedFiles,
			ContextBundle: c.ContextBundle,
		})
		if err != nil {
			return Metrics{}, fmt.Errorf("review case %s: %w", c.CaseID, err)
		}

		s := ScoreCase(c.ExpectedFindings, out.Findings, lineTolerance)
		totalExpected += s.Expected
		totalPredicted += s.Predicted
		tp += s.TruePositives
		fp += s.FalsePositives
		fn += s.FalseNegatives
		severityMatches += s.SeverityMatches
		severityMismatches += s.SeverityMismatches
		calibrationAbsSum += s.CalibrationAbsSum
		calibrationN += s.Predicted
		highConfN += s.HighConfidenceN
		highConfTP += s.HighConfidenceTP
	}

	recall := ratio(tp, tp+fn)
	fpRate := ratio(fp, tp+fp)
	calibration := ratioFloat(calibrationAbsSum, float64(calibrationN))
	highConfPrecision := ratio(highConfTP, highConfN)
	severityAgreement := ratio(severityMatches, severityMatches+severityMismatches)

	metrics := Metrics{
		TotalCases:              len(ds.Cases),
		ExpectedFindings:        totalExpected,
		PredictedFindings:       totalPredicted,
		TruePositives:           tp,
		FalsePositives:          fp,
		FalseNegatives:          fn,
		Recall:                  recall,
		FalsePositiveRate:       fpRate,
		CalibrationMAE:          calibration,
		HighConfidencePrecision: highConfPrecision,
		SeverityMatches:         severityMatches,
		SeverityMismatches:      severityMismatches,
		SeverityAgreement:       severityAgreement,
	}

	if h.Store != nil {
		details, _ := json.Marshal(metrics)
		if err := h.Store.InsertEvalRun(
			ctx,
			ds.ID,
			metrics.TotalCases,
			metrics.ExpectedFindings,
			metrics.PredictedFindings,
			metrics.TruePositives,
			metrics.FalsePositives,
			metrics.FalseNegatives,
			metrics.Recall,
			metrics.FalsePositiveRate,
			metrics.CalibrationMAE,
			metrics.HighConfidencePrecision,
			string(details),
		); err != nil {
			return Metrics{}, err
		}
	}
	if h.Logger != nil {
		h.Logger.Info("monthly eval completed",
			"dataset_id", ds.ID,
			"cases", metrics.TotalCases,
			"recall", metrics.Recall,
			"false_positive_rate", metrics.FalsePositiveRate,
			"calibration_mae", metrics.CalibrationMAE,
			"severity_agreement", metrics.SeverityAgreement,
		)
	}

	return metrics, nil
}

// CaseScore is the matching outcome for one case. Fields are additive so
// callers can aggregate cases however they need (RunMonthly sums them).
type CaseScore struct {
	Expected           int     `json:"expected"`
	Predicted          int     `json:"predicted"`
	TruePositives      int     `json:"true_positives"`
	FalsePositives     int     `json:"false_positives"`
	FalseNegatives     int     `json:"false_negatives"`
	SeverityMatches    int     `json:"severity_matches"`
	SeverityMismatches int     `json:"severity_mismatches"`
	CalibrationAbsSum  float64 `json:"calibration_abs_sum"`
	HighConfidenceN    int     `json:"high_conf_n"`
	HighConfidenceTP   int     `json:"high_conf_tp"`
	// MatchedBy[i] is the index of the prediction matched to expected[i], or -1.
	MatchedBy []int `json:"matched_by"`
}

// ScoreCase matches predictions to expected findings one-to-one on category,
// normalized file, and line within lineTolerance.
func ScoreCase(expected []ExpectedFinding, findings []reviewengine.Finding, lineTolerance int) CaseScore {
	lineTolerance = max(lineTolerance, 0)
	s := CaseScore{Expected: len(expected), Predicted: len(findings), MatchedBy: make([]int, len(expected))}
	matched := make([]bool, len(expected))
	for i := range s.MatchedBy {
		s.MatchedBy[i] = -1
	}
	for pi, pred := range findings {
		label := 0.0
		if mi := findExpectedMatch(expected, matched, pred, lineTolerance); mi >= 0 {
			s.TruePositives++
			label = 1
			matched[mi] = true
			s.MatchedBy[mi] = pi
			if normalizeLabel(expected[mi].Severity) == normalizeLabel(pred.Severity) {
				s.SeverityMatches++
			} else {
				s.SeverityMismatches++
			}
		} else {
			s.FalsePositives++
		}
		s.CalibrationAbsSum += math.Abs(pred.Confidence - label)
		if pred.Confidence >= 0.8 {
			s.HighConfidenceN++
			if label == 1 {
				s.HighConfidenceTP++
			}
		}
	}
	for _, m := range matched {
		if !m {
			s.FalseNegatives++
		}
	}
	return s
}

func findExpectedMatch(expected []ExpectedFinding, matched []bool, pred reviewengine.Finding, lineTolerance int) int {
	bestIndex := -1
	bestDistance := lineTolerance + 1
	for i, exp := range expected {
		if matched[i] || normalizeLabel(exp.Category) != normalizeLabel(pred.Category) || normalizePath(exp.File) != normalizePath(pred.File) {
			continue
		}
		distance := abs(exp.Line - pred.Line)
		if distance <= lineTolerance && distance < bestDistance {
			bestIndex = i
			bestDistance = distance
		}
	}
	return bestIndex
}

func normalizeLabel(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func normalizePath(p string) string {
	return strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func ratioFloat(n, d float64) float64 {
	if d == 0 {
		return 0
	}
	return n / d
}
