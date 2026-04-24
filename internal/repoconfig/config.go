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
	Version      int           `yaml:"version"`
	Review       ReviewConfig  `yaml:"review"`
	Context      ContextConfig `yaml:"context"`
	Status       StatusConfig  `yaml:"status"`
	Instructions string        `yaml:"instructions"`
}

// ReviewConfig controls which findings are published.
type ReviewConfig struct {
	SeverityFloor       string   `yaml:"severity_floor"`
	Categories          []string `yaml:"categories"`
	DetailSeverityFloor string   `yaml:"detail_severity_floor"`
	Walkthrough         *bool    `yaml:"walkthrough"` // pointer to distinguish missing from false
}

// ContextConfig controls context builder behavior.
type ContextConfig struct {
	TokenBudget  int      `yaml:"token_budget"`
	ExcludePaths []string `yaml:"exclude_paths"`
	IncludeDocs  *bool    `yaml:"include_docs"` // pointer to distinguish missing from false
}

// StatusConfig controls NIP-34 review status event publication.
// Status events are opt-in; when disabled (default), Drydock only publishes
// review comments and never emits kind 1630 status events.
type StatusConfig struct {
	Enabled           bool    `yaml:"enabled"`
	OpenSeverityFloor string  `yaml:"open_severity_floor"` // findings at or above this trigger a 1630 status
	MinConfidence     float64 `yaml:"min_confidence"`       // minimum review confidence to publish status
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
		},
		Context: ContextConfig{
			IncludeDocs: &includeDocs,
		},
		Status: StatusConfig{
			Enabled:           false,
			OpenSeverityFloor: "critical",
			MinConfidence:     0.90,
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
