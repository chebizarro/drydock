package repoconfig

import (
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
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

func TestPaymentsConfigDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Payments.Enabled {
		t.Error("payments should be disabled by default")
	}
}

func TestPaymentsConfigFreeAccessDefaults(t *testing.T) {
	cfg := Default()
	if !cfg.Payments.MaintainersAreFree() {
		t.Fatal("repository maintainers should be free by default")
	}
}

func TestPaymentsConfigNormalizesFreePubkeys(t *testing.T) {
	pubkey := nostr.GetPublicKey(nostr.Generate())
	npub := nip19.EncodeNpub(pubkey)
	yml := "version: 1\npayments:\n  free_pubkeys:\n    - " + npub + "\n    - " + pubkey.Hex() + "\n  free_for_maintainers: false\n"
	cfg, err := Parse([]byte(yml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Payments.FreePubkeys) != 1 || cfg.Payments.FreePubkeys[0] != pubkey.Hex() {
		t.Fatalf("unexpected normalized free pubkeys: %#v", cfg.Payments.FreePubkeys)
	}
	if cfg.Payments.MaintainersAreFree() {
		t.Fatal("free_for_maintainers=false should be preserved")
	}
}

func TestPaymentsConfigRejectsInvalidFreePubkey(t *testing.T) {
	_, err := Parse([]byte("version: 1\npayments:\n  free_pubkeys: [not-a-pubkey]\n"))
	if err == nil || !strings.Contains(err.Error(), "invalid payments.free_pubkeys") {
		t.Fatalf("expected invalid free_pubkeys error, got %v", err)
	}
}

func TestPaymentsConfigValid(t *testing.T) {
	yml := "version: 1\npayments:\n  enabled: true\n  price_sats: 100\n  free_reviews_per_day: 2\n"
	cfg, err := Parse([]byte(yml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Payments.Enabled {
		t.Error("expected payments enabled")
	}
	if cfg.Payments.PriceSats != 100 {
		t.Errorf("expected price_sats 100, got %d", cfg.Payments.PriceSats)
	}
	if cfg.Payments.FreeReviewsPerDay != 2 {
		t.Errorf("expected free_reviews_per_day 2, got %d", cfg.Payments.FreeReviewsPerDay)
	}
}

func TestPaymentsConfigWithSubscription(t *testing.T) {
	yml := "version: 1\npayments:\n  enabled: true\n  price_sats: 100\n  subscription_price_sats: 2000\n  subscription_days: 30\n"
	cfg, err := Parse([]byte(yml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Payments.SubscriptionPriceSats != 2000 {
		t.Errorf("expected subscription_price_sats 2000, got %d", cfg.Payments.SubscriptionPriceSats)
	}
	if cfg.Payments.SubscriptionDays != 30 {
		t.Errorf("expected subscription_days 30, got %d", cfg.Payments.SubscriptionDays)
	}
}

func TestPaymentsConfigMissingPrice(t *testing.T) {
	yml := "version: 1\npayments:\n  enabled: true\n"
	_, err := Parse([]byte(yml))
	if err == nil {
		t.Fatal("expected error for missing price_sats")
	}
}

func TestPaymentsConfigPartialSubscription(t *testing.T) {
	yml := "version: 1\npayments:\n  enabled: true\n  price_sats: 100\n  subscription_price_sats: 2000\n"
	_, err := Parse([]byte(yml))
	if err == nil {
		t.Fatal("expected error for partial subscription config")
	}
}

func TestContainsPaymentsConfig(t *testing.T) {
	if !ContainsPaymentsConfig([]byte("payments:\n  enabled: true")) {
		t.Error("expected to detect payments section")
	}
	if ContainsPaymentsConfig([]byte("review:\n  severity_floor: high")) {
		t.Error("should not detect payments in review-only config")
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

func TestEnsembleConfigDefaults(t *testing.T) {
	yml := "version: 1\n"
	cfg, err := Parse([]byte(yml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Ensemble.Enabled {
		t.Error("expected ensemble disabled by default")
	}
	// When disabled, models may be empty (defaults applied only when enabled)
}

func TestEnsembleConfigValid(t *testing.T) {
	yml := `
version: 1
ensemble:
  enabled: true
  models:
    - coder32b
    - llm70b
    - coder14b
  consensus_boost: 0.15
  require_consensus: true
`
	cfg, err := Parse([]byte(yml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Ensemble.Enabled {
		t.Error("expected ensemble enabled")
	}
	if len(cfg.Ensemble.Models) != 3 {
		t.Errorf("expected 3 models, got %d", len(cfg.Ensemble.Models))
	}
	if cfg.Ensemble.ConsensusBoost != 0.15 {
		t.Errorf("expected consensus_boost 0.15, got %f", cfg.Ensemble.ConsensusBoost)
	}
	if !cfg.Ensemble.RequireConsensus {
		t.Error("expected require_consensus true")
	}
}

func TestEnsembleConfigInvalidModel(t *testing.T) {
	yml := `
version: 1
ensemble:
  enabled: true
  models:
    - coder32b
    - invalid_model
`
	_, err := Parse([]byte(yml))
	if err == nil {
		t.Fatal("expected error for invalid model")
	}
}

func TestEnsembleConfigInvalidBoost(t *testing.T) {
	yml := `
version: 1
ensemble:
  enabled: true
  models:
    - coder32b
  consensus_boost: 0.75
`
	_, err := Parse([]byte(yml))
	if err == nil {
		t.Fatal("expected error for consensus_boost > 0.5")
	}
}

func TestEnsembleConfigDefaultModels(t *testing.T) {
	yml := `
version: 1
ensemble:
  enabled: true
`
	cfg, err := Parse([]byte(yml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Ensemble.Models) != 2 {
		t.Errorf("expected 2 default models when enabled with no models specified, got %d", len(cfg.Ensemble.Models))
	}
}

func TestEnsembleToReviewEngineConfig(t *testing.T) {
	cfg := EnsembleConfig{
		Enabled:          true,
		Models:           []string{"coder32b", "llm70b"},
		ConsensusBoost:   0.12,
		RequireConsensus: true,
	}
	reCfg := cfg.ToReviewEngineEnsembleConfig()
	if !reCfg.Enabled {
		t.Error("expected enabled")
	}
	if len(reCfg.Models) != 2 {
		t.Errorf("expected 2 routes, got %d", len(reCfg.Models))
	}
	if reCfg.ConsensusBoost != 0.12 {
		t.Errorf("expected boost 0.12, got %f", reCfg.ConsensusBoost)
	}
	if !reCfg.RequireConsensus {
		t.Error("expected require_consensus")
	}
}

func TestParseReviewStatuses(t *testing.T) {
	t.Run("default is open only", func(t *testing.T) {
		cfg, err := Parse([]byte("version: 1\n"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(cfg.Review.Statuses) != 1 || cfg.Review.Statuses[0] != "open" {
			t.Fatalf("expected default statuses [open], got %v", cfg.Review.Statuses)
		}
	})

	t.Run("draft opt-in", func(t *testing.T) {
		cfg, err := Parse([]byte("version: 1\nreview:\n  statuses: [Open, DRAFT]\n"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(cfg.Review.Statuses) != 2 || cfg.Review.Statuses[0] != "open" || cfg.Review.Statuses[1] != "draft" {
			t.Fatalf("expected normalized [open draft], got %v", cfg.Review.Statuses)
		}
	})

	t.Run("merged and closed rejected", func(t *testing.T) {
		for _, s := range []string{"merged", "applied", "closed"} {
			if _, err := Parse([]byte("version: 1\nreview:\n  statuses: [" + s + "]\n")); err == nil {
				t.Fatalf("expected error for status %q", s)
			}
		}
	})

	t.Run("unknown status rejected", func(t *testing.T) {
		if _, err := Parse([]byte("version: 1\nreview:\n  statuses: [wip]\n")); err == nil {
			t.Fatal("expected error for unknown status")
		}
	})
}
