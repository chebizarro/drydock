package contextbuilder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Workspace represents a detected package/module boundary within a repo.
type Workspace struct {
	Root string // relative path from repo root (e.g. "packages/auth")
	Type string // "npm", "pnpm", "cargo", "go-work", "lerna"
}

// DetectWorkspaces scans the repo for workspace configuration files
// and returns all resolved workspace directories. Returns nil if no
// workspace config is found (single-package repo).
func DetectWorkspaces(repoPath string) []Workspace {
	var workspaces []Workspace

	// npm/yarn: package.json with "workspaces" field
	workspaces = append(workspaces, detectNPMWorkspaces(repoPath)...)

	// pnpm: pnpm-workspace.yaml
	workspaces = append(workspaces, detectPNPMWorkspaces(repoPath)...)

	// Cargo: Cargo.toml with [workspace] members
	workspaces = append(workspaces, detectCargoWorkspaces(repoPath)...)

	// Go: go.work with use directives
	workspaces = append(workspaces, detectGoWorkspaces(repoPath)...)

	// Lerna: lerna.json with packages
	workspaces = append(workspaces, detectLernaWorkspaces(repoPath)...)

	return workspaces
}

// RelevantWorkspaces returns the workspace roots that contain at least one
// of the changed files. Returns nil if no workspaces match (callers should
// treat this as "search entire repo").
func RelevantWorkspaces(workspaces []Workspace, changedFiles []string) []string {
	if len(workspaces) == 0 || len(changedFiles) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var roots []string

	for _, f := range changedFiles {
		for _, ws := range workspaces {
			if pathIsUnder(ws.Root, f) && !seen[ws.Root] {
				seen[ws.Root] = true
				roots = append(roots, ws.Root)
			}
		}
	}
	return roots
}

// pathIsUnder returns true if filePath is inside dir (both relative, forward-slash).
func pathIsUnder(dir, filePath string) bool {
	if dir == "" || dir == "." {
		return true
	}
	d := filepath.ToSlash(dir)
	f := filepath.ToSlash(filePath)
	if !strings.HasSuffix(d, "/") {
		d += "/"
	}
	return strings.HasPrefix(f, d)
}

// --- npm/yarn workspaces ---

func detectNPMWorkspaces(repoPath string) []Workspace {
	data, err := os.ReadFile(filepath.Join(repoPath, "package.json"))
	if err != nil {
		return nil
	}

	var pkg struct {
		Workspaces interface{} `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil || pkg.Workspaces == nil {
		return nil
	}

	var globs []string
	switch v := pkg.Workspaces.(type) {
	case []interface{}:
		// "workspaces": ["packages/*", "apps/*"]
		for _, g := range v {
			if s, ok := g.(string); ok {
				globs = append(globs, s)
			}
		}
	case map[string]interface{}:
		// yarn v1 object: {"packages": ["packages/*"]}
		if pkgs, ok := v["packages"].([]interface{}); ok {
			for _, g := range pkgs {
				if s, ok := g.(string); ok {
					globs = append(globs, s)
				}
			}
		}
	}

	return expandGlobs(repoPath, globs, "npm")
}

// --- pnpm workspaces ---

func detectPNPMWorkspaces(repoPath string) []Workspace {
	data, err := os.ReadFile(filepath.Join(repoPath, "pnpm-workspace.yaml"))
	if err != nil {
		return nil
	}

	// Simple YAML parsing for packages list — avoids a YAML dependency.
	// Format:
	//   packages:
	//     - 'packages/*'
	//     - 'apps/*'
	var globs []string
	inPackages := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "packages:" {
			inPackages = true
			continue
		}
		if inPackages {
			if strings.HasPrefix(trimmed, "- ") {
				g := strings.TrimPrefix(trimmed, "- ")
				g = strings.Trim(g, "'\"")
				globs = append(globs, g)
			} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				break // next key
			}
		}
	}

	return expandGlobs(repoPath, globs, "pnpm")
}

// --- Cargo workspaces ---

func detectCargoWorkspaces(repoPath string) []Workspace {
	data, err := os.ReadFile(filepath.Join(repoPath, "Cargo.toml"))
	if err != nil {
		return nil
	}

	content := string(data)
	// Simple parsing: find [workspace] section, extract members = [...]
	wsIdx := strings.Index(content, "[workspace]")
	if wsIdx < 0 {
		return nil
	}

	section := content[wsIdx:]
	// Find members = [...]
	re := regexp.MustCompile(`(?m)^members\s*=\s*\[([^\]]*)\]`)
	match := re.FindStringSubmatch(section)
	if len(match) < 2 {
		return nil
	}

	var globs []string
	for _, item := range strings.Split(match[1], ",") {
		g := strings.TrimSpace(item)
		g = strings.Trim(g, "\"'")
		if g != "" {
			globs = append(globs, g)
		}
	}

	return expandGlobs(repoPath, globs, "cargo")
}

// --- Go workspaces ---

var goWorkUseRe = regexp.MustCompile(`(?m)^\s*\./([^\s)]+)`)

func detectGoWorkspaces(repoPath string) []Workspace {
	data, err := os.ReadFile(filepath.Join(repoPath, "go.work"))
	if err != nil {
		return nil
	}

	content := string(data)
	// Find use ( ... ) block
	useIdx := strings.Index(content, "use (")
	if useIdx < 0 {
		// Also handle single-line: use ./cmd/server
		useLineRe := regexp.MustCompile(`(?m)^use\s+\./(\S+)`)
		if m := useLineRe.FindStringSubmatch(content); len(m) > 1 {
			dir := m[1]
			full := filepath.Join(repoPath, dir)
			if info, err := os.Stat(full); err == nil && info.IsDir() {
				return []Workspace{{Root: dir, Type: "go-work"}}
			}
		}
		return nil
	}

	block := content[useIdx:]
	closeIdx := strings.Index(block, ")")
	if closeIdx > 0 {
		block = block[:closeIdx]
	}

	var workspaces []Workspace
	for _, match := range goWorkUseRe.FindAllStringSubmatch(block, -1) {
		if len(match) < 2 {
			continue
		}
		dir := match[1]
		full := filepath.Join(repoPath, dir)
		if info, err := os.Stat(full); err == nil && info.IsDir() {
			workspaces = append(workspaces, Workspace{Root: dir, Type: "go-work"})
		}
	}
	return workspaces
}

// --- Lerna ---

func detectLernaWorkspaces(repoPath string) []Workspace {
	data, err := os.ReadFile(filepath.Join(repoPath, "lerna.json"))
	if err != nil {
		return nil
	}

	var cfg struct {
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil || len(cfg.Packages) == 0 {
		return nil
	}

	return expandGlobs(repoPath, cfg.Packages, "lerna")
}

// --- helpers ---

// expandGlobs resolves glob patterns (like "packages/*") against the repo
// filesystem and returns workspaces for directories that actually exist.
func expandGlobs(repoPath string, globs []string, wsType string) []Workspace {
	var workspaces []Workspace
	seen := make(map[string]bool)

	for _, g := range globs {
		pattern := filepath.Join(repoPath, g)
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil || !info.IsDir() {
				continue
			}
			rel, err := filepath.Rel(repoPath, m)
			if err != nil || seen[rel] {
				continue
			}
			seen[rel] = true
			workspaces = append(workspaces, Workspace{Root: rel, Type: wsType})
		}
	}
	return workspaces
}
