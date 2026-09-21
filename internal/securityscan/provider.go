package securityscan

import (
	"context"
	"fmt"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/contextbuilder"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
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
	changedFiles, err := extractChangedFiles(in.PatchEventContent)
	if err != nil {
		// An unparseable diff must surface as a degraded layer, never as a
		// silently empty (apparently clean) scan.
		return "", &contextbuilder.LayerWarning{Err: fmt.Errorf("security scan: %w", err)}
	}
	if len(changedFiles) == 0 {
		return "", nil
	}

	result, err := p.scanner.ScanFiles(ctx, in.RepoPath, changedFiles, in.PatchEventContent)
	if err != nil {
		return "", &contextbuilder.LayerWarning{Err: err}
	}
	if len(result.Findings) == 0 && result.FilesSkipped == 0 && result.FilesErrored == 0 {
		return "", nil
	}

	var b strings.Builder
	if result.FilesSkipped > 0 || result.FilesErrored > 0 {
		b.WriteString(fmt.Sprintf("SECURITY SCAN INCOMPLETE: %d file(s) skipped and %d file(s) errored; do not treat this review as a complete security scan.\n\n", result.FilesSkipped, result.FilesErrored))
	}
	if len(result.Findings) == 0 {
		return b.String(), nil
	}
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

// extractChangedFiles extracts post-image file paths from a unified diff using
// go-gitdiff, matching the authoritative patch analysis in contextbuilder. A
// non-empty diff that cannot be parsed returns an error rather than an empty
// list, so an unparseable diff is never mistaken for "no files changed".
func extractChangedFiles(diff string) ([]string, error) {
	parsed, _, err := gitdiff.Parse(strings.NewReader(diff))
	if err != nil {
		return nil, fmt.Errorf("parse diff: %w", err)
	}
	var files []string
	seen := make(map[string]bool)
	for _, file := range parsed {
		if file.NewName == "" || seen[file.NewName] {
			continue
		}
		seen[file.NewName] = true
		files = append(files, file.NewName)
	}
	return files, nil
}
