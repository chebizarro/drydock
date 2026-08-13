package reviewengine

import (
	"context"
	"errors"
	"testing"

	"drydock/internal/targetidentity"
)

func TestRunWithExecutorRequiresValidatedSnapshotScope(t *testing.T) {
	const (
		patch  = "diff --git a/main.go b/main.go"
		bundle = "finalized snapshot context"
	)
	input := RunInput{
		ContextBundle: bundle, PatchDiff: patch, ChangedFiles: []string{"main.go"},
		TargetEnvelope: targetidentity.New("repo", "root", "patch", "remote", "", "", "", patch, bundle),
		FindingScope:   FindingScopeSnapshot, SkipWalkthrough: true,
		SnapshotFindingValidator: func(_ context.Context, finding Finding) error {
			if finding.File != "outside.go" || finding.Line != 7 {
				return errors.New("outside frozen snapshot")
			}
			return nil
		},
	}
	executor := reviewerExecutorFunc(func(context.Context, ReviewerExecutionRequest) (ReviewerExecutionResult, error) {
		return ReviewerExecutionResult{
			Review: ReviewerOutput{Summary: "audit", Findings: []Finding{{
				Priority: PriorityP1, Category: "security", File: "outside.go", Line: 7, Confidence: 0.9,
			}}},
			ValidatedScope: FindingScopeSnapshot,
		}, nil
	})
	output, err := newExecutorTestEngine(&plannerWalkthroughClient{}).RunWithExecutor(context.Background(), input, executor)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Review.Findings) != 1 || output.Review.Findings[0].File != "outside.go" {
		t.Fatalf("snapshot-scoped finding was filtered: %+v", output.Review.Findings)
	}

	executor = reviewerExecutorFunc(func(context.Context, ReviewerExecutionRequest) (ReviewerExecutionResult, error) {
		return ReviewerExecutionResult{
			Review: ReviewerOutput{Summary: "audit"}, ValidatedScope: FindingScopePatch,
		}, nil
	})
	if _, err := newExecutorTestEngine(&plannerWalkthroughClient{}).RunWithExecutor(context.Background(), input, executor); err == nil {
		t.Fatal("scope mismatch was accepted")
	}
}
