package betterleaks

import (
	"context"

	"drydock/internal/securityscan"
)

const (
	// BinaryName is the only supported secret-scanner executable.
	BinaryName = "betterleaks"

	ValidationStatusValid = "valid"
)

// ScanRequest describes one pinned repository snapshot to scan.
//
// PolicyRef must identify the trusted commit whose root-level scanner policy is
// used. AllowedFiles is always an allowlist. Diff is optional; when present,
// findings must also overlap a line added by the diff.
type ScanRequest struct {
	RepoPath     string
	PolicyRef    string
	AllowedFiles []string
	Diff         string
}

// ScanResult contains only normalized, redacted findings.
type ScanResult struct {
	Findings []securityscan.SecurityFinding
}

// Scanner is the shared secret-scanner contract used by patch and audit paths.
type Scanner interface {
	Scan(context.Context, ScanRequest) (ScanResult, error)
}

// CommandRunner is the process seam used for PATH resolution, trusted-policy
// reads, and betterleaks execution.
type CommandRunner interface {
	LookPath(string) (string, error)
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

// SeverityForValidationStatus maps betterleaks validation state to the drydock
// severity and confidence contract. Only successfully validated credentials are
// critical; absent and every other state remain high severity.
func SeverityForValidationStatus(status string) (string, float64) {
	if status == ValidationStatusValid {
		return "critical", 0.99
	}
	return "high", 0.90
}
