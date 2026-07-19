// Package repoconfig loads per-repository review configuration from .drydock.yaml.
package repoconfig

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"drydock/internal/reviewengine"

	"gopkg.in/yaml.v3"
)

const (
	// ConfigFileName is the name of the per-repo configuration file.
	ConfigFileName = ".drydock.yaml"

	// maxInstructionBytes caps the instructions field to prevent prompt bloat.
	maxInstructionBytes = 4096

	// currentVersion is the only supported schema version.
	currentVersion = 1
)

// RepoConfig is the per-repository review configuration.
type RepoConfig struct {
	Version      int            `yaml:"version"`
	Review       ReviewConfig   `yaml:"review"`
	Context      ContextConfig  `yaml:"context"`
	Status       StatusConfig   `yaml:"status"`
	AutoFix      AutoFixConfig  `yaml:"autofix"`
	Payments     PaymentsConfig `yaml:"payments"`
	Ensemble     EnsembleConfig `yaml:"ensemble"`
	Instructions string         `yaml:"instructions"`
}

// ReviewConfig controls which findings are published.
type ReviewConfig struct {
	SeverityFloor       string   `yaml:"severity_floor"`
	Categories          []string `yaml:"categories"`
	DetailSeverityFloor string   `yaml:"detail_severity_floor"`
	Walkthrough         *bool    `yaml:"walkthrough"` // pointer to distinguish missing from false
	// Statuses lists the NIP-34 root statuses for which reviews run
	// automatically. Allowed values: "open", "draft". Defaults to ["open"].
	// A root with no status event counts as open. Applied/merged and closed
	// roots are never reviewed automatically and cannot be enabled here.
	Statuses []string `yaml:"statuses"`
}

// ContextConfig controls context builder behavior.
type ContextConfig struct {
	TokenBudget  int      `yaml:"token_budget"`
	ExcludePaths []string `yaml:"exclude_paths"`
	IncludeDocs  *bool    `yaml:"include_docs"` // pointer to distinguish missing from false
}

// PaymentsConfig controls Cashu ecash payment gating for review access.
// When enabled, reviews require either an active subscription, a Cashu token
// attached to the patch event, or available free-tier quota.
type PaymentsConfig struct {
	Enabled               bool  `yaml:"enabled"`
	PriceSats             int64 `yaml:"price_sats"`              // per-review price in sats
	FreeReviewsPerDay     int   `yaml:"free_reviews_per_day"`    // free reviews per author per day
	SubscriptionPriceSats int64 `yaml:"subscription_price_sats"` // subscription price in sats
	SubscriptionDays      int   `yaml:"subscription_days"`       // subscription duration in days
}

// EnsembleConfig controls multi-model ensemble review mode.
// When enabled, reviews run multiple models in parallel and merge findings
// using consensus scoring — findings reported by multiple models get boosted
// confidence.
type EnsembleConfig struct {
	Enabled          bool     `yaml:"enabled"`
	Models           []string `yaml:"models"`            // model routes: coder32b, llm70b, coder14b
	ConsensusBoost   float64  `yaml:"consensus_boost"`   // confidence boost per additional model (default 0.1)
	RequireConsensus bool     `yaml:"require_consensus"` // only include findings from 2+ models
}

// AutoFixConfig controls automatic fix-patch generation and publication.
// When enabled, Drydock synthesizes a combined NIP-34 kind 1617 patch event
// from high-confidence SuggestedDiff findings that apply cleanly.
type AutoFixConfig struct {
	Enabled       bool    `yaml:"enabled"`
	MinConfidence float64 `yaml:"min_confidence"` // minimum finding confidence to include
	MaxFindings   int     `yaml:"max_findings"`   // cap on findings per auto-fix patch
}

// StatusConfig controls NIP-34 review status event publication.
// Status events are opt-in; when disabled (default), Drydock only publishes
// review comments and never emits kind 1630 status events.
type StatusConfig struct {
	Enabled           bool    `yaml:"enabled"`
	OpenSeverityFloor string  `yaml:"open_severity_floor"` // findings at or above this trigger a 1630 status
	MinConfidence     float64 `yaml:"min_confidence"`      // minimum review confidence to publish status
}

// Default returns a RepoConfig with sensible defaults.
func Default() RepoConfig {
	includeDocs := true
	walkthrough := true
	return RepoConfig{
		Version: currentVersion,
		Review: ReviewConfig{
			SeverityFloor:       "info",
			DetailSeverityFloor: "high",
			Walkthrough:         &walkthrough,
			Statuses:            []string{"open"},
		},
		Context: ContextConfig{
			IncludeDocs: &includeDocs,
		},
		Status: StatusConfig{
			Enabled:           false,
			OpenSeverityFloor: "critical",
			MinConfidence:     0.90,
		},
		AutoFix: AutoFixConfig{
			Enabled:       false,
			MinConfidence: 0.97,
			MaxFindings:   3,
		},
		Payments: PaymentsConfig{
			Enabled:           false,
			FreeReviewsPerDay: 0,
		},
		Ensemble: EnsembleConfig{
			Enabled:          false,
			Models:           []string{"coder32b", "llm70b"},
			ConsensusBoost:   0.10,
			RequireConsensus: false,
		},
	}
}

// Parse parses and validates a .drydock.yaml document. If data is nil or empty,
// returns Default() with no error. On invalid input, returns Default() with an
// error — the caller decides whether to abort or continue with defaults.
func Parse(data []byte) (RepoConfig, error) {
	if len(data) == 0 {
		return Default(), nil
	}

	var raw RepoConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return Default(), fmt.Errorf("parse .drydock.yaml: %w", err)
	}

	// Version
	if raw.Version == 0 {
		raw.Version = currentVersion
	}
	if raw.Version != currentVersion {
		return Default(), fmt.Errorf(".drydock.yaml: unsupported version %d (expected %d)", raw.Version, currentVersion)
	}

	// Apply defaults for missing fields.
	defaults := Default()
	if raw.Review.SeverityFloor == "" {
		raw.Review.SeverityFloor = defaults.Review.SeverityFloor
	}
	if raw.Review.DetailSeverityFloor == "" {
		raw.Review.DetailSeverityFloor = defaults.Review.DetailSeverityFloor
	}
	if raw.Context.IncludeDocs == nil {
		raw.Context.IncludeDocs = defaults.Context.IncludeDocs
	}
	if raw.Review.Walkthrough == nil {
		raw.Review.Walkthrough = defaults.Review.Walkthrough
	}
	if len(raw.Review.Statuses) == 0 {
		raw.Review.Statuses = defaults.Review.Statuses
	} else {
		normalized := make([]string, 0, len(raw.Review.Statuses))
		for _, s := range raw.Review.Statuses {
			s = strings.ToLower(strings.TrimSpace(s))
			switch s {
			case "open", "draft":
				normalized = append(normalized, s)
			case "applied", "merged", "closed":
				return Default(), fmt.Errorf(".drydock.yaml: review status %q cannot be auto-reviewed (only \"open\" and \"draft\" are allowed)", s)
			default:
				return Default(), fmt.Errorf(".drydock.yaml: invalid review status %q (allowed: \"open\", \"draft\")", s)
			}
		}
		raw.Review.Statuses = normalized
	}

	// Normalize and validate severity floors.
	raw.Review.SeverityFloor = strings.ToLower(strings.TrimSpace(raw.Review.SeverityFloor))
	if !reviewengine.IsValidSeverity(raw.Review.SeverityFloor) {
		return Default(), fmt.Errorf(".drydock.yaml: invalid severity_floor %q", raw.Review.SeverityFloor)
	}
	raw.Review.DetailSeverityFloor = strings.ToLower(strings.TrimSpace(raw.Review.DetailSeverityFloor))
	if !reviewengine.IsValidSeverity(raw.Review.DetailSeverityFloor) {
		return Default(), fmt.Errorf(".drydock.yaml: invalid detail_severity_floor %q", raw.Review.DetailSeverityFloor)
	}

	// Normalize and validate categories.
	if len(raw.Review.Categories) > 0 {
		seen := map[string]bool{}
		valid := make([]string, 0, len(raw.Review.Categories))
		for _, cat := range raw.Review.Categories {
			cat = strings.ToLower(strings.TrimSpace(cat))
			if cat == "" {
				continue
			}
			if !reviewengine.IsValidCategory(cat) {
				return Default(), fmt.Errorf(".drydock.yaml: invalid category %q", cat)
			}
			if !seen[cat] {
				seen[cat] = true
				valid = append(valid, cat)
			}
		}
		if len(valid) == 0 {
			raw.Review.Categories = nil
		} else {
			raw.Review.Categories = valid
		}
	}

	// Validate token budget.
	if raw.Context.TokenBudget < 0 {
		return Default(), fmt.Errorf(".drydock.yaml: token_budget must be >= 0, got %d", raw.Context.TokenBudget)
	}

	// Validate exclude paths.
	if len(raw.Context.ExcludePaths) > 0 {
		seen := map[string]bool{}
		valid := make([]string, 0, len(raw.Context.ExcludePaths))
		for _, p := range raw.Context.ExcludePaths {
			p = strings.TrimSpace(p)
			p = filepath.ToSlash(p) // normalize to forward slash
			if p == "" {
				continue
			}
			if filepath.IsAbs(p) {
				return Default(), fmt.Errorf(".drydock.yaml: exclude_path must be relative, got %q", p)
			}
			if strings.Contains(p, "..") {
				return Default(), fmt.Errorf(".drydock.yaml: exclude_path must not contain '..', got %q", p)
			}
			if !seen[p] {
				seen[p] = true
				valid = append(valid, p)
			}
		}
		raw.Context.ExcludePaths = valid
	}

	// Validate and default status config.
	if raw.Status.OpenSeverityFloor == "" {
		raw.Status.OpenSeverityFloor = "critical"
	}
	raw.Status.OpenSeverityFloor = strings.ToLower(strings.TrimSpace(raw.Status.OpenSeverityFloor))
	if !reviewengine.IsValidSeverity(raw.Status.OpenSeverityFloor) {
		return Default(), fmt.Errorf(".drydock.yaml: invalid status.open_severity_floor %q", raw.Status.OpenSeverityFloor)
	}
	if raw.Status.Enabled && raw.Status.MinConfidence == 0 {
		raw.Status.MinConfidence = 0.90
	}
	if raw.Status.MinConfidence < 0 || raw.Status.MinConfidence > 1 {
		return Default(), fmt.Errorf(".drydock.yaml: status.min_confidence must be in [0,1], got %f", raw.Status.MinConfidence)
	}

	// Validate and default autofix config.
	if raw.AutoFix.Enabled && raw.AutoFix.MinConfidence == 0 {
		raw.AutoFix.MinConfidence = 0.97
	}
	if raw.AutoFix.MinConfidence < 0 || raw.AutoFix.MinConfidence > 1 {
		return Default(), fmt.Errorf(".drydock.yaml: autofix.min_confidence must be in [0,1], got %f", raw.AutoFix.MinConfidence)
	}
	if raw.AutoFix.MaxFindings <= 0 {
		raw.AutoFix.MaxFindings = 3
	}

	// Validate payments config.
	if raw.Payments.Enabled {
		if raw.Payments.PriceSats <= 0 {
			return Default(), fmt.Errorf(".drydock.yaml: payments.price_sats must be > 0 when payments enabled")
		}
		if raw.Payments.FreeReviewsPerDay < 0 {
			raw.Payments.FreeReviewsPerDay = 0
		}
		// Subscription requires both fields or neither.
		hasSubPrice := raw.Payments.SubscriptionPriceSats > 0
		hasSubDays := raw.Payments.SubscriptionDays > 0
		if hasSubPrice != hasSubDays {
			return Default(), fmt.Errorf(".drydock.yaml: payments subscription requires both subscription_price_sats and subscription_days")
		}
	}

	// Validate and default ensemble config.
	if raw.Ensemble.Enabled {
		if len(raw.Ensemble.Models) == 0 {
			raw.Ensemble.Models = []string{"coder32b", "llm70b"}
		}
		for _, m := range raw.Ensemble.Models {
			switch strings.ToLower(strings.TrimSpace(m)) {
			case "coder32b", "llm70b", "coder14b":
				// valid
			default:
				return Default(), fmt.Errorf(".drydock.yaml: invalid ensemble model %q", m)
			}
		}
		if raw.Ensemble.ConsensusBoost == 0 {
			raw.Ensemble.ConsensusBoost = 0.10
		}
		if raw.Ensemble.ConsensusBoost < 0 || raw.Ensemble.ConsensusBoost > 0.5 {
			return Default(), fmt.Errorf(".drydock.yaml: ensemble.consensus_boost must be in [0,0.5], got %f", raw.Ensemble.ConsensusBoost)
		}
	}

	// Validate instructions length.
	raw.Instructions = strings.TrimSpace(raw.Instructions)
	if len(raw.Instructions) > maxInstructionBytes {
		return Default(), fmt.Errorf(".drydock.yaml: instructions exceeds %d bytes (%d)", maxInstructionBytes, len(raw.Instructions))
	}

	return raw, nil
}

// AllowsCategory returns true if the given category is allowed by this config.
// When no categories are configured, all categories are allowed.
func (c RepoConfig) AllowsCategory(category string) bool {
	if len(c.Review.Categories) == 0 {
		return true
	}
	cat := strings.ToLower(strings.TrimSpace(category))
	for _, allowed := range c.Review.Categories {
		if allowed == cat {
			return true
		}
	}
	return false
}

// AllowsSeverity returns true if the given severity is at or above the
// configured severity floor.
func (c RepoConfig) AllowsSeverity(severity string) bool {
	return reviewengine.IsAtOrAboveSeverity(severity, c.Review.SeverityFloor)
}

// WalkthroughEnabled returns true if walkthrough generation is enabled.
func (c RepoConfig) WalkthroughEnabled() bool {
	if c.Review.Walkthrough == nil {
		return true
	}
	return *c.Review.Walkthrough
}

// DocsEnabled returns true if documentation ingestion is enabled.
func (c RepoConfig) DocsEnabled() bool {
	if c.Context.IncludeDocs == nil {
		return true
	}
	return *c.Context.IncludeDocs
}

// ContainsPaymentsConfig returns true if the raw YAML data contains a top-level
// "payments:" key. Used to implement fail-closed behavior when the payments
// section is present but the config fails to parse.
func ContainsPaymentsConfig(data []byte) bool {
	for _, line := range bytes.Split(data, []byte("\n")) {
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, _, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key = strings.Trim(strings.TrimSpace(key), `"'`)
		if key == "payments" {
			return true
		}
	}
	return false
}

// ToEnsembleRoutes converts the config's model strings to reviewengine.ModelRoute.
func (c EnsembleConfig) ToEnsembleRoutes() []reviewengine.ModelRoute {
	routes := make([]reviewengine.ModelRoute, 0, len(c.Models))
	for _, m := range c.Models {
		switch strings.ToLower(strings.TrimSpace(m)) {
		case "coder32b":
			routes = append(routes, reviewengine.RouteCoder32B)
		case "llm70b":
			routes = append(routes, reviewengine.RouteLLM70B)
		case "coder14b":
			routes = append(routes, reviewengine.RouteCoder14B)
		}
	}
	return routes
}

// ToReviewEngineEnsembleConfig converts to the reviewengine's EnsembleConfig.
func (c EnsembleConfig) ToReviewEngineEnsembleConfig() reviewengine.EnsembleConfig {
	return reviewengine.EnsembleConfig{
		Enabled:          c.Enabled,
		Models:           c.ToEnsembleRoutes(),
		ConsensusBoost:   c.ConsensusBoost,
		RequireConsensus: c.RequireConsensus,
	}
}

// PromptInstructions generates the repo-policy instruction text for the LLM.
func (c RepoConfig) PromptInstructions() string {
	var parts []string
	if c.Review.SeverityFloor != "" && c.Review.SeverityFloor != "info" {
		parts = append(parts, fmt.Sprintf("Minimum severity to report: %s. Do not report findings below this level.", c.Review.SeverityFloor))
	}
	if len(c.Review.Categories) > 0 {
		parts = append(parts, fmt.Sprintf("Only report findings in these categories: %s.", strings.Join(c.Review.Categories, ", ")))
	}
	if c.Instructions != "" {
		parts = append(parts, c.Instructions)
	}
	return strings.Join(parts, "\n")
}
