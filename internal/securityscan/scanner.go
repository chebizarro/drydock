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
)

// SecurityFinding represents a single security issue found by the scanner.
type SecurityFinding struct {
	RuleID      string  `json:"rule_id"`
	Severity    string  `json:"severity"`
	Category    string  `json:"category"`
	File        string  `json:"file"`
	Line        int     `json:"line"`
	Evidence    string  `json:"evidence"`
	Description string  `json:"description"`
	Suggestion  string  `json:"suggestion"`
	Confidence  float64 `json:"confidence"`
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
	rules []Rule
}

// New creates a Scanner with the builtin rule set.
func New() *Scanner {
	return &Scanner{rules: BuiltinRules()}
}

// NewWithRules creates a Scanner with a custom rule set (useful for testing).
func NewWithRules(rules []Rule) *Scanner {
	return &Scanner{rules: rules}
}

// ScanFiles runs all applicable rules against the changed files in repoPath.
// Only lines added in the diff (prefixed with "+") are scanned when diffContent
// is provided, to avoid flagging pre-existing issues.
func (s *Scanner) ScanFiles(ctx context.Context, repoPath string, changedFiles []string, diffContent string) ScanResult {
	// Parse the diff to extract added lines per file.
	addedLines := parseDiffAddedLines(diffContent)
	hasDiff := diffContent != ""

	result := ScanResult{
		RulesChecked: len(s.rules),
	}

	for _, relPath := range changedFiles {
		select {
		case <-ctx.Done():
			return result
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
			if !rule.appliesToFile(relPath) {
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

// parseDiffAddedLines extracts a map of file → {lineNumber: true} for all lines
// added in the diff (lines starting with "+", excluding the "+++ b/" header).
func parseDiffAddedLines(diffContent string) map[string]map[int]bool {
	result := make(map[string]map[int]bool)
	if diffContent == "" {
		return result
	}

	var currentFile string
	var newLineNum int

	for _, line := range strings.Split(diffContent, "\n") {
		if strings.HasPrefix(line, "+++ b/") {
			currentFile = strings.TrimPrefix(line, "+++ b/")
			continue
		}
		if strings.HasPrefix(line, "--- ") {
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			// Parse hunk header: @@ -old,count +new,count @@
			newLineNum = parseHunkNewStart(line)
			continue
		}
		if currentFile == "" {
			continue
		}
		if strings.HasPrefix(line, "+") {
			if result[currentFile] == nil {
				result[currentFile] = make(map[int]bool)
			}
			result[currentFile][newLineNum] = true
			newLineNum++
		} else if strings.HasPrefix(line, "-") {
			// Removed line — don't advance new line counter.
			continue
		} else {
			// Context line — advance new line counter.
			newLineNum++
		}
	}

	return result
}

// parseHunkNewStart extracts the new file start line from a hunk header.
// Format: @@ -old,count +new,count @@
func parseHunkNewStart(line string) int {
	// Find "+N" after the first space.
	plusIdx := strings.Index(line, "+")
	if plusIdx < 0 {
		return 1
	}
	rest := line[plusIdx+1:]
	// Read digits until comma or space.
	var num int
	for _, c := range rest {
		if c >= '0' && c <= '9' {
			num = num*10 + int(c-'0')
		} else {
			break
		}
	}
	if num == 0 {
		return 1
	}
	return num
}
