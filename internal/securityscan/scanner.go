package securityscan

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/securityscan/surface"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

// SecurityFinding represents a single security issue found by the scanner.
type SecurityFinding struct {
	RuleID      string  `json:"rule_id"`
	Severity    string  `json:"severity"`
	Category    string  `json:"category"`
	File        string  `json:"file"`
	Line        int     `json:"line"`
	EndLine     int     `json:"end_line,omitempty"`
	Evidence    string  `json:"evidence"`
	Description string  `json:"description"`
	Suggestion  string  `json:"suggestion"`
	Confidence  float64 `json:"confidence"`
	Sensitive   bool    `json:"sensitive,omitempty"`
}

// ScanResult holds the output of a security scan.
type ScanResult struct {
	Findings     []SecurityFinding
	FilesScanned int
	FilesSkipped int
	FilesErrored int
	RulesChecked int
}

// Scanner runs deterministic security rules against source files.
type Scanner struct {
	rules        []Rule
	surfaceRules []Rule
}

// New creates a Scanner with the builtin rule set.
func New() *Scanner {
	return &Scanner{rules: BuiltinRules(), surfaceRules: SurfaceRules()}
}

// NewWithRules creates a Scanner with a custom rule set (useful for testing).
func NewWithRules(rules []Rule) *Scanner {
	return &Scanner{rules: rules}
}

// NewWithRuleSets creates a Scanner with custom finding and surface rules.
func NewWithRuleSets(rules, surfaceRules []Rule) *Scanner {
	return &Scanner{rules: rules, surfaceRules: surfaceRules}
}

// ScanFiles runs all applicable rules against the changed files in repoPath.
// Only lines added in the diff (prefixed with "+") are scanned when diffContent
// is provided, to avoid flagging pre-existing issues. If a diff is supplied but
// cannot be parsed, ScanFiles returns an error rather than an empty result, so
// an unparseable diff can never be mistaken for a clean scan.
func (s *Scanner) ScanFiles(ctx context.Context, repoPath string, changedFiles []string, diffContent string) (ScanResult, error) {
	// Parse the diff to extract added lines per file.
	addedLines, err := ParseDiffAddedLines(diffContent)
	if err != nil {
		return ScanResult{}, fmt.Errorf("scan files: %w", err)
	}
	hasDiff := diffContent != ""

	result := ScanResult{
		RulesChecked: len(s.rules),
	}

	for _, relPath := range changedFiles {
		select {
		case <-ctx.Done():
			return result, nil
		default:
		}

		absPath := filepath.Join(repoPath, relPath)

		// Determine which lines to scan.
		var lineFilter map[int]bool
		if hasDiff {
			fileLines, exists := addedLines[relPath]
			if !exists || len(fileLines) == 0 {
				// Diff is present but this file has no added lines
				// (rename-only, mode change, binary, etc.) — skip scanning
				// to avoid surfacing pre-existing issues.
				result.FilesSkipped++
				continue
			}
			lineFilter = fileLines
		}
		// If hasDiff is false (no diff provided), lineFilter stays nil → scan whole file.

		findings, err := s.scanFile(ctx, relPath, absPath, lineFilter)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				result.FilesSkipped++
			} else {
				result.FilesErrored++
			}
			continue
		}
		result.Findings = append(result.Findings, findings...)
		result.FilesScanned++
	}

	return result, nil
}

// LocateSurface finds security-relevant locations in the selected files.
// Locator matches are context only and are never returned as findings.
func (s *Scanner) LocateSurface(ctx context.Context, repoPath string, files []string) surface.Result {
	result := surface.Result{RulesChecked: len(s.surfaceRules)}
	for _, relPath := range files {
		select {
		case <-ctx.Done():
			return result
		default:
		}

		locations, err := s.scanSurfaceFile(ctx, relPath, filepath.Join(repoPath, relPath))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				result.FilesSkipped++
			} else {
				result.FilesErrored++
			}
			continue
		}
		result.Locations = append(result.Locations, locations...)
		result.FilesScanned++
	}
	return result
}

// scanFile scans a single file against all applicable rules.
// If addedLineNums is non-nil, only those line numbers are checked (diff-aware).
func (s *Scanner) scanFile(_ context.Context, relPath, absPath string, addedLineNums map[int]bool) ([]SecurityFinding, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", relPath, err)
	}
	defer f.Close()

	var findings []SecurityFinding
	scanner := bufio.NewScanner(f)
	// Increase buffer for long/minified lines (1MB max).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		// If we have diff info, only scan added lines.
		if addedLineNums != nil && !addedLineNums[lineNum] {
			continue
		}

		for _, rule := range s.rules {
			if rule.Classification == RuleClassificationSurface || !rule.appliesToFile(relPath) {
				continue
			}
			if rule.Pattern.MatchString(line) {
				// Truncate evidence to avoid bloating context.
				evidence := strings.TrimSpace(line)
				if len(evidence) > 200 {
					evidence = evidence[:200] + "..."
				}

				findings = append(findings, SecurityFinding{
					RuleID:      rule.ID,
					Severity:    rule.Severity,
					Category:    rule.Category,
					File:        relPath,
					Line:        lineNum,
					EndLine:     lineNum,
					Evidence:    evidence,
					Description: rule.Description,
					Suggestion:  rule.Suggestion,
					Confidence:  1.0, // deterministic rules have full confidence
				})
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", relPath, err)
	}
	return findings, nil
}

func (s *Scanner) scanSurfaceFile(ctx context.Context, relPath, absPath string) ([]surface.Location, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", relPath, err)
	}
	defer f.Close()

	var locations []surface.Location
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNum := 0
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return locations, ctx.Err()
		default:
		}

		lineNum++
		line := scanner.Text()
		for _, rule := range s.surfaceRules {
			if rule.Classification != RuleClassificationSurface || !rule.appliesToFile(relPath) || !rule.Pattern.MatchString(line) {
				continue
			}
			evidence := strings.TrimSpace(line)
			if len(evidence) > 200 {
				evidence = evidence[:200] + "..."
			}
			locations = append(locations, surface.Location{
				Tag:      rule.SurfaceTag,
				RuleID:   rule.ID,
				File:     relPath,
				Line:     lineNum,
				Evidence: evidence,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", relPath, err)
	}
	return locations, nil
}

// ParseDiffAddedLines extracts a map of file → {lineNumber: true} for all lines
// added in the diff, keyed by the post-image file name. Parsing goes through
// go-gitdiff — the same parser contextbuilder uses for the authoritative patch
// analysis — so securityscan agrees with the rest of the pipeline on which
// lines a patch adds. A non-empty diff that cannot be parsed is returned as an
// error, never as an empty map: callers scope their scan to added lines, so a
// silent empty result would turn an unparseable diff into a false "clean scan".
// The result can be reused by scanners that need to restrict findings to patch
// additions.
func ParseDiffAddedLines(diffContent string) (map[string]map[int]bool, error) {
	result := make(map[string]map[int]bool)
	if diffContent == "" {
		return result, nil
	}

	files, _, err := gitdiff.Parse(strings.NewReader(diffContent))
	if err != nil {
		return nil, fmt.Errorf("parse diff: %w", err)
	}

	for _, file := range files {
		if file.NewName == "" {
			continue // pure deletion or unnamed: no added lines
		}
		for _, fragment := range file.TextFragments {
			lineNum := fragment.NewPosition
			for _, line := range fragment.Lines {
				switch line.Op {
				case gitdiff.OpAdd:
					if result[file.NewName] == nil {
						result[file.NewName] = make(map[int]bool)
					}
					result[file.NewName][int(lineNum)] = true
					lineNum++
				case gitdiff.OpContext:
					lineNum++
				case gitdiff.OpDelete:
					// Removed line — does not exist in the post-image.
				}
			}
		}
	}

	return result, nil
}
