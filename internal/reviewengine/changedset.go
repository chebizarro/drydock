package reviewengine

import (
	"fmt"
	"log/slog"
	"strings"

	"drydock/internal/targetidentity"
)

// changedSet is a normalized membership set of deterministically parsed
// changed-file paths.
type changedSet map[string]struct{}

func newChangedSet(files []string) changedSet {
	if len(files) == 0 {
		return nil
	}
	set := make(changedSet, len(files))
	for _, f := range files {
		if p := normalizeReviewPath(f); p != "" {
			set[p] = struct{}{}
		}
	}
	return set
}

func (s changedSet) contains(path string) bool {
	if s == nil {
		return false
	}
	_, ok := s[normalizeReviewPath(path)]
	return ok
}

func normalizeReviewPath(p string) string {
	return strings.TrimPrefix(strings.TrimSpace(p), "./")
}

// filterFindingsToChangedFiles drops findings anchored to files outside the
// deterministic changed-file set. LLM reviewers see contextual layers
// (project docs, related code) alongside the diff and can hallucinate
// findings against files the change never touched; only the parsed diff is
// authoritative for what changed. Findings without a file path are kept.
// No-op when the changed set is empty (callers without a deterministic set,
// e.g. eval harnesses).
func filterFindingsToChangedFiles(findings []Finding, changed []string, envelope targetidentity.Envelope, patchDiff, contextBundle string, logger *slog.Logger, label string) ([]Finding, error) {
	if envelope != (targetidentity.Envelope{}) {
		if err := envelope.VerifyMaterials(patchDiff, contextBundle); err != nil {
			return nil, fmt.Errorf("refuse changed-file output for mismatched target envelope: %w", err)
		}
	}
	set := newChangedSet(changed)
	if set == nil {
		return findings, nil
	}
	kept := make([]Finding, 0, len(findings))
	var dropped []string
	for _, f := range findings {
		if strings.TrimSpace(f.File) == "" || set.contains(f.File) {
			kept = append(kept, f)
			continue
		}
		dropped = append(dropped, f.File)
	}
	if len(dropped) > 0 && logger != nil {
		logger.Warn("dropped findings referencing unchanged files",
			"label", label, "dropped", dropped, "changed_files", changed)
	}
	return kept, nil
}

// filterWalkthroughToChangedFiles drops walkthrough file summaries whose
// paths are not in the deterministic changed-file set, preventing contextual
// documentation from being presented as modified files.
// FilterOutputToChangedFiles applies the final target-integrity boundary after
// all LLM, security-review, and scanner findings have been merged.
func FilterOutputToChangedFiles(review ReviewerOutput, walkthrough WalkthroughOutput, changed []string, envelope targetidentity.Envelope, patchDiff, contextBundle string, logger *slog.Logger) (ReviewerOutput, WalkthroughOutput, error) {
	findings, err := filterFindingsToChangedFiles(review.Findings, changed, envelope, patchDiff, contextBundle, logger, "final")
	if err != nil {
		return ReviewerOutput{}, WalkthroughOutput{}, err
	}
	review.Findings = findings
	return review, filterWalkthroughToChangedFiles(walkthrough, changed, logger), nil
}

func filterWalkthroughToChangedFiles(w WalkthroughOutput, changed []string, logger *slog.Logger) WalkthroughOutput {
	set := newChangedSet(changed)
	if set == nil {
		return w
	}
	kept := make([]FileSummary, 0, len(w.FileSummaries))
	var dropped []string
	for _, fs := range w.FileSummaries {
		if set.contains(fs.File) {
			kept = append(kept, fs)
			continue
		}
		dropped = append(dropped, fs.File)
	}
	if len(dropped) > 0 && logger != nil {
		logger.Warn("dropped walkthrough summaries referencing unchanged files",
			"dropped", dropped, "changed_files", changed)
	}
	w.FileSummaries = kept
	return w
}
