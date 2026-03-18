package eval

import (
	"context"

	"drydock/internal/reviewengine"
)

type EngineRunner struct {
	Engine *reviewengine.Engine
	FewShot []string
}

func (r EngineRunner) ReviewCase(ctx context.Context, in RunCaseInput) (reviewengine.ReviewerOutput, error) {
	out, err := r.Engine.Run(ctx, reviewengine.RunInput{
		ContextBundle: in.ContextBundle + "\n\n## patch\n" + in.PatchDiff,
		ChangedFiles:  in.ChangedFiles,
		FewShot:       r.FewShot,
	})
	if err != nil {
		return reviewengine.ReviewerOutput{}, err
	}
	return out.Review, nil
}

