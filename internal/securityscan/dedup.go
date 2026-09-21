package securityscan

import (
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

// MergeScannerFindings merges deterministic scanner findings into the LLM
// findings and returns one normalized set. Finding identity is decided by the shared
// reviewengine.SameFindingLocus predicate; the merge policy on top of it is
// this package's own:
//   - A non-sensitive scanner finding that shares an LLM finding's identity
//     keeps the LLM finding as the representative, upgrades its severity to the
//     scanner's when higher, applies a corroboration confidence boost, and
//     tags the evidence with the SAST rule ID.
//   - A sensitive scanner finding merges with every LLM finding overlapping its
//     expanded [Line-3, EndLine+3] span and replaces the LLM-provided text with
//     canonical scanner text.
//   - Unmatched scanner findings are converted to reviewengine.Finding and
//     prepended.
//
// The LLM finding stays the representative deliberately: scanner findings carry
// a flat Confidence 1.0 (DRYDOCK-uth5) even for hedged rules, so "highest
// confidence wins" would replace richer LLM explanations with terse rule text.
func MergeScannerFindings(scanFindings []SecurityFinding, llmFindings []reviewengine.Finding) ([]reviewengine.Finding, error) {
	if len(scanFindings) == 0 {
		// Still normalize: the returned set is published, and callers rely on
		// canonical priorities being populated regardless of which path produced it.
		return reviewengine.NormalizeFindings(llmFindings)
	}

	boosted := make(map[int]bool) // LLM findings already merged with a scanner finding
	var unmatched []SecurityFinding

	for _, sf := range scanFindings {
		matched := false
		if sf.Sensitive {
			endLine := sf.EndLine
			if endLine < sf.Line {
				endLine = sf.Line
			}
			for idx := range llmFindings {
				llmFinding := &llmFindings[idx]
				if llmFinding.File != sf.File || llmFinding.Line < sf.Line-3 || llmFinding.Line > endLine+3 {
					continue
				}
				mergeSensitiveFinding(llmFinding, sf)
				boosted[idx] = true
				matched = true
			}
			if !matched {
				unmatched = append(unmatched, sf)
			}
			continue
		}

		// Non-sensitive: fold the scanner's signal into the first LLM finding
		// that shares the finding's identity. The LLM finding stays the base.
		candidate := reviewengine.Finding{File: sf.File, Category: sf.Category, Line: sf.Line}
		for idx := range llmFindings {
			if boosted[idx] || !reviewengine.SameFindingLocus(candidate, llmFindings[idx]) {
				continue
			}
			// Corroboration boost: two independent methods agree.
			if llmFindings[idx].Confidence < 0.95 {
				llmFindings[idx].Confidence = min(llmFindings[idx].Confidence+0.15, 1.0)
			}
			// Upgrade severity if the scanner found a higher one.
			if reviewengine.IsAtOrAboveSeverity(sf.Severity, llmFindings[idx].Severity) {
				llmFindings[idx].Severity = sf.Severity
			}
			// Append scanner evidence as additional context.
			llmFindings[idx].Evidence += " [SAST: " + sf.RuleID + "]"
			boosted[idx] = true
			matched = true
			break
		}
		if !matched {
			unmatched = append(unmatched, sf)
		}
	}

	// Convert unmatched scanner findings to reviewengine.Finding.
	result := make([]reviewengine.Finding, 0, len(unmatched)+len(llmFindings))
	for _, sf := range unmatched {
		category := sf.Category
		if sf.Sensitive {
			category = "security"
		}
		result = append(result, reviewengine.Finding{
			Severity:    sf.Severity,
			Category:    category,
			File:        sf.File,
			Line:        sf.Line,
			Evidence:    sf.Evidence,
			Explanation: sf.Description + " [" + sf.RuleID + "]",
			Suggestion:  sf.Suggestion,
			Sensitive:   sf.Sensitive,
			Confidence:  sf.Confidence,
		})
	}
	result = append(result, llmFindings...)
	return reviewengine.NormalizeFindings(result)
}

func mergeSensitiveFinding(llmFinding *reviewengine.Finding, scannerFinding SecurityFinding) {
	if llmFinding.Confidence < 0.95 {
		llmFinding.Confidence = min(llmFinding.Confidence+0.15, 1.0)
	}
	if reviewengine.IsAtOrAboveSeverity(scannerFinding.Severity, llmFinding.Severity) {
		llmFinding.Severity = scannerFinding.Severity
	}
	llmFinding.Category = "security"
	llmFinding.Evidence = scannerFinding.Evidence
	llmFinding.Explanation = scannerFinding.Description + " [" + scannerFinding.RuleID + "]"
	llmFinding.Suggestion = scannerFinding.Suggestion
	llmFinding.SuggestedDiff = ""
	llmFinding.SuggestedCode = ""
	llmFinding.Sensitive = true
}
