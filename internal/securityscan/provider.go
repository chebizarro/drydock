package securityscan

import (
	"context"
	"fmt"
	"strings"

	"drydock/internal/contextbuilder"
)

const LayerSecurityScan = "security-scan"

// Provider is a context builder provider that injects security scan findings
// as a high-priority layer. This ensures the LLM reviewer sees deterministic
// security findings and can reason about them alongside other context.
type Provider struct {
	scanner *Scanner
}

// NewProvider creates a context builder provider backed by the security scanner.
func NewProvider(scanner *Scanner) *Provider {
	return &Provider{scanner: scanner}
}

func (p *Provider) LayerName() string { return LayerSecurityScan }

// Priority 1 (same as patch diff) ensures security findings are never dropped
// by token budget. The layer name "security-scan" sorts after "patch-diff"
// alphabetically, so it appears immediately after the patch diff.
func (p *Provider) Priority() int { return 1 }

func (p *Provider) Build(ctx context.Context, in contextbuilder.BuildInput) (string, error) {
	if in.RepoPath == "" || in.PatchEventContent == "" {
		return "", nil
	}

	// Extract changed files from the patch.
	changedFiles := extractChangedFiles(in.PatchEventContent)
	if len(changedFiles) == 0 {
		return "", nil
	}

	result := p.scanner.ScanFiles(ctx, in.RepoPath, changedFiles, in.PatchEventContent)
	if len(result.Findings) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("SAST scanner found %d potential security issue(s) in changed files.\n", len(result.Findings)))
	b.WriteString("These are deterministic pattern matches — review each in context:\n\n")

	for _, f := range result.Findings {
		b.WriteString(fmt.Sprintf("[%s] %s | %s:%d\n", f.RuleID, f.Severity, f.File, f.Line))
		b.WriteString(fmt.Sprintf("  %s\n", f.Description))
		b.WriteString(fmt.Sprintf("  Evidence: %s\n", f.Evidence))
		if f.Suggestion != "" {
			b.WriteString(fmt.Sprintf("  Fix: %s\n", f.Suggestion))
		}
		b.WriteString("\n")
	}

	return b.String(), nil
}

// extractChangedFiles extracts file paths from a unified diff.
func extractChangedFiles(diff string) []string {
	var files []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++ b/") {
			path := strings.TrimPrefix(line, "+++ b/")
			if !seen[path] {
				seen[path] = true
				files = append(files, path)
			}
		}
	}
	return files
}
