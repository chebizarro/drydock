// Package securityreview orchestrates the PR security review lens.
package securityreview

import (
	"context"
	"fmt"
	"strings"

	"drydock/internal/codemap"
	"drydock/internal/contextbuilder"
	"drydock/internal/metrics"
	"drydock/internal/nostrscan"
	"drydock/internal/nostrscan/knowledge"
	"drydock/internal/repoconfig"
	"drydock/internal/reviewengine"
	"drydock/internal/securityscan"
	"drydock/internal/securityverify"
)

// SecurityEvidence is the structured security-specific subset of a context bundle.
type SecurityEvidence struct {
	SAST            string
	TaintPaths      string
	SecuritySurface string
}

// SecurityResult is the output of a security review stage run.
type SecurityResult struct {
	Evidence    SecurityEvidence
	Findings    []reviewengine.Finding
	NostrActive bool
	Error       error
}

// Option configures a security review stage.
type Option func(*Stage)

// WithNostrEnabled applies the operator-level Nostr lens mode.
func WithNostrEnabled(mode string) Option {
	return func(s *Stage) {
		mode = strings.ToLower(strings.TrimSpace(mode))
		switch mode {
		case "auto", "true", "false":
			s.nostrEnabled = mode
		default:
			s.nostrEnabled = "false"
		}
	}
}

// Stage runs security review, adversarial verification, and classification.
type Stage struct {
	reviewer         *reviewengine.Engine
	client           reviewengine.LLMClient
	verifyEndpoint   reviewengine.ModelEndpoint
	classifyEndpoint reviewengine.ModelEndpoint
	nostrEnabled     string
}

// New constructs a security review stage.
func New(
	reviewer *reviewengine.Engine,
	client reviewengine.LLMClient,
	verifyEndpoint reviewengine.ModelEndpoint,
	classifyEndpoint reviewengine.ModelEndpoint,
	opts ...Option,
) *Stage {
	stage := &Stage{
		reviewer:         reviewer,
		client:           client,
		verifyEndpoint:   verifyEndpoint,
		classifyEndpoint: classifyEndpoint,
		nostrEnabled:     "auto",
	}
	for _, opt := range opts {
		opt(stage)
	}
	return stage
}

// Run executes the PR security lens for an already-built context bundle.
func (s *Stage) Run(ctx context.Context, bundle contextbuilder.ContextBundle, repoPath string, cfg repoconfig.SecurityConfig) SecurityResult {
	result := SecurityResult{}
	if strings.TrimSpace(repoPath) == "" {
		result.Error = fmt.Errorf("securityreview: empty repo path")
		return result
	}

	profile, nostrActive, err := s.detectNostr(ctx, repoPath, cfg.Nostr)
	if err != nil {
		result.Error = err
		return result
	}
	result.NostrActive = nostrActive
	if !cfg.Enabled && !nostrActive {
		return result
	}
	if s == nil || s.reviewer == nil {
		result.Error = fmt.Errorf("securityreview: nil reviewer")
		return result
	}

	var nostrPreamble string
	var nostrCandidates []reviewengine.Finding
	if nostrActive {
		bundle, nostrPreamble, nostrCandidates, err = activateNostr(ctx, bundle, repoPath, profile, cfg.Nostr)
		if err != nil {
			result.Error = err
			return result
		}
	}

	evidence := ExtractEvidence(bundle, cfg)
	result.Evidence = evidence
	route := reviewengine.ModelRoute(strings.TrimSpace(cfg.ReviewerRoute))
	if route == "" {
		route = reviewengine.RouteSec70B
	}
	systemPrompt := securityReviewerSystemPrompt()
	if nostrPreamble != "" {
		systemPrompt += "\n\n" + nostrPreamble
	}
	review, err := s.reviewer.Run(ctx, reviewengine.RunInput{
		ContextBundle:                bundle.Content,
		ChangedFiles:                 bundle.ChangedFiles,
		ReviewerRoute:                route,
		ReviewerSystemPromptOverride: systemPrompt,
		SkipWalkthrough:              true,
	})
	if err != nil {
		result.Error = fmt.Errorf("security reviewer: %w", err)
		return result
	}

	packet := evidence.prompt()
	candidates := append([]reviewengine.Finding(nil), nostrCandidates...)
	for _, finding := range review.Review.Findings {
		finding.Category = "security"
		if packet != "" {
			finding.Evidence += "\n\nSecurity evidence packet:\n" + packet
		}
		candidates = append(candidates, finding)
	}

	verifyVotes := cfg.VerifyVotes
	if nostrActive && cfg.Nostr.VerifyVotes > verifyVotes {
		verifyVotes = cfg.Nostr.VerifyVotes
	}
	verifyCfg := securityverify.Config{
		VerifyVotes:      verifyVotes,
		VerifyEndpoint:   s.verifyEndpoint,
		ClassifyEndpoint: s.classifyEndpoint,
	}
	verified, err := securityverify.New(s.client, verifyCfg).Run(ctx, reviewengine.DeduplicateFindings(candidates))
	if err != nil {
		result.Error = fmt.Errorf("security verify: %w", err)
		return result
	}

	for i := range verified {
		cwe := strings.ToUpper(strings.TrimSpace(verified[i].Category))
		metrics.SecurityFindings.With(cwe, verified[i].Severity).Inc()
		verified[i].Evidence, _, _ = strings.Cut(verified[i].Evidence, "\n\nSecurity evidence packet:\n")
		if strings.HasPrefix(cwe, "CWE-") && !strings.Contains(verified[i].Evidence, "["+cwe+"]") {
			verified[i].Evidence = strings.TrimSpace("[" + cwe + "] " + verified[i].Evidence)
		}
		verified[i].Category = "security"
	}
	result.Findings = verified
	return result
}

func (s *Stage) detectNostr(ctx context.Context, repoPath string, cfg repoconfig.NostrConfig) (nostrscan.NostrProfile, bool, error) {
	if s == nil || s.nostrEnabled == "false" || cfg.Enabled == "false" || cfg.Enabled == "" {
		return nostrscan.NostrProfile{}, false, nil
	}
	profile, err := nostrscan.Detect(ctx, repoPath, "HEAD", nostrscan.WithMinConfidence(cfg.MinDetectConfidence))
	if err != nil {
		return nostrscan.NostrProfile{}, false, fmt.Errorf("securityreview: detect nostr project: %w", err)
	}
	return profile, profile.IsNostr, nil
}

func activateNostr(ctx context.Context, bundle contextbuilder.ContextBundle, repoPath string, profile nostrscan.NostrProfile, cfg repoconfig.NostrConfig) (contextbuilder.ContextBundle, string, []reviewengine.Finding, error) {
	roles := effectiveNostrRoles(profile.Roles, cfg)
	rules := filterNostrRules(nostrscan.PresenceRulesForRoles(roles), cfg)
	scanner := securityscan.NewWithRuleSets(rules, nostrscan.SurfaceRules())
	diff := extractLayer(bundle.Content, contextbuilder.LayerPatchDiff)
	scan := scanner.ScanFiles(ctx, repoPath, bundle.ChangedFiles, diff)
	findings := filterNostrFindings(scan.Findings, roles, cfg)

	if cfg.AbsenceAnalysis {
		codeMap, err := codemap.New().Build(ctx, repoPath, "HEAD")
		if err != nil {
			return bundle, "", nil, fmt.Errorf("securityreview: build codemap for nostr absence analysis: %w", err)
		}
		allFiles := make([]string, 0, len(codeMap.Files))
		for file := range codeMap.Files {
			allFiles = append(allFiles, file)
		}
		surfaces := scanner.LocateSurface(ctx, repoPath, allFiles)
		absence := nostrscan.AnalyzeAbsences(ctx, repoPath, codeMap, surfaces)
		findings = append(findings, filterNostrFindings(absence.Findings, roles, cfg)...)
	}

	preamble := ""
	if cfg.KnowledgePack {
		contextLayer, err := knowledge.Context()
		if err != nil {
			return bundle, "", nil, fmt.Errorf("securityreview: load nostr knowledge context: %w", err)
		}
		preamble, err = knowledge.ReviewerSystemPreamble()
		if err != nil {
			return bundle, "", nil, fmt.Errorf("securityreview: load nostr reviewer preamble: %w", err)
		}
		bundle.Content = strings.TrimSpace(bundle.Content) + "\n\n## nostr-protocol\n" + contextLayer
		bundle.LayersUsed = append(bundle.LayersUsed, "nostr-protocol")
	}
	return bundle, preamble, nostrReviewFindings(findings), nil
}

func effectiveNostrRoles(detected []nostrscan.Role, cfg repoconfig.NostrConfig) []nostrscan.Role {
	detectedStrings := make([]string, 0, len(detected))
	for _, role := range detected {
		detectedStrings = append(detectedStrings, string(role))
	}
	configured := cfg.EffectiveRoles(detectedStrings)
	roles := make([]nostrscan.Role, 0, len(configured))
	for _, role := range configured {
		roles = append(roles, nostrscan.Role(role))
	}
	return roles
}

func filterNostrRules(rules []securityscan.Rule, cfg repoconfig.NostrConfig) []securityscan.Rule {
	out := make([]securityscan.Rule, 0, len(rules))
	for _, rule := range rules {
		if cfg.AllowsRule(rule.ID) {
			out = append(out, rule)
		}
	}
	return out
}

func filterNostrFindings(findings []securityscan.SecurityFinding, roles []nostrscan.Role, cfg repoconfig.NostrConfig) []securityscan.SecurityFinding {
	out := make([]securityscan.SecurityFinding, 0, len(findings))
	for _, finding := range findings {
		if cfg.AllowsRule(finding.RuleID) && nostrscan.RuleAppliesToRoles(finding.RuleID, roles) {
			out = append(out, finding)
		}
	}
	return out
}

func nostrReviewFindings(findings []securityscan.SecurityFinding) []reviewengine.Finding {
	out := make([]reviewengine.Finding, 0, len(findings))
	for _, finding := range findings {
		cwe := securityscan.SASTRuleCWE[finding.RuleID]
		evidence := "[" + finding.RuleID + "] " + finding.Evidence
		if cwe != "" {
			evidence = "[" + cwe + "] " + evidence
		}
		out = append(out, reviewengine.Finding{
			Severity: finding.Severity, Category: "security", File: finding.File, Line: finding.Line,
			Evidence: evidence, Explanation: finding.Description, Suggestion: finding.Suggestion,
			Confidence: finding.Confidence,
		})
	}
	return out
}

// ExtractEvidence extracts security provider layers from the rendered bundle.
func ExtractEvidence(bundle contextbuilder.ContextBundle, cfg repoconfig.SecurityConfig) SecurityEvidence {
	var evidence SecurityEvidence
	if cfg.SAST {
		evidence.SAST = extractLayer(bundle.Content, securityscan.LayerSecurityScan)
	}
	if cfg.Taint {
		evidence.TaintPaths = extractLayer(bundle.Content, contextbuilder.LayerTaint)
	}
	if cfg.Surface {
		evidence.SecuritySurface = extractLayer(bundle.Content, contextbuilder.LayerSecuritySurface)
	}
	return evidence
}

func extractLayer(content, name string) string {
	header := "## " + name
	start := strings.Index(content, header)
	if start < 0 {
		return ""
	}
	start += len(header)
	if start < len(content) && content[start] == '\n' {
		start++
	}
	end := strings.Index(content[start:], "\n\n## ")
	if end < 0 {
		return strings.TrimSpace(content[start:])
	}
	return strings.TrimSpace(content[start : start+end])
}

func (e SecurityEvidence) prompt() string {
	var parts []string
	if e.SAST != "" {
		parts = append(parts, "SAST:\n"+e.SAST)
	}
	if e.TaintPaths != "" {
		parts = append(parts, "Taint paths:\n"+e.TaintPaths)
	}
	if e.SecuritySurface != "" {
		parts = append(parts, "Security surface:\n"+e.SecuritySurface)
	}
	return strings.Join(parts, "\n\n")
}

func securityReviewerSystemPrompt() string {
	return reviewengine.DefaultReviewerSystemPrompt() + `

You are performing a dedicated security review. Trace every attacker-controlled source to its sink, enumerate each trust boundary crossed, and require concrete reachability evidence. Map every candidate finding to the most specific applicable CWE. Report only security findings and set category to "security".`
}
