// Package securityreview orchestrates the PR security review lens.
package securityreview

import (
	"context"
	"fmt"
	"strings"

	"drydock/internal/contextbuilder"
	"drydock/internal/metrics"
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
	Evidence SecurityEvidence
	Findings []reviewengine.Finding
	Error    error
}

// Stage runs security review, adversarial verification, and classification.
type Stage struct {
	reviewer         *reviewengine.Engine
	client           reviewengine.LLMClient
	verifyEndpoint   reviewengine.ModelEndpoint
	classifyEndpoint reviewengine.ModelEndpoint
}

// New constructs a security review stage.
func New(
	reviewer *reviewengine.Engine,
	client reviewengine.LLMClient,
	verifyEndpoint reviewengine.ModelEndpoint,
	classifyEndpoint reviewengine.ModelEndpoint,
) *Stage {
	return &Stage{
		reviewer:         reviewer,
		client:           client,
		verifyEndpoint:   verifyEndpoint,
		classifyEndpoint: classifyEndpoint,
	}
}

// Run executes the PR security lens for an already-built context bundle.
func (s *Stage) Run(ctx context.Context, bundle contextbuilder.ContextBundle, repoPath string, cfg repoconfig.SecurityConfig) SecurityResult {
	evidence := ExtractEvidence(bundle, cfg)
	result := SecurityResult{Evidence: evidence}
	if s == nil || s.reviewer == nil {
		result.Error = fmt.Errorf("securityreview: nil reviewer")
		return result
	}
	if strings.TrimSpace(repoPath) == "" {
		result.Error = fmt.Errorf("securityreview: empty repo path")
		return result
	}

	route := reviewengine.ModelRoute(strings.TrimSpace(cfg.ReviewerRoute))
	if route == "" {
		route = reviewengine.RouteSec70B
	}
	review, err := s.reviewer.Run(ctx, reviewengine.RunInput{
		ContextBundle:                bundle.Content,
		ChangedFiles:                 bundle.ChangedFiles,
		ReviewerRoute:                route,
		ReviewerSystemPromptOverride: securityReviewerSystemPrompt(),
		SkipWalkthrough:              true,
	})
	if err != nil {
		result.Error = fmt.Errorf("security reviewer: %w", err)
		return result
	}

	packet := evidence.prompt()
	candidates := make([]reviewengine.Finding, 0, len(review.Review.Findings))
	for _, finding := range review.Review.Findings {
		finding.Category = "security"
		if packet != "" {
			finding.Evidence += "\n\nSecurity evidence packet:\n" + packet
		}
		candidates = append(candidates, finding)
	}

	verifyCfg := securityverify.Config{
		VerifyVotes:      cfg.VerifyVotes,
		VerifyEndpoint:   s.verifyEndpoint,
		ClassifyEndpoint: s.classifyEndpoint,
	}
	verified, err := securityverify.New(s.client, verifyCfg).Run(ctx, candidates)
	if err != nil {
		result.Error = fmt.Errorf("security verify: %w", err)
		return result
	}

	for i := range verified {
		cwe := strings.ToUpper(strings.TrimSpace(verified[i].Category))
		metrics.SecurityFindings.With(cwe, verified[i].Severity).Inc()
		verified[i].Evidence, _, _ = strings.Cut(verified[i].Evidence, "\n\nSecurity evidence packet:\n")
		if strings.HasPrefix(cwe, "CWE-") {
			verified[i].Evidence = strings.TrimSpace("[" + cwe + "] " + verified[i].Evidence)
		}
		verified[i].Category = "security"
	}
	result.Findings = verified
	return result
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
