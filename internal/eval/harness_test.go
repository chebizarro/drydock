package eval

import (
	"context"
	"path/filepath"
	"testing"

	"drydock/internal/db"
	"drydock/internal/reviewengine"
)

type fakeRunner struct {
	out reviewengine.ReviewerOutput
}

func (f fakeRunner) ReviewCase(context.Context, RunCaseInput) (reviewengine.ReviewerOutput, error) {
	return f.out, nil
}

func TestHarnessComputesMetrics(t *testing.T) {
	ds := Dataset{
		ID: "d1",
		Cases: []PatchCase{
			{
				CaseID: "c1",
				ExpectedFindings: []ExpectedFinding{
					{Category: "security", File: "a.go", Line: 10},
					{Category: "correctness", File: "b.go", Line: 20},
				},
			},
		},
	}
	runner := fakeRunner{
		out: reviewengine.ReviewerOutput{
			Summary: "ok",
			Findings: []reviewengine.Finding{
				{Category: "security", File: "a.go", Line: 10, Severity: "high", Confidence: 0.9},
				{Category: "style", File: "x.go", Line: 1, Severity: "low", Confidence: 0.8},
			},
		},
	}
	h := Harness{Runner: runner}
	m, err := h.RunMonthly(context.Background(), ds)
	if err != nil {
		t.Fatalf("run monthly: %v", err)
	}
	if m.TruePositives != 1 || m.FalsePositives != 1 || m.FalseNegatives != 1 {
		t.Fatalf("unexpected confusion counts: %+v", m)
	}
	if m.Recall != 0.5 {
		t.Fatalf("expected recall 0.5, got %f", m.Recall)
	}
	if m.FalsePositiveRate != 0.5 {
		t.Fatalf("expected false positive rate 0.5, got %f", m.FalsePositiveRate)
	}
	if m.HighConfidencePrecision != 0.5 {
		t.Fatalf("expected high confidence precision 0.5, got %f", m.HighConfidencePrecision)
	}
}

func TestHarnessPersistsEvalRun(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	ds := Dataset{ID: "d2", Cases: []PatchCase{{CaseID: "c1"}}}
	h := Harness{
		Runner: fakeRunner{out: reviewengine.ReviewerOutput{Summary: "ok"}},
		Store:  store,
	}
	if _, err := h.RunMonthly(ctx, ds); err != nil {
		t.Fatalf("run monthly: %v", err)
	}
	count, err := store.CountEvalRuns(ctx)
	if err != nil {
		t.Fatalf("count eval runs: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 eval run row, got %d", count)
	}
}

func mustStore(t *testing.T, ctx context.Context) *db.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "eval.db")
	store, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}


