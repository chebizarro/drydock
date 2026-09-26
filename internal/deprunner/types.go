// Package deprunner defines the wire contract and drydock-side client for the
// dep-runner sidecar: an isolated service that runs package-manager toolchains
// (go, npm, cargo, pip) against untrusted repository manifests to produce a
// dependency-upgrade edit.
//
// The sidecar mirrors the internal/lspbridge design: a separate image behind a
// Docker Compose profile, reached over HTTP with a bearer token and a /healthz
// probe. drydock itself ships no toolchains and gains no host privilege; the
// container boundary the upgrade feature needs is the sidecar's own.
//
// This package is deliberately dependency-light so both the lean sidecar binary
// (cmd/dep-runner) and drydock can import it without pulling heavy transitive
// dependencies.
package deprunner

import (
	"fmt"
	"strings"
)

// Supported ecosystem identifiers. These match reviewengine.PackageIdentity's
// ecosystem tokens so an SCA finding maps directly onto an update request.
const (
	EcosystemGo    = "go"
	EcosystemNPM   = "npm"
	EcosystemCargo = "cargo"
	EcosystemPip   = "pip"
)

// Update result statuses.
const (
	// StatusOK means the toolchain edited at least one manifest/lockfile.
	StatusOK = "ok"
	// StatusNoChange means the command succeeded but changed nothing (e.g. the
	// requested version was already resolved).
	StatusNoChange = "no_change"
	// StatusError means the update could not be produced; see Error.
	StatusError = "error"
)

// SupportedEcosystems returns the ecosystem identifiers the sidecar handles.
func SupportedEcosystems() []string {
	return []string{EcosystemGo, EcosystemNPM, EcosystemCargo, EcosystemPip}
}

// IsSupportedEcosystem reports whether eco is a handled ecosystem.
func IsSupportedEcosystem(eco string) bool {
	switch eco {
	case EcosystemGo, EcosystemNPM, EcosystemCargo, EcosystemPip:
		return true
	default:
		return false
	}
}

// ManifestFile is a single text file supplied to, or returned from, the sidecar.
// Path is always repo-relative, forward-slash, and free of traversal; the
// sidecar rejects anything else rather than writing outside its temp root.
type ManifestFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// UpdateRequest is the body of POST /update.
//
// The caller sends only the manifest set (manifest + lockfile bytes), never the
// whole repository: the sidecar seeds a fresh temp root with these files, runs
// the ecosystem's lockfile-updating command, and returns the files the toolchain
// changed. The caller (drydock) applies the returned bytes to its real worktree
// and produces the authoritative git diff there — diff generation stays on
// drydock's existing git path rather than being hand-rolled in the sidecar.
type UpdateRequest struct {
	Ecosystem   string         `json:"ecosystem"`
	Package     string         `json:"package"`
	FromVersion string         `json:"from_version,omitempty"`
	ToVersion   string         `json:"to_version"`
	Manifests   []ManifestFile `json:"manifests"`
}

// UpdateResponse is the body of a POST /update reply.
type UpdateResponse struct {
	Status       string         `json:"status"`
	ChangedFiles []ManifestFile `json:"changed_files,omitempty"`
	Stdout       string         `json:"stdout,omitempty"`
	Stderr       string         `json:"stderr,omitempty"`
	Error        string         `json:"error,omitempty"`
	DurationMS   int64          `json:"duration_ms,omitempty"`
}

// HealthResponse is the body of GET /healthz.
type HealthResponse struct {
	Status       string   `json:"status"`
	Ecosystems   []string `json:"ecosystems,omitempty"`
	Toolchains   []string `json:"toolchains,omitempty"` // toolchain binaries present on PATH
	AllowScripts bool     `json:"allow_scripts"`        // operator opt-in to lifecycle scripts
	AuthRequired bool     `json:"auth_required"`
}

// Validate performs transport-level structural checks that do not require a
// filesystem. Filesystem confinement of manifest paths happens server-side.
func (r UpdateRequest) Validate() error {
	if !IsSupportedEcosystem(r.Ecosystem) {
		return fmt.Errorf("unsupported ecosystem %q (supported: %s)", r.Ecosystem, strings.Join(SupportedEcosystems(), ", "))
	}
	if strings.TrimSpace(r.Package) == "" {
		return fmt.Errorf("package is required")
	}
	if strings.TrimSpace(r.ToVersion) == "" {
		return fmt.Errorf("to_version is required")
	}
	if len(r.Manifests) == 0 {
		return fmt.Errorf("manifests are required")
	}
	for i, m := range r.Manifests {
		if err := validateRelPath(m.Path); err != nil {
			return fmt.Errorf("manifest %d path %q: %w", i, m.Path, err)
		}
	}
	return nil
}

// validateRelPath enforces that p is a clean, relative, traversal-free path.
// Duplicated here (rather than importing internal/safepath) so this wire package
// stays dependency-light for the sidecar binary.
func validateRelPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("path must not be empty")
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return fmt.Errorf("path must be relative")
	}
	// Windows drive-letter absolute paths (e.g. C:\...).
	if len(p) >= 2 && p[1] == ':' {
		return fmt.Errorf("path must be relative")
	}
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return fmt.Errorf("path must not contain traversal")
		}
	}
	return nil
}
