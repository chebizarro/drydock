package nostrscan

import (
	"context"
	"fmt"

	"git.sharegap.net/cascadia/drydock/internal/codemap"
	"git.sharegap.net/cascadia/drydock/internal/repoconfig"
	"git.sharegap.net/cascadia/drydock/internal/securityscan"
	"git.sharegap.net/cascadia/drydock/internal/securityscan/surface"
)

// ActivateInput is the scope for one Nostr security activation.
//
// Files is both the scan scope and the finding allowlist: presence, surface,
// and absence findings are all gated to it, so the PR and audit paths agree on
// which findings are in scope (previously only the audit path filtered).
// Diff, when set, makes the presence scan diff-aware. CodeMap enables absence
// analysis when non-nil and Config.AbsenceAnalysis is set.
type ActivateInput struct {
	RepoPath string
	Files    []string
	Diff     string
	CodeMap  *codemap.Map
	Profile  NostrProfile
	Config   repoconfig.NostrConfig
}

// Activate runs the deterministic Nostr lens: role/rule selection, the presence
// scan, surface location, and optional absence analysis, filtered to the
// configured roles, rules, and files. It is the single entry point shared by
// the PR (securityreview) and audit (auditengine) paths.
//
// The returned ScanResult carries the presence-scan coverage counts alongside
// the presence+absence findings; callers that track coverage read them, and
// callers that do not simply ignore them.
func Activate(ctx context.Context, in ActivateInput) (securityscan.ScanResult, surface.Result, error) {
	roles := ActivationRoles(in.Profile.Roles, in.Config)
	rules := allowedRules(PresenceRulesForRoles(roles), in.Config)
	scanner := securityscan.NewWithRuleSets(rules, SurfaceRules())

	presence, err := scanner.ScanFiles(ctx, in.RepoPath, in.Files, in.Diff)
	if err != nil {
		return securityscan.ScanResult{}, surface.Result{}, fmt.Errorf("nostrscan: presence scan: %w", err)
	}
	surfaces := scanner.LocateSurface(ctx, in.RepoPath, in.Files)

	findings := allowedFindings(presence.Findings, in.Files, roles, in.Config)
	if in.Config.AbsenceAnalysis && in.CodeMap != nil {
		absence := AnalyzeAbsences(ctx, in.RepoPath, in.CodeMap, surfaces)
		findings = append(findings, allowedFindings(absence.Findings, in.Files, roles, in.Config)...)
	}
	presence.Findings = findings
	return presence, surfaces, nil
}

// ActivationRoles resolves the effective roles from detection and operator config.
func ActivationRoles(detected []Role, cfg repoconfig.NostrConfig) []Role {
	detectedStrings := make([]string, 0, len(detected))
	for _, role := range detected {
		detectedStrings = append(detectedStrings, string(role))
	}
	configured := cfg.EffectiveRoles(detectedStrings)
	roles := make([]Role, 0, len(configured))
	for _, role := range configured {
		roles = append(roles, Role(role))
	}
	return roles
}

func allowedRules(rules []securityscan.Rule, cfg repoconfig.NostrConfig) []securityscan.Rule {
	out := make([]securityscan.Rule, 0, len(rules))
	for _, rule := range rules {
		if cfg.AllowsRule(rule.ID) {
			out = append(out, rule)
		}
	}
	return out
}

func allowedFindings(findings []securityscan.SecurityFinding, files []string, roles []Role, cfg repoconfig.NostrConfig) []securityscan.SecurityFinding {
	allowed := make(map[string]struct{}, len(files))
	for _, file := range files {
		allowed[file] = struct{}{}
	}
	out := make([]securityscan.SecurityFinding, 0, len(findings))
	for _, finding := range findings {
		if _, ok := allowed[finding.File]; !ok {
			continue
		}
		if cfg.AllowsRule(finding.RuleID) && RuleAppliesToRoles(finding.RuleID, roles) {
			out = append(out, finding)
		}
	}
	return out
}
