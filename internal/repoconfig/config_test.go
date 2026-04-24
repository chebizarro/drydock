package repoconfig

import (
	"strings"
	"testing"
)

func TestDefaultsOnEmptyInput(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Version != 1 {
		t.Errorf("version = %d, want 1", cfg.Version)
	}
	if cfg.Review.SeverityFloor != "info" {
		t.Errorf("severity_floor = %q, want info", cfg.Review.SeverityFloor)
	}
	if cfg.Review.DetailSeverityFloor != "high" {
		t.Errorf("detail_severity_floor = %q, want high", cfg.Review.DetailSeverityFloor)
	}
	if !cfg.DocsEnabled() {
		t.Error("expected docs enabled by default")
	}
	if len(cfg.Review.Categories) != 0 {
		t.Errorf("expected no categories, got %v", cfg.Review.Categories)
	}
}

func TestDefaultsOnEmptyBytes(t *testing.T) {
	cfg, err := Parse([]byte(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Version != 1 {
		t.Errorf("version = %d, want 1", cfg.Version)
	}
}

func TestValidParse(t *testing.T) {
	yaml := `
version: 1
review:
  severity_floor: medium
  categories:
    - security
    - correctness
  detail_severity_floor: high
context:
  token_budget: 32000
  exclude_paths:
    - "vendor/**"
    - "docs/generated/**"
  include_docs: true
instructions: |
  Prioritize API compatibility.
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Review.SeverityFloor != "medium" {
		t.Errorf("severity_floor = %q, want medium", cfg.Review.SeverityFloor)
	}
	if len(cfg.Review.Categories) != 2 {
		t.Fatalf("categories = %v, want 2", cfg.Review.Categories)
	}
	if cfg.Review.Categories[0] != "security" || cfg.Review.Categories[1] != "correctness" {
		t.Errorf("categories = %v", cfg.Review.Categories)
	}
	if cfg.Context.TokenBudget != 32000 {
		t.Errorf("token_budget = %d, want 32000", cfg.Context.TokenBudget)
	}
	if len(cfg.Context.ExcludePaths) != 2 {
		t.Errorf("exclude_paths = %v, want 2", cfg.Context.ExcludePaths)
	}
	if !cfg.DocsEnabled() {
		t.Error("expected docs enabled")
	}
	if !strings.Contains(cfg.Instructions, "API compatibility") {
		t.Errorf("instructions = %q", cfg.Instructions)
	}
}

func TestInvalidVersion(t *testing.T) {
	yaml := `version: 99`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for unsupported version")
	}
	if !strings.Contains(err.Error(), "unsupported version") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestInvalidSeverity(t *testing.T) {
	yaml := `
version: 1
review:
  severity_floor: extreme
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for invalid severity")
	}
	if !strings.Contains(err.Error(), "invalid severity_floor") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestInvalidCategory(t *testing.T) {
	yaml := `
version: 1
review:
  categories:
    - performance
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for invalid category")
	}
	if !strings.Contains(err.Error(), "invalid category") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestAbsoluteExcludePath(t *testing.T) {
	yaml := `
version: 1
context:
  exclude_paths:
    - "/etc/passwd"
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for absolute exclude path")
	}
	if !strings.Contains(err.Error(), "must be relative") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestTraversalExcludePath(t *testing.T) {
	yaml := `
version: 1
context:
  exclude_paths:
    - "../secret"
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for traversal exclude path")
	}
	if !strings.Contains(err.Error(), "must not contain") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestInstructionLengthCap(t *testing.T) {
	yaml := "version: 1\ninstructions: " + strings.Repeat("x", 5000)
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for oversized instructions")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestDocsDisabled(t *testing.T) {
	yaml := `
version: 1
context:
  include_docs: false
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DocsEnabled() {
		t.Error("expected docs disabled")
	}
}

func TestAllowsCategory(t *testing.T) {
	cfg := RepoConfig{
		Review: ReviewConfig{
			Categories: []string{"security", "correctness"},
		},
	}
	if !cfg.AllowsCategory("security") {
		t.Error("should allow security")
	}
	if cfg.AllowsCategory("style") {
		t.Error("should not allow style")
	}

	// No categories = allow all.
	cfg2 := Default()
	if !cfg2.AllowsCategory("style") {
		t.Error("default should allow all categories")
	}
}

func TestAllowsSeverity(t *testing.T) {
	cfg := RepoConfig{
		Review: ReviewConfig{SeverityFloor: "medium"},
	}
	if !cfg.AllowsSeverity("high") {
		t.Error("high should be above medium floor")
	}
	if cfg.AllowsSeverity("low") {
		t.Error("low should be below medium floor")
	}
}

func TestPromptInstructions(t *testing.T) {
	cfg := RepoConfig{
		Review: ReviewConfig{
			SeverityFloor: "medium",
			Categories:    []string{"security"},
		},
		Instructions: "Focus on auth flows.",
	}
	pi := cfg.PromptInstructions()
	if !strings.Contains(pi, "medium") {
		t.Errorf("expected severity floor in instructions: %q", pi)
	}
	if !strings.Contains(pi, "security") {
		t.Errorf("expected categories in instructions: %q", pi)
	}
	if !strings.Contains(pi, "auth flows") {
		t.Errorf("expected custom instructions: %q", pi)
	}
}

func TestDuplicateCategoriesDeduped(t *testing.T) {
	yaml := `
version: 1
review:
  categories:
    - security
    - security
    - correctness
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Review.Categories) != 2 {
		t.Errorf("expected 2 deduped categories, got %v", cfg.Review.Categories)
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	yaml := `
version: 1
reveiw:
	severity_floor: high
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for unknown field 'reveiw'")
	}
	if !strings.Contains(err.Error(), "parse .drydock.yaml") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestStatusConfigDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Status.Enabled {
		t.Error("status should be disabled by default")
	}
}

func TestStatusConfigValid(t *testing.T) {
	yml := "version: 1\nstatus:\n  enabled: true\n  open_severity_floor: high\n  min_confidence: 0.85\n"
	cfg, err := Parse([]byte(yml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Status.Enabled {
		t.Error("expected status enabled")
	}
	if cfg.Status.OpenSeverityFloor != "high" {
		t.Errorf("expected open_severity_floor 'high', got %q", cfg.Status.OpenSeverityFloor)
	}
	if cfg.Status.MinConfidence != 0.85 {
		t.Errorf("expected min_confidence 0.85, got %f", cfg.Status.MinConfidence)
	}
}

func TestStatusConfigDefaultsMinConfidence(t *testing.T) {
	yml := "version: 1\nstatus:\n  enabled: true\n"
	cfg, err := Parse([]byte(yml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Status.MinConfidence != 0.90 {
		t.Errorf("expected default min_confidence 0.90, got %f", cfg.Status.MinConfidence)
	}
	if cfg.Status.OpenSeverityFloor != "critical" {
		t.Errorf("expected default open_severity_floor 'critical', got %q", cfg.Status.OpenSeverityFloor)
	}
}

func TestStatusConfigInvalidSeverity(t *testing.T) {
	yaml := `
version: 1
status:
	enabled: true
	open_severity_floor: extreme
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for invalid status severity")
	}
}

func TestStatusConfigInvalidConfidence(t *testing.T) {
	yaml := `
version: 1
status:
	enabled: true
	min_confidence: 1.5
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for out-of-range confidence")
	}
}

func TestAutoFixConfigDefaults(t *testing.T) {
	cfg := Default()
	if cfg.AutoFix.Enabled {
		t.Error("autofix should be disabled by default")
	}
	if cfg.AutoFix.MinConfidence != 0.97 {
		t.Errorf("expected default min_confidence 0.97, got %f", cfg.AutoFix.MinConfidence)
	}
	if cfg.AutoFix.MaxFindings != 3 {
		t.Errorf("expected default max_findings 3, got %d", cfg.AutoFix.MaxFindings)
	}
}

func TestAutoFixConfigValid(t *testing.T) {
	yml := "version: 1\nautofix:\n  enabled: true\n  min_confidence: 0.95\n  max_findings: 5\n"
	cfg, err := Parse([]byte(yml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.AutoFix.Enabled {
		t.Error("expected autofix enabled")
	}
	if cfg.AutoFix.MinConfidence != 0.95 {
		t.Errorf("expected min_confidence 0.95, got %f", cfg.AutoFix.MinConfidence)
	}
	if cfg.AutoFix.MaxFindings != 5 {
		t.Errorf("expected max_findings 5, got %d", cfg.AutoFix.MaxFindings)
	}
}

func TestAutoFixConfigDefaultsMinConfidence(t *testing.T) {
	yml := "version: 1\nautofix:\n  enabled: true\n"
	cfg, err := Parse([]byte(yml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AutoFix.MinConfidence != 0.97 {
		t.Errorf("expected default min_confidence 0.97, got %f", cfg.AutoFix.MinConfidence)
	}
	if cfg.AutoFix.MaxFindings != 3 {
		t.Errorf("expected default max_findings 3, got %d", cfg.AutoFix.MaxFindings)
	}
}

func TestAutoFixConfigInvalidConfidence(t *testing.T) {
	yml := "version: 1\nautofix:\n  enabled: true\n  min_confidence: 1.5\n"
	_, err := Parse([]byte(yml))
	if err == nil {
		t.Fatal("expected error for out-of-range autofix confidence")
	}
}

func TestMissingSeverityFloorDefaults(t *testing.T) {
	yaml := `
version: 1
review:
  categories:
    - security
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Review.SeverityFloor != "info" {
		t.Errorf("expected default severity_floor 'info', got %q", cfg.Review.SeverityFloor)
	}
}
