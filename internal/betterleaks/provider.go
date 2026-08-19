package betterleaks

import (
	"context"
	"fmt"
	"strings"

	"drydock/internal/contextbuilder"
)

const LayerSecretScan = "secret-scan"

// Provider injects a previously rendered betterleaks result into review
// context. It deliberately does not execute a scan.
type Provider struct{}

func NewProvider() *Provider { return &Provider{} }

func (*Provider) LayerName() string { return LayerSecretScan }
func (*Provider) Priority() int     { return 1 }

func (*Provider) Build(ctx context.Context, in contextbuilder.BuildInput) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return in.SecretScanContext, nil
}

// FormatContext renders only canonical redacted text. It never uses finding
// evidence, descriptions, suggestions, or any raw scanner report fields.
func FormatContext(result ScanResult) string {
	if len(result.Findings) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "BETTERLEAKS SECRET SCAN: %d potential secret(s) detected.\n", len(result.Findings))
	b.WriteString("Secret values are redacted. Treat every finding as sensitive and rotate exposed credentials.\n\n")
	for _, finding := range result.Findings {
		endLine := finding.EndLine
		if endLine <= 0 {
			endLine = finding.Line
		}
		location := fmt.Sprintf("%s:%d", finding.File, finding.Line)
		if endLine != finding.Line {
			location = fmt.Sprintf("%s:%d-%d", finding.File, finding.Line, endLine)
		}
		fmt.Fprintf(&b, "[%s] %s | %s\n", finding.RuleID, finding.Severity, location)
		b.WriteString("  Potential secret detected; value redacted. Remove it from source and rotate the credential.\n\n")
	}
	return b.String()
}
