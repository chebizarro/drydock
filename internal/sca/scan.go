package sca

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

// Runner runs an SCA tool binary and looks it up on PATH. It matches
// nostrprobe.ToolRunner (the auditengine Dependencies.Tools type) structurally,
// so the audit engine passes its existing runner through unchanged. The
// repository path is supplied as a command argument, not as a working directory.
type Runner interface {
	LookPath(string) (string, error)
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// scaTool describes one SCA scanner and how to invoke it against a repo path.
// All three emit SARIF 2.1.0, whose fixed schema keeps the file path and the
// message in one result object — unlike their bespoke JSON, where they live in
// different objects (DRYDOCK-rnmo).
var scaTools = []struct {
	names []string
	args  func(repoPath string) []string
}{
	{[]string{"trivy"}, func(repo string) []string { return []string{"fs", "--format", "sarif", repo} }},
	{[]string{"grype"}, func(repo string) []string { return []string{"dir:" + repo, "-o", "sarif"} }},
	{[]string{"osv-scanner", "osv"}, func(repo string) []string { return []string{"--format", "sarif", "-r", repo} }},
}

// Scan runs every available SCA tool against repoPath and returns the union of
// their findings. A tool that is not installed is logged and skipped (SCA is
// best-effort infrastructure); a tool that runs but fails is logged and skipped;
// only a genuinely malformed SARIF payload from a tool that succeeded aborts the
// scan, since that indicates a contract break rather than a missing scanner.
func Scan(ctx context.Context, runner Runner, repoPath string, logger *slog.Logger) ([]reviewengine.Finding, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if runner == nil {
		return nil, fmt.Errorf("sca scan: runner is required")
	}
	var findings []reviewengine.Finding
	for _, tool := range scaTools {
		name, ok := availableTool(runner, tool.names...)
		if !ok {
			logger.Info("optional security scanner unavailable; skipping", "tools", tool.names)
			continue
		}
		out, err := runner.Run(ctx, name, tool.args(repoPath)...)
		if err != nil {
			logger.Warn("optional security scanner failed", "tool", name, "error", err)
			continue
		}
		parsed, parseErr := ParseSARIFFindings(name, out)
		if parseErr != nil {
			return nil, fmt.Errorf("parse %s findings: %w", name, parseErr)
		}
		findings = append(findings, parsed...)
	}
	return findings, nil
}

// Scanner binds a Runner and logger so it can expose the repoPath-only Scan
// signature the dependency-upgrade service depends on. It owns the runner/logger
// binding rather than forcing every caller to thread them through.
type Scanner struct {
	runner Runner
	logger *slog.Logger
}

// NewScanner returns a Scanner over the given tool runner.
func NewScanner(runner Runner, logger *slog.Logger) *Scanner {
	return &Scanner{runner: runner, logger: logger}
}

// Scan runs every available SCA tool against repoPath.
func (s *Scanner) Scan(ctx context.Context, repoPath string) ([]reviewengine.Finding, error) {
	return Scan(ctx, s.runner, repoPath, s.logger)
}

func availableTool(runner Runner, names ...string) (string, bool) {
	for _, name := range names {
		if _, err := runner.LookPath(name); err == nil {
			return name, true
		}
	}
	return "", false
}
