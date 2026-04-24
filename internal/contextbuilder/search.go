package contextbuilder

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// searcher wraps symbol callsite search, preferring ripgrep when available
// and falling back to git grep.
type searcher struct {
	hasRg     bool
	rgPath    string
	initOnce  sync.Once
}

// newSearcher creates a searcher. Call init() before first use (done lazily).
func newSearcher() *searcher {
	return &searcher{}
}

func (s *searcher) init() {
	s.initOnce.Do(func() {
		path, err := exec.LookPath("rg")
		if err == nil {
			s.hasRg = true
			s.rgPath = path
		}
	})
}

// SearchSymbol finds callsites of a symbol in the repo, returning file:line:content lines.
// When workspaceRoots are provided, only those directories are searched.
func (s *searcher) SearchSymbol(ctx context.Context, repoPath, symbol string, workspaceRoots []string) (string, error) {
	s.init()

	if s.hasRg {
		pattern := `\b` + regexp.QuoteMeta(symbol) + `\b`
		return s.rgSearch(ctx, repoPath, pattern, nil, workspaceRoots)
	}
	return s.gitGrepSymbol(ctx, repoPath, symbol, nil, workspaceRoots)
}

// SearchSymbolTests finds test-related callsites of a symbol.
// When workspaceRoots are provided, only those directories are searched.
func (s *searcher) SearchSymbolTests(ctx context.Context, repoPath, symbol string, workspaceRoots []string) (string, error) {
	s.init()

	if s.hasRg {
		pattern := `\b` + regexp.QuoteMeta(symbol) + `\b`
		return s.rgSearch(ctx, repoPath, pattern, testGlobs, workspaceRoots)
	}
	return s.gitGrepSymbol(ctx, repoPath, symbol, []string{"*test*", "*_test.go", "*.spec.*", "*.test.*"}, workspaceRoots)
}

// gitGrepSymbol searches for a symbol using git grep.
// Uses -P (Perl regex with \b) when available, falls back to plain -F (fixed string).
// When workspaceRoots are provided, pathspecs are scoped to those directories.
func (s *searcher) gitGrepSymbol(ctx context.Context, repoPath, symbol string, pathspecs []string, workspaceRoots []string) (string, error) {
	// Merge workspace roots into pathspecs for directory scoping
	effectiveSpecs := buildGitPathspecs(pathspecs, workspaceRoots)

	// Try -P (Perl regex) for word boundaries first
	args := []string{"grep", "-n", "-P", `\b` + regexp.QuoteMeta(symbol) + `\b`}
	if len(effectiveSpecs) > 0 {
		args = append(args, "--")
		args = append(args, effectiveSpecs...)
	}
	result, err := runGit(ctx, repoPath, args...)
	if err == nil {
		return result, nil
	}

	// Fall back to fixed-string search if -P not supported
	args = []string{"grep", "-n", "-F", symbol}
	if len(effectiveSpecs) > 0 {
		args = append(args, "--")
		args = append(args, effectiveSpecs...)
	}
	return runGit(ctx, repoPath, args...)
}

// buildGitPathspecs combines file-type globs with workspace directory scoping.
// If workspaceRoots is empty, returns pathspecs unchanged.
// If pathspecs is empty but workspaceRoots is set, returns workspace dirs.
// If both are set, returns workspace-scoped pathspecs.
func buildGitPathspecs(pathspecs, workspaceRoots []string) []string {
	if len(workspaceRoots) == 0 {
		return pathspecs
	}
	if len(pathspecs) == 0 {
		// Just scope to workspace directories
		specs := make([]string, len(workspaceRoots))
		for i, r := range workspaceRoots {
			specs[i] = r + "/"
		}
		return specs
	}
	// Combine: for each workspace root, apply each pathspec pattern
	var specs []string
	for _, root := range workspaceRoots {
		for _, ps := range pathspecs {
			specs = append(specs, filepath.Join(root, ps))
		}
	}
	return specs
}

var testGlobs = []string{
	"*test*",
	"*_test.go",
	"*.spec.*",
	"*.test.*",
	"*_spec.rb",
	"test_*.py",
	"*Test.java",
	"*Tests.java",
}

// rgSearch runs ripgrep with the given pattern, optionally filtering to glob patterns.
// When workspaceRoots are provided, search is scoped to those directories.
func (s *searcher) rgSearch(ctx context.Context, repoPath, pattern string, globs []string, workspaceRoots []string) (string, error) {
	args := []string{
		"--no-heading",   // file:line:content format
		"--line-number",  // include line numbers
		"--color=never",  // no ANSI codes
		"--max-count=50", // cap matches per file
		"--max-columns=200", // truncate long lines
		"-e", pattern,    // search pattern
	}

	for _, g := range globs {
		args = append(args, "--glob", g)
	}

	// Scope to workspace directories if provided
	if len(workspaceRoots) > 0 {
		for _, root := range workspaceRoots {
			args = append(args, filepath.Join(repoPath, root))
		}
	} else {
		args = append(args, repoPath)
	}

	cmd := exec.CommandContext(ctx, s.rgPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// rg exits 1 when no matches found — that's not an error
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1 {
			return "", nil
		}
		return "", fmt.Errorf("rg: %w", err)
	}

	result := strings.TrimSpace(string(out))

	// Strip the repoPath prefix from output lines for cleaner display
	if repoPath != "" && repoPath != "." {
		prefix := repoPath
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		lines := strings.Split(result, "\n")
		for i, line := range lines {
			lines[i] = strings.TrimPrefix(line, prefix)
		}
		result = strings.Join(lines, "\n")
	}

	return result, nil
}

// searchHit represents a single structured search match.
type searchHit struct {
	File string
	Line int
	Text string
}

// SearchSymbolHits returns structured search hits for a symbol, suitable for
// blast-radius analysis. Returns file-level match data instead of formatted text.
func (s *searcher) SearchSymbolHits(ctx context.Context, repoPath, symbol string, workspaceRoots []string) ([]searchHit, error) {
	raw, err := s.SearchSymbol(ctx, repoPath, symbol, workspaceRoots)
	if err != nil || raw == "" {
		return nil, err
	}
	return parseSearchHits(raw), nil
}

// parseSearchHits parses file:line:text formatted output into structured hits.
func parseSearchHits(raw string) []searchHit {
	var hits []searchHit
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: file:line:text
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 {
			continue
		}
		lineNum := 0
		if len(parts) >= 2 {
			fmt.Sscanf(parts[1], "%d", &lineNum)
		}
		text := ""
		if len(parts) >= 3 {
			text = parts[2]
		}
		hits = append(hits, searchHit{
			File: parts[0],
			Line: lineNum,
			Text: strings.TrimSpace(text),
		})
	}
	return hits
}

// UseRipgrep returns true if ripgrep is available.
func (s *searcher) UseRipgrep() bool {
	s.init()
	return s.hasRg
}
