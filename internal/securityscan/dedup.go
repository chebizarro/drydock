package securityscan

import (
	"drydock/internal/reviewengine"
)

// DeduplicateFindings merges scanner findings with LLM findings. When a scanner
// finding matches an LLM finding (same file, nearby line, same category), the
// LLM finding's confidence is boosted and the scanner finding is dropped.
// Unmatched scanner findings are converted to reviewengine.Finding and prepended.
func DeduplicateFindings(scanFindings []SecurityFinding, llmFindings []reviewengine.Finding) []reviewengine.Finding {
	if len(scanFindings) == 0 {
		return llmFindings
	}

	// Build a lookup index for LLM findings by file.
	type fileLineKey struct {
		file string
		line int
	}
	llmIdx := make(map[fileLineKey]int, len(llmFindings))
	for i, f := range llmFindings {
		llmIdx[fileLineKey{f.File, f.Line}] = i
	}

	boosted := make(map[int]bool) // indices of LLM findings that were boosted
	var unmatched []SecurityFinding

	for _, sf := range scanFindings {
		matched := false
		// Check exact match first, then nearby lines (±3).
		// Only merge when the LLM finding is also in the "security" category.
		for delta := 0; delta <= 3; delta++ {
			for _, d := range []int{delta, -delta} {
				key := fileLineKey{sf.File, sf.Line + d}
				if idx, ok := llmIdx[key]; ok && !boosted[idx] && llmFindings[idx].Category == sf.Category {
					// Boost the LLM finding's confidence.
					if llmFindings[idx].Confidence < 0.95 {
						llmFindings[idx].Confidence = min(llmFindings[idx].Confidence+0.15, 1.0)
					}
					// Upgrade severity if scanner found a higher severity.
					if reviewengine.IsAtOrAboveSeverity(sf.Severity, llmFindings[idx].Severity) {
						llmFindings[idx].Severity = sf.Severity
					}
					// Append scanner evidence as additional context.
					llmFindings[idx].Evidence += " [SAST: " + sf.RuleID + "]"
					boosted[idx] = true
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			unmatched = append(unmatched, sf)
		}
	}

	// Convert unmatched scanner findings to reviewengine.Finding.
	result := make([]reviewengine.Finding, 0, len(unmatched)+len(llmFindings))
	for _, sf := range unmatched {
		result = append(result, reviewengine.Finding{
			Severity:    sf.Severity,
			Category:    sf.Category,
			File:        sf.File,
			Line:        sf.Line,
			Evidence:    sf.Evidence,
			Explanation: sf.Description + " [" + sf.RuleID + "]",
			Suggestion:  sf.Suggestion,
			Confidence:  sf.Confidence,
		})
	}
	result = append(result, llmFindings...)
	return result
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
