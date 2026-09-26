// Package repoconfig loads per-repository review configuration from .drydock.yaml.
package repoconfig

import (
	"bytes"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
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
	Security     SecurityConfig `yaml:"security"`
	Ensemble     EnsembleConfig `yaml:"ensemble"`
	Upgrades     UpgradesConfig `yaml:"upgrades"`
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
// When enabled, reviews require configured free access, repository maintainership,
// an active subscription, a Cashu token, or available free-tier quota.
type PaymentsConfig struct {
	Enabled               bool     `yaml:"enabled"`
	PriceSats             int64    `yaml:"price_sats"`              // per-review price in sats
	FreeReviewsPerDay     int      `yaml:"free_reviews_per_day"`    // free reviews per author per day
	FreePubkeys           []string `yaml:"free_pubkeys"`            // pubkeys with unlimited free reviews
	FreeForMaintainers    *bool    `yaml:"free_for_maintainers"`    // nil means enabled by default
	AcceptZaps            *bool    `yaml:"accept_zaps"`             // nil means enabled when payments are enabled
	SubscriptionPriceSats int64    `yaml:"subscription_price_sats"` // subscription price in sats
	SubscriptionDays      int      `yaml:"subscription_days"`       // subscription duration in days
}

// MaintainersAreFree reports whether repository owners and maintainers bypass payment gating.
func (c PaymentsConfig) MaintainersAreFree() bool {
	return c.FreeForMaintainers == nil || *c.FreeForMaintainers
}

// AcceptsZaps reports whether NIP-57 zap receipts may authorize reviews.
func (c PaymentsConfig) AcceptsZaps() bool {
	return c.Enabled && (c.AcceptZaps == nil || *c.AcceptZaps)
}

// SecurityConfig controls the repository security review lens and its gating policy.
type SecurityConfig struct {
	Enabled       bool                `yaml:"enabled"`
	GateSeverity  string              `yaml:"gate_severity"`
	MinConfidence float64             `yaml:"min_confidence"`
	ReviewerRoute string              `yaml:"reviewer_route"`
	ClassifyRoute string              `yaml:"classify_route"`
	VerifyVotes   int                 `yaml:"verify_votes"`
	CWETaxonomy   bool                `yaml:"cwe_taxonomy"`
	SAST          bool                `yaml:"sast"`
	Taint         bool                `yaml:"taint"`
	Surface       bool                `yaml:"surface"`
	SCA           bool                `yaml:"sca"`
	SecretScan    bool                `yaml:"secret_scan"`
	Audit         SecurityAuditConfig `yaml:"audit"`
	Nostr         NostrConfig         `yaml:"nostr"`
}

// SecurityAuditConfig controls defaults for full repository security audits.
type SecurityAuditConfig struct {
	Localizer   string `yaml:"localizer"`
	Depth       string `yaml:"depth"`
	VerifyVotes int    `yaml:"verify_votes"`
	SARIF       bool   `yaml:"sarif"`

	// AutoOnSnapshot is a deprecation shim: accepted for backward compatibility,
	// read by nothing, and slated for removal once existing .drydock.yaml files
	// have dropped the key. It must stay declared because Parse uses
	// KnownFields(true), so removing it turns a stale key into a decode error.
	// RequiresFailClosed prevents that error from silently downgrading policy,
	// but retaining the shim avoids unnecessarily stopping repositories that
	// still carry this harmless legacy key. This is a compatibility policy, not
	// a live knob.
	AutoOnSnapshot bool `yaml:"auto_on_snapshot"`
}

// NostrConfig controls the protocol-specific Nostr security lens.
type NostrConfig struct {
	Enabled             string           `yaml:"enabled"`
	MinDetectConfidence float64          `yaml:"min_detect_confidence"`
	Roles               NostrRolesConfig `yaml:"roles"`
	Rules               NostrRulesConfig `yaml:"rules"`
	KnowledgePack       bool             `yaml:"knowledge_pack"`
	AbsenceAnalysis     bool             `yaml:"absence_analysis"`
	VerifyVotes         int              `yaml:"verify_votes"`
	Probe               NostrProbeConfig `yaml:"probe"`
}

// NostrRolesConfig is either auto-detected roles or an explicit role list.
type NostrRolesConfig struct {
	Auto  bool
	Roles []string
}

// UnmarshalYAML accepts either "auto" or a sequence of Nostr roles.
func (c *NostrRolesConfig) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if strings.EqualFold(strings.TrimSpace(node.Value), "auto") {
			*c = NostrRolesConfig{Auto: true}
			return nil
		}
		return fmt.Errorf("roles must be \"auto\" or a role list")
	case yaml.SequenceNode:
		var roles []string
		if err := node.Decode(&roles); err != nil {
			return err
		}
		*c = NostrRolesConfig{Roles: roles}
		return nil
	default:
		return fmt.Errorf("roles must be \"auto\" or a role list")
	}
}

// NostrRulesConfig is either all rules, an explicit include list, or an exclude list.
type NostrRulesConfig struct {
	All     bool
	Include []string
	Exclude []string
}

// UnmarshalYAML accepts "all", a sequence of rule IDs, or {exclude: [...]}.
func (c *NostrRulesConfig) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if strings.EqualFold(strings.TrimSpace(node.Value), "all") {
			*c = NostrRulesConfig{All: true}
			return nil
		}
		return fmt.Errorf("rules must be \"all\", a rule list, or an exclude mapping")
	case yaml.SequenceNode:
		var rules []string
		if err := node.Decode(&rules); err != nil {
			return err
		}
		*c = NostrRulesConfig{Include: rules}
		return nil
	case yaml.MappingNode:
		var mapping map[string][]string
		if err := node.Decode(&mapping); err != nil {
			return err
		}
		if len(mapping) != 1 || mapping["exclude"] == nil {
			return fmt.Errorf("rules mapping may contain only exclude")
		}
		*c = NostrRulesConfig{Exclude: mapping["exclude"]}
		return nil
	default:
		return fmt.Errorf("rules must be \"all\", a rule list, or an exclude mapping")
	}
}

// NostrProbeConfig controls optional dynamic probing. AuthorizedTargets is
// decoded solely so repository-supplied values can be explicitly rejected.
type NostrProbeConfig struct {
	Enabled           bool          `yaml:"enabled"`
	Active            bool          `yaml:"active"`
	IUnderstand       bool          `yaml:"i_understand"`
	AuthorizedTargets []string      `yaml:"authorized_targets"`
	Timeout           time.Duration `yaml:"timeout"`
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

// UpgradesConfig controls automated dependency-upgrade patches: which
// vulnerable dependencies the upgrade service proposes patches for and how it
// resolves target versions. The *when* (default-branch/new-patch/schedule
// triggers) lands with the trigger stage; this block owns the *what* and *how*.
//
// AllowScripts is decoded solely so a repository can be explicitly REJECTED for
// trying to loosen it. Permitting package lifecycle scripts (npm postinstall,
// cargo build.rs, pip setup.py) means executing attacker-controlled code from the
// repository under review, so it is an operator-only, sidecar-side decision and
// must never be settable from repository config. Mirrors the
// security.nostr.probe.authorized_targets rejection precedent.
type UpgradesConfig struct {
	// Enabled gates the whole feature for the repository; defaults false
	// (fail-closed, like security).
	Enabled bool `yaml:"enabled"`
	// Triggers selects which events cause a scan for this repository. The block
	// owns the *when*; the fields below own the *what* and *how*.
	Triggers UpgradeTriggersConfig `yaml:"triggers"`
	// Policy selects the target-version resolution policy: "next_patch" (smallest
	// published version at or above the scanner's fixed version) or "latest".
	Policy string `yaml:"policy"`
	// Ecosystems limits which package ecosystems are upgraded. Allowed tokens:
	// go, npm, cargo, pip. Empty means the default set.
	Ecosystems []string `yaml:"ecosystems"`
	// Ignore lists package names that are never upgraded.
	Ignore []string `yaml:"ignore"`
	// Allow, when non-empty, restricts upgrades to exactly these package names.
	Allow []string `yaml:"allow"`
	// MaxUpgradesPerRun caps how many upgrade patches one scan may propose. Zero
	// (or absent) means the default cap; to propose none, disable the feature.
	MaxUpgradesPerRun int `yaml:"max_upgrades_per_run"`
	// AllowScripts uses a pointer so an explicit value (true or false) is
	// distinguishable from an absent key; any presence is rejected.
	AllowScripts *bool `yaml:"allow_scripts"`
}

// UpgradeTriggersConfig selects which trigger sources scan this repository.
//
// DefaultBranch is a pointer so an omitted key defaults to enabled (the cheap,
// event-driven path) while an explicit false disables reactive scanning.
//
// Schedule is a plain boolean opt-in, deliberately NOT a cadence token: the
// scan cadence is an operator concern (a single in-process ticker at
// DRYDOCK_DEPUPGRADE_INTERVAL), and a per-repository weekly/monthly token would
// be a false promise — honoring it would require either per-repo persistence or
// a wasted clone every sweep to read the in-repo cadence. This flag only opts a
// repository into the operator's periodic sweep.
//
// A new-patch (kind-1617/1618) trigger is intentionally absent: cheap manifest
// pre-detection is only possible for kind-1617 (the diff is in the content),
// so a new_patch knob would silently do nothing for pull-request events.
type UpgradeTriggersConfig struct {
	DefaultBranch *bool `yaml:"default_branch"`
	Schedule      bool  `yaml:"schedule"`
}

// DefaultBranchEnabled reports whether default-branch head-change scans run for
// this repository. An absent key defaults to enabled.
func (t UpgradeTriggersConfig) DefaultBranchEnabled() bool {
	return t.DefaultBranch == nil || *t.DefaultBranch
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
	freeForMaintainers := true
	acceptZaps := true
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
			Enabled:            false,
			FreeReviewsPerDay:  0,
			FreeForMaintainers: &freeForMaintainers,
			AcceptZaps:         &acceptZaps,
		},
		Security: SecurityConfig{
			Enabled:       false,
			GateSeverity:  "high",
			MinConfidence: 0.90,
			ReviewerRoute: "sec70b",
			ClassifyRoute: "secclassify",
			VerifyVotes:   1,
			CWETaxonomy:   true,
			SAST:          true,
			Taint:         true,
			Surface:       true,
			SCA:           false,
			SecretScan:    false,
			Audit: SecurityAuditConfig{
				Localizer:   "heuristic",
				Depth:       "standard",
				VerifyVotes: 3,
				SARIF:       true,
			},
			Nostr: NostrConfig{
				Enabled:             "auto",
				MinDetectConfidence: 0.6,
				Roles:               NostrRolesConfig{Auto: true},
				Rules:               NostrRulesConfig{All: true},
				KnowledgePack:       true,
				AbsenceAnalysis:     true,
				VerifyVotes:         2,
				Probe: NostrProbeConfig{
					Timeout: 30 * time.Second,
				},
			},
		},
		Ensemble: EnsembleConfig{
			Enabled:          false,
			Models:           []string{"coder32b", "llm70b"},
			ConsensusBoost:   0.10,
			RequireConsensus: false,
		},
		Upgrades: UpgradesConfig{
			Enabled:           false,
			Policy:            "next_patch",
			Ecosystems:        []string{"go"},
			MaxUpgradesPerRun: 5,
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
	raw.Security = Default().Security
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
	if raw.Payments.FreeForMaintainers == nil {
		raw.Payments.FreeForMaintainers = defaults.Payments.FreeForMaintainers
	}
	if raw.Payments.AcceptZaps == nil {
		raw.Payments.AcceptZaps = defaults.Payments.AcceptZaps
	}
	if len(raw.Payments.FreePubkeys) > 0 {
		seen := make(map[string]struct{}, len(raw.Payments.FreePubkeys))
		normalized := make([]string, 0, len(raw.Payments.FreePubkeys))
		for _, configured := range raw.Payments.FreePubkeys {
			pubkey := scope.NormalizePubkey(configured)
			if _, err := nostr.PubKeyFromHex(pubkey); err != nil {
				return Default(), fmt.Errorf(".drydock.yaml: invalid payments.free_pubkeys entry %q", configured)
			}
			if _, exists := seen[pubkey]; !exists {
				seen[pubkey] = struct{}{}
				normalized = append(normalized, pubkey)
			}
		}
		raw.Payments.FreePubkeys = normalized
	}
	if raw.Payments.Enabled {
		if raw.Payments.PriceSats <= 0 {
			return Default(), fmt.Errorf(".drydock.yaml: payments.price_sats must be > 0 when payments enabled")
		}
		if raw.Payments.PriceSats > math.MaxInt64/1000 {
			return Default(), fmt.Errorf(".drydock.yaml: payments.price_sats is too large")
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

	// Normalize and validate security gating fields. The security defaults are
	// installed before decoding so omitted true-valued fields remain enabled
	// while explicit false values are preserved.
	raw.Security.GateSeverity = strings.ToLower(strings.TrimSpace(raw.Security.GateSeverity))
	if !reviewengine.IsValidSeverity(raw.Security.GateSeverity) {
		return Default(), fmt.Errorf(".drydock.yaml: invalid security.gate_severity %q", raw.Security.GateSeverity)
	}
	if raw.Security.MinConfidence < 0 || raw.Security.MinConfidence > 1 {
		return Default(), fmt.Errorf(".drydock.yaml: security.min_confidence must be in [0,1], got %f", raw.Security.MinConfidence)
	}
	raw.Security.ReviewerRoute = strings.TrimSpace(raw.Security.ReviewerRoute)
	raw.Security.ClassifyRoute = strings.TrimSpace(raw.Security.ClassifyRoute)
	raw.Security.Audit.Localizer = strings.ToLower(strings.TrimSpace(raw.Security.Audit.Localizer))
	raw.Security.Audit.Depth = strings.ToLower(strings.TrimSpace(raw.Security.Audit.Depth))

	// Normalize and validate the Nostr lens. Any invalid value returns the
	// fail-closed defaults, which leave dynamic probing disabled.
	raw.Security.Nostr.Enabled = strings.ToLower(strings.TrimSpace(raw.Security.Nostr.Enabled))
	switch raw.Security.Nostr.Enabled {
	case "auto", "true", "false":
	default:
		return Default(), fmt.Errorf(".drydock.yaml: security.nostr.enabled must be auto, true, or false, got %q", raw.Security.Nostr.Enabled)
	}
	if raw.Security.Nostr.MinDetectConfidence < 0 || raw.Security.Nostr.MinDetectConfidence > 1 {
		return Default(), fmt.Errorf(".drydock.yaml: security.nostr.min_detect_confidence must be in [0,1], got %f", raw.Security.Nostr.MinDetectConfidence)
	}
	if raw.Security.Nostr.VerifyVotes < 1 {
		return Default(), fmt.Errorf(".drydock.yaml: security.nostr.verify_votes must be at least 1")
	}
	if err := validateNostrRoles(&raw.Security.Nostr.Roles); err != nil {
		return Default(), fmt.Errorf(".drydock.yaml: security.nostr.roles: %w", err)
	}
	if err := validateNostrRules(&raw.Security.Nostr.Rules); err != nil {
		return Default(), fmt.Errorf(".drydock.yaml: security.nostr.rules: %w", err)
	}
	if len(raw.Security.Nostr.Probe.AuthorizedTargets) > 0 {
		return Default(), fmt.Errorf(".drydock.yaml: security.nostr.probe.authorized_targets is operator-only and cannot be set by repository config")
	}

	// Reject repository attempts to enable package lifecycle scripts: that is an
	// operator-only, sidecar-side knob (see UpgradesConfig).
	if raw.Upgrades.AllowScripts != nil {
		return Default(), fmt.Errorf(".drydock.yaml: upgrades.allow_scripts is operator-only and cannot be set by repository config")
	}

	// Default and validate the upgrades policy block. An absent/disabled block
	// still gets defaults so downstream code never sees an empty policy.
	if raw.Upgrades.Policy == "" {
		raw.Upgrades.Policy = defaults.Upgrades.Policy
	}
	raw.Upgrades.Policy = strings.ToLower(strings.TrimSpace(raw.Upgrades.Policy))
	switch raw.Upgrades.Policy {
	case "next_patch", "latest":
	default:
		return Default(), fmt.Errorf(".drydock.yaml: invalid upgrades.policy %q (allowed: next_patch, latest)", raw.Upgrades.Policy)
	}
	if len(raw.Upgrades.Ecosystems) == 0 {
		raw.Upgrades.Ecosystems = defaults.Upgrades.Ecosystems
	} else {
		seen := map[string]bool{}
		valid := make([]string, 0, len(raw.Upgrades.Ecosystems))
		for _, e := range raw.Upgrades.Ecosystems {
			e = strings.ToLower(strings.TrimSpace(e))
			if e == "" {
				continue
			}
			switch e {
			case "go", "npm", "cargo", "pip":
			default:
				return Default(), fmt.Errorf(".drydock.yaml: invalid upgrades.ecosystem %q (allowed: go, npm, cargo, pip)", e)
			}
			if !seen[e] {
				seen[e] = true
				valid = append(valid, e)
			}
		}
		if len(valid) == 0 {
			raw.Upgrades.Ecosystems = defaults.Upgrades.Ecosystems
		} else {
			raw.Upgrades.Ecosystems = valid
		}
	}
	raw.Upgrades.Ignore = normalizeUpgradePackageList(raw.Upgrades.Ignore)
	raw.Upgrades.Allow = normalizeUpgradePackageList(raw.Upgrades.Allow)
	if raw.Upgrades.MaxUpgradesPerRun < 0 {
		return Default(), fmt.Errorf(".drydock.yaml: upgrades.max_upgrades_per_run must be >= 0, got %d", raw.Upgrades.MaxUpgradesPerRun)
	}
	if raw.Upgrades.MaxUpgradesPerRun == 0 {
		raw.Upgrades.MaxUpgradesPerRun = defaults.Upgrades.MaxUpgradesPerRun
	}
	if raw.Security.Nostr.Probe.Timeout <= 0 {
		return Default(), fmt.Errorf(".drydock.yaml: security.nostr.probe.timeout must be greater than zero")
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

// normalizeUpgradePackageList trims, drops empty entries, and de-duplicates a
// list of package names, preserving order and case (package identifiers are
// case-sensitive across ecosystems, so they are not lowercased).
func normalizeUpgradePackageList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func validateNostrRoles(config *NostrRolesConfig) error {
	if config.Auto {
		if len(config.Roles) != 0 {
			return fmt.Errorf("auto cannot be combined with explicit roles")
		}
		return nil
	}
	if len(config.Roles) == 0 {
		return fmt.Errorf("explicit role list must not be empty")
	}
	seen := make(map[string]struct{}, len(config.Roles))
	normalized := make([]string, 0, len(config.Roles))
	for _, role := range config.Roles {
		role = strings.ToLower(strings.TrimSpace(role))
		switch role {
		case "client", "relay", "signer", "library", "dvm":
		default:
			return fmt.Errorf("invalid role %q", role)
		}
		if _, ok := seen[role]; !ok {
			seen[role] = struct{}{}
			normalized = append(normalized, role)
		}
	}
	config.Roles = normalized
	return nil
}

func validateNostrRules(config *NostrRulesConfig) error {
	modes := 0
	if config.All {
		modes++
	}
	if len(config.Include) > 0 {
		modes++
	}
	if len(config.Exclude) > 0 {
		modes++
	}
	if modes != 1 {
		return fmt.Errorf("select exactly one of all, include list, or exclude list")
	}
	seen := make(map[string]struct{})
	normalize := func(values []string) ([]string, error) {
		out := make([]string, 0, len(values))
		for _, rule := range values {
			rule = strings.ToUpper(strings.TrimSpace(rule))
			if !strings.HasPrefix(rule, "NOSTR-") {
				return nil, fmt.Errorf("invalid rule id %q", rule)
			}
			if _, ok := seen[rule]; !ok {
				seen[rule] = struct{}{}
				out = append(out, rule)
			}
		}
		return out, nil
	}
	var err error
	if len(config.Include) > 0 {
		config.Include, err = normalize(config.Include)
	} else if len(config.Exclude) > 0 {
		config.Exclude, err = normalize(config.Exclude)
	}
	return err
}

// EffectiveRoles returns explicit roles when configured, otherwise detected roles.
func (c NostrConfig) EffectiveRoles(detected []string) []string {
	if !c.Roles.Auto {
		return append([]string(nil), c.Roles.Roles...)
	}
	return append([]string(nil), detected...)
}

// AllowsRule reports whether a Nostr rule is selected by repository policy.
func (c NostrConfig) AllowsRule(id string) bool {
	id = strings.ToUpper(strings.TrimSpace(id))
	if c.Rules.All {
		return true
	}
	for _, configured := range c.Rules.Include {
		if configured == id {
			return true
		}
	}
	if len(c.Rules.Exclude) > 0 {
		for _, configured := range c.Rules.Exclude {
			if configured == id {
				return false
			}
		}
		return true
	}
	return false
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

// RequiresFailClosed reports whether discarding an invalid document in favor of
// Default would risk relaxing repository policy. Review selection, status,
// payments, security, and dependency-upgrade settings all gate, suppress, or
// constrain service output, so their presence makes every parse error fatal.
//
// Context, autofix, ensemble, and instructions only tune an operation (and
// their defaults do not grant an operation that was otherwise forbidden), so a
// document containing only those known blocks may still degrade to defaults.
// Unknown top-level keys and malformed YAML fail closed because their intended
// block cannot be classified safely. Schema extensions must be added to one of
// these two sets deliberately; leaving a new key unclassified is fail-closed.
func RequiresFailClosed(data []byte) bool {
	if len(bytes.TrimSpace(data)) == 0 {
		return false
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return true
	}
	if len(document.Content) == 0 {
		return false
	}
	root := document.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return false
	}
	if root.Kind != yaml.MappingNode || len(root.Content)%2 != 0 {
		return true
	}
	for i := 0; i < len(root.Content); i += 2 {
		key := strings.TrimSpace(root.Content[i].Value)
		switch key {
		case "review", "status", "payments", "security", "upgrades":
			return true
		case "version", "context", "autofix", "ensemble", "instructions":
			// Known tuning or schema metadata: a parse failure cannot relax a
			// declared gate because this document declares none.
		default:
			return true
		}
	}
	return false
}

// ContainsPaymentsConfig returns true if the raw YAML data contains a top-level
// "payments:" key. It preserves the payment-specific protocol error while all
// other restrictive policy uses RequiresFailClosed.
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
