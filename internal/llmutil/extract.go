package llmutil

import "strings"

// ExtractJSON extracts the first JSON object found in raw text.
// It strips surrounding markdown code fences when present.
func ExtractJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}

	raw = stripCodeFence(raw)

	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start >= 0 && end > start {
		return raw[start : end+1]
	}
	return raw
}

func stripCodeFence(raw string) string {
	lines := strings.Split(raw, "\n")
	if len(lines) < 2 {
		return raw
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[0]), "```") {
		return raw
	}

	end := -1
	for i := len(lines) - 1; i > 0; i-- {
		if strings.TrimSpace(lines[i]) == "```" {
			end = i
			break
		}
	}
	if end <= 0 {
		return raw
	}
	return strings.TrimSpace(strings.Join(lines[1:end], "\n"))
}
