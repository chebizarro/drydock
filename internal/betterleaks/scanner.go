package betterleaks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	"drydock/internal/securityscan"
)

const (
	betterleaksConfig = ".betterleaks.toml"
	gitleaksConfig    = ".gitleaks.toml"
	betterleaksBase   = ".betterleaks-baseline.json"
	gitleaksBase      = ".gitleaks-baseline.json"

	canonicalEvidence    = "Secret value redacted by betterleaks."
	canonicalDescription = "Potential secret detected by betterleaks."
	canonicalSuggestion  = "Remove the secret from source, rotate it, and use a secret manager."
)

var policyPaths = []string{
	betterleaksConfig,
	gitleaksConfig,
	betterleaksBase,
	gitleaksBase,
}

type commandScanner struct {
	runner     CommandRunner
	validation bool
}

type osCommandRunner struct{}

func (osCommandRunner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func (osCommandRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// Capture and discard stderr. Scanner diagnostics can contain matched data
	// and must never be incorporated into findings or successful output.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	return cmd.Output()
}

// New creates a PATH-backed betterleaks scanner. Validation is operator-gated
// and, when enabled, applies to every invocation.
func New(validation bool) Scanner {
	return NewWithRunner(osCommandRunner{}, validation)
}

// NewWithRunner creates a scanner with an injectable command runner.
func NewWithRunner(runner CommandRunner, validation bool) Scanner {
	if runner == nil {
		runner = osCommandRunner{}
	}
	return &commandScanner{runner: runner, validation: validation}
}

func (s *commandScanner) Scan(ctx context.Context, req ScanRequest) (ScanResult, error) {
	if err := ctx.Err(); err != nil {
		return ScanResult{}, err
	}
	if strings.TrimSpace(req.RepoPath) == "" {
		return ScanResult{}, errors.New("betterleaks: repo path is required")
	}

	repoPath, err := filepath.Abs(req.RepoPath)
	if err != nil {
		return ScanResult{}, fmt.Errorf("betterleaks: resolve repo path: %w", err)
	}
	binary, err := s.runner.LookPath(BinaryName)
	if err != nil {
		return ScanResult{}, fmt.Errorf("betterleaks: %s not found on PATH: %w", BinaryName, err)
	}

	var gitBinary string
	if req.PolicyRef != "" {
		gitBinary, err = s.runner.LookPath("git")
		if err != nil {
			return ScanResult{}, fmt.Errorf("betterleaks: git not found on PATH: %w", err)
		}
	}

	allowed, err := normalizeAllowedFiles(repoPath, req.AllowedFiles)
	if err != nil {
		return ScanResult{}, err
	}

	tempDir, err := os.MkdirTemp("", "drydock-betterleaks-")
	if err != nil {
		return ScanResult{}, fmt.Errorf("betterleaks: create private temp directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	configPath, baselinePath, err := s.materializePolicy(ctx, gitBinary, repoPath, req.PolicyRef, tempDir)
	if err != nil {
		return ScanResult{}, err
	}

	args := []string{
		"dir",
		"--redact",
		"--report-format", "json",
		"--report-path", "-",
		"--exit-code", "0",
	}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	if baselinePath != "" {
		args = append(args, "--baseline-path", baselinePath)
	}
	if s.validation {
		args = append(args, "--validation")
	}
	args = append(args, repoPath)

	output, err := s.runner.Run(ctx, tempDir, binary, args...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ScanResult{}, ctxErr
		}
		return ScanResult{}, fmt.Errorf("betterleaks: command failed: %w", err)
	}

	report, err := parseReport(output)
	if err != nil {
		return ScanResult{}, fmt.Errorf("betterleaks: parse JSON report: %w", err)
	}

	addedLines := securityscan.ParseDiffAddedLines(req.Diff)
	patchMode := req.Diff != ""
	findings := make([]securityscan.SecurityFinding, 0, len(report))
	for i, raw := range report {
		finding, err := normalizeFinding(repoPath, raw)
		if err != nil {
			return ScanResult{}, fmt.Errorf("betterleaks: finding %d: %w", i, err)
		}
		if !allowed[finding.File] {
			continue
		}
		if patchMode && !spanContainsAddedLine(addedLines[finding.File], finding.Line, finding.EndLine) {
			continue
		}
		findings = append(findings, finding)
	}

	return ScanResult{Findings: findings}, nil
}

func (s *commandScanner) materializePolicy(
	ctx context.Context,
	gitBinary, repoPath, policyRef, tempDir string,
) (configPath, baselinePath string, err error) {
	if policyRef == "" {
		return "", "", nil
	}

	args := append([]string{"ls-tree", "--name-only", policyRef, "--"}, policyPaths...)
	output, runErr := s.runner.Run(ctx, repoPath, gitBinary, args...)
	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", "", ctxErr
		}
		return "", "", fmt.Errorf("betterleaks: inspect policy ref: %w", runErr)
	}

	present := make(map[string]bool)
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSuffix(line, "\r")
		for _, fixedPath := range policyPaths {
			if line == fixedPath {
				present[fixedPath] = true
			}
		}
	}

	configName := firstPresent(present, betterleaksConfig, gitleaksConfig)
	baselineName := firstPresent(present, betterleaksBase, gitleaksBase)
	if configName != "" {
		configPath, err = s.materializeBlob(ctx, gitBinary, repoPath, policyRef, configName, tempDir)
		if err != nil {
			return "", "", err
		}
	}
	if baselineName != "" {
		baselinePath, err = s.materializeBlob(ctx, gitBinary, repoPath, policyRef, baselineName, tempDir)
		if err != nil {
			return "", "", err
		}
	}
	return configPath, baselinePath, nil
}

func (s *commandScanner) materializeBlob(
	ctx context.Context,
	gitBinary, repoPath, policyRef, name, tempDir string,
) (string, error) {
	output, err := s.runner.Run(ctx, repoPath, gitBinary, "show", policyRef+":"+name)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("betterleaks: materialize %s: %w", name, err)
	}
	path := filepath.Join(tempDir, name)
	if err := os.WriteFile(path, output, 0o600); err != nil {
		return "", fmt.Errorf("betterleaks: write materialized policy: %w", err)
	}
	return path, nil
}

func firstPresent(present map[string]bool, names ...string) string {
	for _, name := range names {
		if present[name] {
			return name
		}
	}
	return ""
}

type reportFinding struct {
	RuleID           string
	File             string
	StartLine        int
	EndLine          int
	ValidationStatus string
}

func parseReport(data []byte) ([]reportFinding, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var findings []reportFinding
	if err := decoder.Decode(&findings); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("trailing JSON data: %w", err)
	}
	return findings, nil
}

func normalizeAllowedFiles(repoPath string, files []string) (map[string]bool, error) {
	allowed := make(map[string]bool, len(files))
	for _, file := range files {
		normalized, err := normalizePath(repoPath, file)
		if err != nil {
			return nil, fmt.Errorf("betterleaks: invalid allowed file %q: %w", file, err)
		}
		allowed[normalized] = true
	}
	return allowed, nil
}

func normalizeFinding(repoPath string, raw reportFinding) (securityscan.SecurityFinding, error) {
	if raw.RuleID == "" || containsUnsafeText(raw.RuleID) {
		return securityscan.SecurityFinding{}, errors.New("invalid rule ID")
	}
	file, err := normalizePath(repoPath, raw.File)
	if err != nil {
		return securityscan.SecurityFinding{}, fmt.Errorf("invalid file path: %w", err)
	}
	if raw.StartLine <= 0 {
		return securityscan.SecurityFinding{}, errors.New("invalid start line")
	}
	endLine := raw.EndLine
	if endLine == 0 {
		endLine = raw.StartLine
	}
	if endLine < raw.StartLine {
		return securityscan.SecurityFinding{}, errors.New("end line precedes start line")
	}
	severity, confidence := SeverityForValidationStatus(raw.ValidationStatus)
	return securityscan.SecurityFinding{
		RuleID:      raw.RuleID,
		Severity:    severity,
		Category:    "security",
		File:        file,
		Line:        raw.StartLine,
		EndLine:     endLine,
		Evidence:    canonicalEvidence,
		Description: canonicalDescription,
		Suggestion:  canonicalSuggestion,
		Confidence:  confidence,
		Sensitive:   true,
	}, nil
}

func normalizePath(repoPath, reported string) (string, error) {
	if reported == "" || containsUnsafeText(reported) {
		return "", errors.New("empty or unsafe path")
	}
	candidate := reported
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(repoPath, candidate)
	}
	candidate = filepath.Clean(candidate)
	relative, err := filepath.Rel(repoPath, candidate)
	if err != nil {
		return "", err
	}
	if relative == "." || relative == ".." || filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes repository")
	}
	return filepath.ToSlash(relative), nil
}

func containsUnsafeText(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func spanContainsAddedLine(lines map[int]bool, start, end int) bool {
	for line := range lines {
		if line >= start && line <= end {
			return true
		}
	}
	return false
}
