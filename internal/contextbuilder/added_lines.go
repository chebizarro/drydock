package contextbuilder

import (
	"context"
	"fmt"
	"strings"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

// AddedLinesTokenCap bounds the optional line index independently of the bundle budget.
const AddedLinesTokenCap = 2048

type addedLinesProvider struct{}

func (addedLinesProvider) LayerName() string { return LayerAddedLines }

// Priority 2 sorts added-lines before modified-files, immediately after the patch.
func (addedLinesProvider) Priority() int { return 2 }

func (addedLinesProvider) Build(_ context.Context, in BuildInput) (string, error) {
	if !in.IncludeAddedLines {
		return "", nil
	}
	files, err := parsePatch(in.PatchEventContent)
	if err != nil {
		return "", &LayerWarning{Err: fmt.Errorf("parse patch: %w", err)}
	}
	var out strings.Builder
	for _, file := range files {
		if file.IsBinary || file.NewName == "" || isExcludedPath(file.NewName) {
			continue
		}
		for _, fragment := range file.TextFragments {
			if fragment == nil {
				continue
			}
			lineNumber := fragment.NewPosition
			for _, line := range fragment.Lines {
				switch line.Op {
				case gitdiff.OpAdd:
					fmt.Fprintf(&out, "%s:%d: %s\n", file.NewName, lineNumber, strings.TrimSuffix(line.Line, "\n"))
					lineNumber++
				case gitdiff.OpContext:
					lineNumber++
				}
			}
		}
	}
	return out.String(), nil
}
