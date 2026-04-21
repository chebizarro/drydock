package contextbuilder

import (
	"context"
	"fmt"
	"os/exec"
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
func (s *searcher) SearchSymbol(ctx context.Context, repoPath, symbol string) (string, error) {
	s.init()

	if s.hasRg {
		pattern := `\b` + regexp.QuoteMeta(symbol) + `\b`
		return s.rgSearch(ctx, repoPath, pattern, nil)
	}
	return s.gitGrepSymbol(ctx, repoPath, symbol, nil)
}

// SearchSymbolTests finds test-related callsites of a symbol.
func (s *searcher) SearchSymbolTests(ctx context.Context, repoPath, symbol string) (string, error) {
	s.init()

	if s.hasRg {
		pattern := `\b` + regexp.QuoteMeta(symbol) + `\b`
		return s.rgSearch(ctx, repoPath, pattern, testGlobs)
	}
	return s.gitGrepSymbol(ctx, repoPath, symbol, []string{"*test*", "*_test.go", "*.spec.*", "*.test.*"})
}

// gitGrepSymbol searches for a symbol using git grep.
// Uses -P (Perl regex with \b) when available, falls back to plain -F (fixed string).
func (s *searcher) gitGrepSymbol(ctx context.Context, repoPath, symbol string, pathspecs []string) (string, error) {
	// Try -P (Perl regex) for word boundaries first
	args := []string{"grep", "-n", "-P", `\b` + regexp.QuoteMeta(symbol) + `\b`}
	if len(pathspecs) > 0 {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}
	result, err := runGit(ctx, repoPath, args...)
	if err == nil {
		return result, nil
	}

	// Fall back to fixed-string search if -P not supported
	args = []string{"grep", "-n", "-F", symbol}
	if len(pathspecs) > 0 {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}
	return runGit(ctx, repoPath, args...)
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
func (s *searcher) rgSearch(ctx context.Context, repoPath, pattern string, globs []string) (string, error) {
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

	args = append(args, repoPath)

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

// UseRipgrep returns true if ripgrep is available.
func (s *searcher) UseRipgrep() bool {
	s.init()
	return s.hasRg
}
