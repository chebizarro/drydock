// Package securityreview orchestrates the PR security review lens.
package securityreview

import (
	"context"
	"fmt"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/codemap"
	"git.sharegap.net/cascadia/drydock/internal/contextbuilder"
	"git.sharegap.net/cascadia/drydock/internal/metrics"
	"git.sharegap.net/cascadia/drydock/internal/nostrscan"
	"git.sharegap.net/cascadia/drydock/internal/nostrscan/knowledge"
	"git.sharegap.net/cascadia/drydock/internal/repoconfig"
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
	"git.sharegap.net/cascadia/drydock/internal/securityscan"
	"git.sharegap.net/cascadia/drydock/internal/securityverify"
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
	deduped, err := reviewengine.DeduplicateFindings(candidates)
	if err != nil {
		result.Error = fmt.Errorf("deduplicate security findings: %w", err)
		return result
	}
	verified, err := securityverify.New(s.client, verifyCfg).Run(ctx, deduped)
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
	var codeMap *codemap.Map
	if cfg.AbsenceAnalysis {
		built, err := codemap.New().Build(ctx, repoPath, "HEAD")
		if err != nil {
			return bundle, "", nil, fmt.Errorf("securityreview: build codemap for nostr absence analysis: %w", err)
		}
		codeMap = built
	}
	scan, _, err := nostrscan.Activate(ctx, nostrscan.ActivateInput{
		RepoPath: repoPath,
		Files:    bundle.ChangedFiles,
		Diff:     extractLayer(bundle.Content, contextbuilder.LayerPatchDiff),
		CodeMap:  codeMap,
		Profile:  profile,
		Config:   cfg,
	})
	if err != nil {
		return bundle, "", nil, err
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
	return bundle, preamble, securityscan.ReviewFindings(scan.Findings), nil
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
