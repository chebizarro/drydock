package securityscan

import (
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

// corroborationConfidence is the floor a scanner finding must reach before it
// is allowed to boost or upgrade a colliding LLM finding. Hedged heuristic
// rules (DRYDOCK-uth5) fall below it, so a guess cannot raise an LLM finding's
// severity.
const corroborationConfidence = 0.8

// ReviewFinding converts a deterministic scanner finding into the review-engine
// shape, carrying rule identity as RuleID/CWE fields (DRYDOCK-vrbe) rather than
// encoding it in Evidence prose and parsing it back out later.
func (f SecurityFinding) ReviewFinding() reviewengine.Finding {
	return reviewengine.Finding{
		Severity:    f.Severity,
		Category:    "security",
		File:        f.File,
		Line:        f.Line,
		Evidence:    f.Evidence,
		Explanation: f.Description,
		Suggestion:  f.Suggestion,
		Sensitive:   f.Sensitive,
		Confidence:  f.Confidence,
		RuleID:      f.RuleID,
		CWE:         RuleCWE(f.RuleID),
	}
}

// ReviewFindings converts a batch of deterministic scanner findings. It is the
// single conversion shared by the PR and audit paths (DRYDOCK-ams0).
func ReviewFindings(findings []SecurityFinding) []reviewengine.Finding {
	out := make([]reviewengine.Finding, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.ReviewFinding())
	}
	return out
}

// scannerCorroborates reports whether a scanner finding is confident enough to
// reinforce a colliding LLM finding. A zero confidence predates the Confidence
// field and is treated as a literal (full-confidence) match.
func scannerCorroborates(sf SecurityFinding) bool {
	return sf.Confidence == 0 || sf.Confidence >= corroborationConfidence
}

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
// The LLM finding stays the representative deliberately, so "highest confidence
// wins" would not replace richer LLM explanations with terse rule text. A
// scanner finding only boosts or upgrades a colliding LLM finding when it is
// itself high-confidence (scannerCorroborates); hedged heuristic rules
// (DRYDOCK-uth5) still tag their evidence but cannot raise severity.
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
			corroborates := scannerCorroborates(sf)
			for idx := range llmFindings {
				llmFinding := &llmFindings[idx]
				if llmFinding.File != sf.File || llmFinding.Line < sf.Line-3 || llmFinding.Line > endLine+3 {
					continue
				}
				mergeSensitiveFinding(llmFinding, sf, corroborates)
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
			if scannerCorroborates(sf) {
				// Corroboration boost: two independent, confident methods agree.
				if llmFindings[idx].Confidence < 0.95 {
					llmFindings[idx].Confidence = min(llmFindings[idx].Confidence+0.15, 1.0)
				}
				// Upgrade severity if the scanner found a higher one.
				if reviewengine.IsAtOrAboveSeverity(sf.Severity, llmFindings[idx].Severity) {
					llmFindings[idx].Severity = sf.Severity
				}
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

// mergeSensitiveFinding folds a sensitive scanner finding into an overlapping
// LLM finding. The confidence boost and severity upgrade are gated on
// corroborates (scannerCorroborates) for parity with the non-sensitive branch:
// a hedged heuristic secret rule (confidence in (0, 0.8)) still replaces the
// LLM-provided text with canonical scanner text but cannot raise severity or
// confidence. Legacy zero-confidence and literal (>=0.8) findings still upgrade.
func mergeSensitiveFinding(llmFinding *reviewengine.Finding, scannerFinding SecurityFinding, corroborates bool) {
	if corroborates {
		if llmFinding.Confidence < 0.95 {
			llmFinding.Confidence = min(llmFinding.Confidence+0.15, 1.0)
		}
		if reviewengine.IsAtOrAboveSeverity(scannerFinding.Severity, llmFinding.Severity) {
			llmFinding.Severity = scannerFinding.Severity
		}
	}
	llmFinding.Category = "security"
	llmFinding.Evidence = scannerFinding.Evidence
	llmFinding.Explanation = scannerFinding.Description + " [" + scannerFinding.RuleID + "]"
	llmFinding.Suggestion = scannerFinding.Suggestion
	llmFinding.SuggestedDiff = ""
	llmFinding.SuggestedCode = ""
	llmFinding.Sensitive = true
}
