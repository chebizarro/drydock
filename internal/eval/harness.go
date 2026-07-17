package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"drydock/internal/db"
	"drydock/internal/reviewengine"
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
	lineTolerance := max(h.LineTolerance, 0)

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

		matchedExpected := make([]bool, len(c.ExpectedFindings))
		totalExpected += len(c.ExpectedFindings)

		for _, pred := range out.Findings {
			totalPredicted++
			label := 0.0
			matchIndex := findExpectedMatch(c.ExpectedFindings, matchedExpected, pred, lineTolerance)
			if matchIndex >= 0 {
				tp++
				label = 1
				matchedExpected[matchIndex] = true
				if normalizeLabel(c.ExpectedFindings[matchIndex].Severity) == normalizeLabel(pred.Severity) {
					severityMatches++
				} else {
					severityMismatches++
				}
			} else {
				fp++
			}

			calibrationAbsSum += math.Abs(pred.Confidence - label)
			calibrationN++
			if pred.Confidence >= 0.8 {
				highConfN++
				if label == 1 {
					highConfTP++
				}
			}
		}

		for _, matched := range matchedExpected {
			if !matched {
				fn++
			}
		}
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
