package securityscan

import (
	"drydock/internal/reviewengine"
)

// DeduplicateFindings merges scanner findings with LLM findings. Non-sensitive
// findings merge with one same-category finding on a nearby line. Sensitive
// findings merge with every finding overlapping their expanded line span and
// replace LLM-provided text with canonical scanner text. Unmatched scanner
// findings are converted to reviewengine.Finding and prepended.
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
	if normalized, err := reviewengine.NormalizeFindings(result); err == nil {
		return normalized
	}
	return result
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

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
