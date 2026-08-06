package contextbuilder

import (
	"context"
	"fmt"
	"strings"

	"drydock/internal/securityscan/surface"
)

const LayerSecuritySurface = "security-surface"

type securitySurfaceLocator interface {
	LocateSurface(ctx context.Context, repoPath string, files []string) surface.Result
}

// SecuritySurfaceProvider emits a compact index of security-relevant locations.
type SecuritySurfaceProvider struct {
	locator securitySurfaceLocator
}

func NewSecuritySurfaceProvider(locator securitySurfaceLocator) *SecuritySurfaceProvider {
	return &SecuritySurfaceProvider{locator: locator}
}

func (p *SecuritySurfaceProvider) LayerName() string { return LayerSecuritySurface }
func (p *SecuritySurfaceProvider) Priority() int     { return 2 }

func (p *SecuritySurfaceProvider) Build(ctx context.Context, in BuildInput) (string, error) {
	if p.locator == nil || in.RepoPath == "" || in.PatchEventContent == "" {
		return "", nil
	}

	patchFiles, err := parsePatch(in.PatchEventContent)
	if err != nil {
		return "", &LayerWarning{Err: fmt.Errorf("parse patch: %w", err)}
	}
	files := make([]string, 0, len(patchFiles))
	seen := make(map[string]bool, len(patchFiles))
	for _, file := range patchFiles {
		path := pickPath(file)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		files = append(files, path)
	}
	if len(files) == 0 {
		return "", nil
	}

	result := p.locator.LocateSurface(ctx, in.RepoPath, files)
	var b strings.Builder
	if result.FilesSkipped > 0 || result.FilesErrored > 0 {
		fmt.Fprintf(&b, "SECURITY SURFACE INCOMPLETE: %d file(s) skipped, %d file(s) errored.\n", result.FilesSkipped, result.FilesErrored)
	}
	for _, location := range result.Locations {
		fmt.Fprintf(&b, "[%s] %s:%d %s\n", location.Tag, location.File, location.Line, location.Evidence)
	}
	return strings.TrimSpace(b.String()), nil
}
