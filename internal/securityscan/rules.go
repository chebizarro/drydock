// Package securityscan provides deterministic, regex-based security scanning
// for common vulnerability patterns. It complements the LLM reviewer with
// reliable pattern matching that catches well-known issues LLMs may miss.
package securityscan

import "regexp"

// Rule defines a single security scanning pattern.
type Rule struct {
	ID          string         // unique identifier, e.g. "SEC-001"
	Pattern     *regexp.Regexp // compiled regex to match against source lines
	Severity    string         // "critical", "high", "medium"
	Category    string         // "security"
	Description string         // human-readable explanation
	Languages   []string       // file extensions this rule applies to (empty = all)
	Suggestion  string         // recommended fix
}

// appliesToFile returns true if the rule applies to the given file path.
func (r Rule) appliesToFile(path string) bool {
	if len(r.Languages) == 0 {
		return true // universal rule
	}
	for _, ext := range r.Languages {
		if hasExtension(path, ext) {
			return true
		}
	}
	return false
}

func hasExtension(path, ext string) bool {
	if len(path) < len(ext) {
		return false
	}
	return path[len(path)-len(ext):] == ext
}

// BuiltinRules returns the curated set of security scanning rules.
func BuiltinRules() []Rule {
	return []Rule{
		// --- Hardcoded Secrets ---
		{
			ID:          "SEC-001",
			Pattern:     regexp.MustCompile(`(?i)(api[_-]?key|apikey|secret[_-]?key|auth[_-]?token|access[_-]?token)\s*[:=]\s*["'][\w/+\-]{16,}["']`),
			Severity:    "critical",
			Category:    "security",
			Description: "Hardcoded API key or secret token detected. Secrets should be loaded from environment variables or a secrets manager.",
			Suggestion:  "Move the secret to an environment variable and read it at runtime.",
		},
		{
			ID:          "SEC-002",
			Pattern:     regexp.MustCompile(`(?i)(password|passwd|pwd)\s*[:=]\s*["'][^"']{4,}["']`),
			Severity:    "critical",
			Category:    "security",
			Description: "Hardcoded password detected. Passwords must never be committed to source control.",
			Suggestion:  "Use environment variables or a secrets manager for credentials.",
		},
		{
			ID:       "SEC-003",
			Pattern:  regexp.MustCompile(`(?i)-----BEGIN\s+(RSA\s+)?PRIVATE\s+KEY-----`),
			Severity: "critical",
			Category: "security",
			Description: "Private key committed to source. Private keys must be stored in secure key management, not in code.",
			Suggestion:  "Remove the private key and rotate it immediately.",
		},

		// --- SQL Injection ---
		{
			ID:          "SEC-010",
			Pattern:     regexp.MustCompile(`(?i)(fmt\.Sprintf|".*"\s*\+)\s*.*\b(SELECT|INSERT|UPDATE|DELETE|DROP)\b`),
			Severity:    "high",
			Category:    "security",
			Description: "Possible SQL injection via string concatenation or formatting. Use parameterized queries.",
			Languages:   []string{".go"},
			Suggestion:  "Use parameterized queries (?, $1) instead of string interpolation.",
		},
		{
			ID:          "SEC-011",
			Pattern:     regexp.MustCompile(`(?i)(f["'].*\{.*\}.*(?:SELECT|INSERT|UPDATE|DELETE|DROP)|["'].*(?:SELECT|INSERT|UPDATE|DELETE|DROP).*["']\s*%)`),
			Severity:    "high",
			Category:    "security",
			Description: "Possible SQL injection via string formatting. Use parameterized queries.",
			Languages:   []string{".py"},
			Suggestion:  "Use parameterized queries with placeholders instead of f-strings or % formatting.",
		},

		// --- Command Injection ---
		{
			ID:          "SEC-020",
			Pattern:     regexp.MustCompile(`(?i)exec\.Command\s*\(\s*["'](?:sh|bash|cmd)['"]\s*,\s*["']-c["']`),
			Severity:    "high",
			Category:    "security",
			Description: "Shell command execution via exec.Command with -c flag. User input in the command string enables command injection.",
			Languages:   []string{".go"},
			Suggestion:  "Avoid shell execution. Use exec.Command with explicit arguments instead of sh -c.",
		},
		{
			ID:          "SEC-021",
			Pattern:     regexp.MustCompile(`(?i)(?:os\.system|os\.popen)\s*\(|(?:subprocess\.(?:call|run|Popen))\s*\([^)]*shell\s*=\s*True`),
			Severity:    "high",
			Category:    "security",
			Description: "Shell command execution with shell=True or os.system/popen. User input enables command injection.",
			Languages:   []string{".py"},
			Suggestion:  "Use subprocess.run with a list of arguments and shell=False. Avoid os.system/popen entirely.",
		},

		// --- Path Traversal ---
		{
			ID:          "SEC-030",
			Pattern:     regexp.MustCompile(`(?:os\.Open|os\.ReadFile|ioutil\.ReadFile|os\.Create|filepath\.Join)\s*\(.*\+`),
			Severity:    "medium",
			Category:    "security",
			Description: "File path constructed with string concatenation. Unsanitized user input may enable path traversal (../).",
			Languages:   []string{".go"},
			Suggestion:  "Validate paths with filepath.Clean and ensure they don't escape the intended directory.",
		},

		// --- Insecure Crypto ---
		{
			ID:          "SEC-040",
			Pattern:     regexp.MustCompile(`(?i)\b(?:md5|sha1)\.(?:New|Sum)\b`),
			Severity:    "medium",
			Category:    "security",
			Description: "Weak hash algorithm (MD5/SHA1) used. These are vulnerable to collision attacks and should not be used for security purposes.",
			Languages:   []string{".go"},
			Suggestion:  "Use SHA-256 or stronger for security-sensitive hashing. MD5/SHA1 are acceptable only for non-security checksums.",
		},
		{
			ID:          "SEC-041",
			Pattern:     regexp.MustCompile(`(?i)\b(?:hashlib\.(?:md5|sha1)|Digest::(?:MD5|SHA1))\b`),
			Severity:    "medium",
			Category:    "security",
			Description: "Weak hash algorithm (MD5/SHA1) used. These are vulnerable to collision attacks.",
			Languages:   []string{".py", ".rb"},
			Suggestion:  "Use SHA-256 or stronger for security-sensitive hashing.",
		},
		{
			ID:          "SEC-042",
			Pattern:     regexp.MustCompile(`(?i)\b(?:DES|RC4|Blowfish)\b.*(?:cipher|encrypt|crypt)`),
			Severity:    "high",
			Category:    "security",
			Description: "Weak or deprecated cipher algorithm detected. DES, RC4, and Blowfish are considered broken.",
			Suggestion:  "Use AES-256-GCM or ChaCha20-Poly1305 for encryption.",
		},

		// --- XSS ---
		{
			ID:          "SEC-050",
			Pattern:     regexp.MustCompile(`(?i)(?:innerHTML|outerHTML|document\.write|\.html\()\s*(?:=|\()\s*[^"']`),
			Severity:    "high",
			Category:    "security",
			Description: "Potential cross-site scripting (XSS) via unsafe DOM manipulation. User input injected into innerHTML or document.write can execute arbitrary scripts.",
			Languages:   []string{".js", ".ts", ".jsx", ".tsx"},
			Suggestion:  "Use textContent instead of innerHTML, or sanitize input with a library like DOMPurify.",
		},

		// --- SSRF ---
		{
			ID:          "SEC-060",
			Pattern:     regexp.MustCompile(`(?i)(?:http\.Get|http\.Post|requests\.get|requests\.post)\s*\(\s*(?:.*\+|.*fmt\.|.*[Ff]ormat|.*%[sv])`),
			Severity:    "medium",
			Category:    "security",
			Description: "HTTP request URL constructed from dynamic input. This may enable server-side request forgery (SSRF) if the URL is user-controlled.",
			Suggestion:  "Validate and allowlist target URLs. Block requests to internal/private IP ranges.",
		},

		// --- Insecure TLS ---
		{
			ID:          "SEC-070",
			Pattern:     regexp.MustCompile(`(?i)InsecureSkipVerify\s*:\s*true`),
			Severity:    "high",
			Category:    "security",
			Description: "TLS certificate verification disabled. This allows man-in-the-middle attacks.",
			Languages:   []string{".go"},
			Suggestion:  "Remove InsecureSkipVerify or use it only in test environments with build tags.",
		},

		// --- Unsafe Deserialization ---
		{
			ID:          "SEC-080",
			Pattern:     regexp.MustCompile(`(?i)(?:pickle\.loads?|yaml\.(?:load|unsafe_load))\s*\(`),
			Severity:    "high",
			Category:    "security",
			Description: "Unsafe deserialization detected. Deserializing untrusted data can lead to remote code execution.",
			Languages:   []string{".py"},
			Suggestion:  "Use yaml.safe_load instead of yaml.load. Avoid pickle for untrusted data.",
		},
	}
}
