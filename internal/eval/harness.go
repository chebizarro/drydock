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

type Harness struct {
	Runner ReviewRunner
	Store  *db.Store
	Logger *slog.Logger
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

		expectedMap := make(map[string]bool, len(c.ExpectedFindings))
		matchedExpected := make(map[string]bool, len(c.ExpectedFindings))
		for _, exp := range c.ExpectedFindings {
			expectedMap[expectedKey(exp)] = true
			totalExpected++
		}

		for _, pred := range out.Findings {
			totalPredicted++
			key := predictedKey(pred)
			label := 0.0
			if expectedMap[key] && !matchedExpected[key] {
				tp++
				label = 1
				matchedExpected[key] = true
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

		for key := range expectedMap {
			if !matchedExpected[key] {
				fn++
			}
		}
	}

	recall := ratio(tp, tp+fn)
	fpRate := ratio(fp, tp+fp)
	calibration := ratioFloat(calibrationAbsSum, float64(calibrationN))
	highConfPrecision := ratio(highConfTP, highConfN)

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
		)
	}

	return metrics, nil
}

func expectedKey(f ExpectedFinding) string {
	return strings.ToLower(strings.TrimSpace(f.Category)) + "|" + normalizePath(f.File) + "|" + fmt.Sprint(f.Line)
}

func predictedKey(f reviewengine.Finding) string {
	return strings.ToLower(strings.TrimSpace(f.Category)) + "|" + normalizePath(f.File) + "|" + fmt.Sprint(f.Line)
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

