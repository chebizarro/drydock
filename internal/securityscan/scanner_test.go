package securityscan

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"drydock/internal/reviewengine"
)

func TestBuiltinRulesCompile(t *testing.T) {
	rules := BuiltinRules()
	if len(rules) == 0 {
		t.Fatal("expected at least one builtin rule")
	}
	for _, r := range rules {
		if r.ID == "" {
			t.Error("rule has empty ID")
		}
		if r.Pattern == nil {
			t.Errorf("rule %s has nil pattern", r.ID)
		}
		if r.Severity == "" {
			t.Errorf("rule %s has empty severity", r.ID)
		}
		if r.Category != "security" {
			t.Errorf("rule %s has unexpected category: %s", r.ID, r.Category)
		}
	}
}

func TestScanDetectsHardcodedAPIKey(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.go", `package config

const apiKey = "sk-1234567890abcdef1234567890abcdef"
`)
	scanner := New()
	result := scanner.ScanFiles(context.Background(), dir, []string{"config.go"}, "")

	if len(result.Findings) == 0 {
		t.Fatal("expected at least one finding for hardcoded API key")
	}
	found := false
	for _, f := range result.Findings {
		if f.RuleID == "SEC-001" {
			found = true
			if f.Severity != "critical" {
				t.Errorf("expected critical severity, got %s", f.Severity)
			}
			if f.Line != 3 {
				t.Errorf("expected line 3, got %d", f.Line)
			}
			if f.File != "config.go" {
				t.Errorf("expected file config.go, got %s", f.File)
			}
			if f.Confidence != 1.0 {
				t.Errorf("expected confidence 1.0, got %f", f.Confidence)
			}
		}
	}
	if !found {
		t.Error("SEC-001 rule should have matched hardcoded API key")
	}
}

func TestScanDetectsHardcodedPassword(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "db.go", `package db

var password = "super_secret_password123"
`)
	scanner := New()
	result := scanner.ScanFiles(context.Background(), dir, []string{"db.go"}, "")

	foundSEC002 := false
	for _, f := range result.Findings {
		if f.RuleID == "SEC-002" {
			foundSEC002 = true
		}
	}
	if !foundSEC002 {
		t.Error("SEC-002 should match hardcoded password")
	}
}

func TestScanDetectsPrivateKey(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "certs.go", `package certs

var key = "-----BEGIN RSA PRIVATE KEY-----"
`)
	scanner := New()
	result := scanner.ScanFiles(context.Background(), dir, []string{"certs.go"}, "")

	foundSEC003 := false
	for _, f := range result.Findings {
		if f.RuleID == "SEC-003" {
			foundSEC003 = true
		}
	}
	if !foundSEC003 {
		t.Error("SEC-003 should match private key")
	}
}

func TestScanDetectsSQLInjection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "query.go", `package db

func getUser(name string) {
	q := fmt.Sprintf("SELECT * FROM users WHERE name='%s'", name)
}
`)
	scanner := New()
	result := scanner.ScanFiles(context.Background(), dir, []string{"query.go"}, "")

	foundSEC010 := false
	for _, f := range result.Findings {
		if f.RuleID == "SEC-010" {
			foundSEC010 = true
		}
	}
	if !foundSEC010 {
		t.Error("SEC-010 should match SQL injection via fmt.Sprintf")
	}
}

func TestScanDetectsInsecureTLS(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "http.go", `package client

var cfg = &tls.Config{InsecureSkipVerify: true}
`)
	scanner := New()
	result := scanner.ScanFiles(context.Background(), dir, []string{"http.go"}, "")

	foundSEC070 := false
	for _, f := range result.Findings {
		if f.RuleID == "SEC-070" {
			foundSEC070 = true
		}
	}
	if !foundSEC070 {
		t.Error("SEC-070 should match InsecureSkipVerify")
	}
}

func TestScanDetectsWeakHash(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hash.go", `package crypto

h := md5.New()
`)
	scanner := New()
	result := scanner.ScanFiles(context.Background(), dir, []string{"hash.go"}, "")

	foundSEC040 := false
	for _, f := range result.Findings {
		if f.RuleID == "SEC-040" {
			foundSEC040 = true
		}
	}
	if !foundSEC040 {
		t.Error("SEC-040 should match weak hash md5.New()")
	}
}

func TestScanCleanFileNoFindings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "safe.go", `package main

func main() {
	fmt.Println("Hello, world!")
}
`)
	scanner := New()
	result := scanner.ScanFiles(context.Background(), dir, []string{"safe.go"}, "")

	if len(result.Findings) != 0 {
		t.Errorf("expected 0 findings for clean file, got %d", len(result.Findings))
		for _, f := range result.Findings {
			t.Logf("  %s: %s (line %d)", f.RuleID, f.Evidence, f.Line)
		}
	}
	if result.FilesScanned != 1 {
		t.Errorf("expected 1 file scanned, got %d", result.FilesScanned)
	}
}

func TestScanLanguageFiltering(t *testing.T) {
	dir := t.TempDir()
	// Python-only rule should not fire on a .go file
	writeFile(t, dir, "test.go", `package main

import "os"
os.system("ls")
`)
	scanner := New()
	result := scanner.ScanFiles(context.Background(), dir, []string{"test.go"}, "")

	for _, f := range result.Findings {
		if f.RuleID == "SEC-021" {
			t.Error("Python-only rule SEC-021 should not match .go files")
		}
	}
}

func TestScanDiffAwareOnlyAddedLines(t *testing.T) {
	dir := t.TempDir()
	// File has hardcoded secret on line 5 (pre-existing) and line 10 (new)
	writeFile(t, dir, "config.go", `package config

// existing code
// filler
const oldKey = "api_key: abcdef1234567890abcdef"
// more filler
// more filler
// more filler
// more filler
const newKey = "api_key: 1234567890abcdef1234567890"
`)

	diff := `diff --git a/config.go b/config.go
--- a/config.go
+++ b/config.go
@@ -9,0 +9,2 @@
+// more filler
+const newKey = "api_key: 1234567890abcdef1234567890"
`

	scanner := New()
	result := scanner.ScanFiles(context.Background(), dir, []string{"config.go"}, diff)

	// Should only flag line 10 (added), not line 5 (pre-existing).
	for _, f := range result.Findings {
		if f.RuleID == "SEC-001" && f.Line == 5 {
			t.Error("diff-aware scan should not flag pre-existing issues (line 5)")
		}
	}
}

func TestScanDeletedFileNoError(t *testing.T) {
	dir := t.TempDir()
	// File doesn't exist — scanner should gracefully skip.
	scanner := New()
	result := scanner.ScanFiles(context.Background(), dir, []string{"nonexistent.go"}, "")

	if len(result.Findings) != 0 {
		t.Error("should produce no findings for nonexistent file")
	}
	if result.FilesScanned != 1 {
		t.Errorf("expected 1 file attempted, got %d", result.FilesScanned)
	}
}

func TestScanContextCancellation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.go", `package a
const key = "api_key: 1234567890abcdef1234567890"
`)
	writeFile(t, dir, "b.go", `package b
const key = "api_key: 1234567890abcdef1234567890"
`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	scanner := New()
	result := scanner.ScanFiles(ctx, dir, []string{"a.go", "b.go"}, "")

	// Should stop early — may scan 0 or 1 files.
	if result.FilesScanned > 1 {
		t.Error("should stop scanning after context cancellation")
	}
}

func TestDeduplicateFindings_MergesOverlap(t *testing.T) {
	scanFindings := []SecurityFinding{
		{
			RuleID:   "SEC-001",
			Severity: "critical",
			Category: "security",
			File:     "config.go",
			Line:     10,
			Evidence: "hardcoded key",
		},
	}
	llmFindings := []reviewengine.Finding{
		{
			Severity:   "high",
			Category:   "security",
			File:       "config.go",
			Line:       10,
			Evidence:   "API key exposed",
			Confidence: 0.8,
		},
	}

	merged := DeduplicateFindings(scanFindings, llmFindings)

	// Should have 1 finding (merged), not 2.
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged finding, got %d", len(merged))
	}

	// LLM finding should be boosted.
	if merged[0].Confidence < 0.9 {
		t.Errorf("confidence should be boosted, got %f", merged[0].Confidence)
	}
	// Severity should be upgraded to critical.
	if merged[0].Severity != "critical" {
		t.Errorf("severity should be upgraded to critical, got %s", merged[0].Severity)
	}
	// Should have SAST marker in evidence.
	if !strings.Contains(merged[0].Evidence, "SAST: SEC-001") {
		t.Error("merged finding should contain SAST rule reference")
	}
}

func TestDeduplicateFindings_CategoryMismatchNoMerge(t *testing.T) {
	scanFindings := []SecurityFinding{
		{RuleID: "SEC-001", Severity: "critical", Category: "security", File: "config.go", Line: 10},
	}
	llmFindings := []reviewengine.Finding{
		{Severity: "medium", Category: "correctness", File: "config.go", Line: 10, Confidence: 0.8},
	}
	merged := DeduplicateFindings(scanFindings, llmFindings)
	// Category mismatch — should NOT merge, should have 2 findings.
	if len(merged) != 2 {
		t.Fatalf("expected 2 findings (category mismatch), got %d", len(merged))
	}
}

func TestDeduplicateFindings_NearbyLineMatch(t *testing.T) {
	scanFindings := []SecurityFinding{
		{RuleID: "SEC-010", Severity: "high", Category: "security", File: "db.go", Line: 42},
	}
	llmFindings := []reviewengine.Finding{
		{Severity: "medium", Category: "security", File: "db.go", Line: 44, Confidence: 0.7},
	}

	merged := DeduplicateFindings(scanFindings, llmFindings)

	// Line 42 and 44 are within ±3, should merge.
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged finding (nearby line), got %d", len(merged))
	}
	if !strings.Contains(merged[0].Evidence, "SAST") {
		t.Error("nearby-line match should still merge")
	}
}

func TestDeduplicateFindings_UnmatchedPrepended(t *testing.T) {
	scanFindings := []SecurityFinding{
		{RuleID: "SEC-070", Severity: "high", Category: "security", File: "http.go", Line: 5},
	}
	llmFindings := []reviewengine.Finding{
		{Severity: "medium", Category: "correctness", File: "main.go", Line: 100, Confidence: 0.6},
	}

	merged := DeduplicateFindings(scanFindings, llmFindings)

	// Should have 2 findings — scanner finding prepended.
	if len(merged) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(merged))
	}
	if merged[0].File != "http.go" {
		t.Error("scanner finding should be prepended")
	}
	if !strings.Contains(merged[0].Explanation, "SEC-070") {
		t.Error("unmatched scanner finding should contain rule ID in explanation")
	}
	if merged[1].File != "main.go" {
		t.Error("LLM finding should follow")
	}
}

func TestDeduplicateFindings_EmptyScan(t *testing.T) {
	llmFindings := []reviewengine.Finding{
		{Severity: "medium", File: "main.go", Line: 1},
	}

	merged := DeduplicateFindings(nil, llmFindings)
	if len(merged) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(merged))
	}
}

func TestProviderLayerNameAndPriority(t *testing.T) {
	provider := NewProvider(New())
	if provider.LayerName() != "security-scan" {
		t.Errorf("expected layer name 'security-scan', got %s", provider.LayerName())
	}
	if provider.Priority() != 1 {
		t.Errorf("expected priority 1, got %d", provider.Priority())
	}
}

func TestParseHunkNewStart(t *testing.T) {
	tests := []struct {
		line     string
		expected int
	}{
		{"@@ -1,5 +1,7 @@", 1},
		{"@@ -10,3 +15,5 @@", 15},
		{"@@ -0,0 +1,10 @@ package main", 1},
		{"@@ -5 +5 @@", 5},
	}
	for _, tc := range tests {
		got := parseHunkNewStart(tc.line)
		if got != tc.expected {
			t.Errorf("parseHunkNewStart(%q) = %d, want %d", tc.line, got, tc.expected)
		}
	}
}

func TestRuleAppliesToFile(t *testing.T) {
	goRule := Rule{Languages: []string{".go"}, Pattern: regexp.MustCompile("test")}
	universalRule := Rule{Pattern: regexp.MustCompile("test")}

	if !goRule.appliesToFile("main.go") {
		t.Error(".go rule should apply to main.go")
	}
	if goRule.appliesToFile("main.py") {
		t.Error(".go rule should not apply to main.py")
	}
	if !universalRule.appliesToFile("anything.xyz") {
		t.Error("universal rule should apply to any file")
	}
}

func TestEvidenceTruncation(t *testing.T) {
	dir := t.TempDir()
	// Write a file with a very long line containing a secret.
	longLine := `const api_key = "` + strings.Repeat("a", 300) + `"`
	writeFile(t, dir, "long.go", "package main\n"+longLine+"\n")

	scanner := New()
	result := scanner.ScanFiles(context.Background(), dir, []string{"long.go"}, "")

	for _, f := range result.Findings {
		if len(f.Evidence) > 210 { // 200 + "..."
			t.Errorf("evidence should be truncated, got %d chars", len(f.Evidence))
		}
	}
}

// --- helper ---

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
