package reviewengine

import "strings"

func BuildChecklist(changedFiles []string) []string {
	unique := map[string]bool{}
	add := func(item string) {
		if strings.TrimSpace(item) != "" {
			unique[item] = true
		}
	}

	for _, file := range changedFiles {
		lower := strings.ToLower(file)
		switch {
		case strings.HasSuffix(lower, ".sql") || strings.Contains(lower, "query") || strings.Contains(lower, "orm"):
			add("SQL injection and unsafe query construction checks")
		}
		if strings.Contains(lower, "auth") || strings.Contains(lower, "session") || strings.Contains(lower, "permission") {
			add("Session management and privilege escalation checks")
		}
		if strings.Contains(lower, "crypto") || strings.Contains(lower, "cipher") || strings.Contains(lower, "sign") {
			add("Timing attack, key handling, nonce/IV reuse checks")
		}
		if strings.Contains(lower, "handler") || strings.Contains(lower, "input") || strings.Contains(lower, "request") {
			add("Input sanitization and validation checks")
		}
		if strings.Contains(lower, "migration") || strings.Contains(lower, "schema") {
			add("Migration rollback safety and constraint violation checks")
		}
	}

	out := make([]string, 0, len(unique))
	for item := range unique {
		out = append(out, item)
	}
	return out
}

func IsSecuritySensitive(changedFiles []string) bool {
	for _, file := range changedFiles {
		lower := strings.ToLower(file)
		if strings.Contains(lower, "auth") || strings.Contains(lower, "crypto") || strings.Contains(lower, "security") {
			return true
		}
	}
	return false
}

