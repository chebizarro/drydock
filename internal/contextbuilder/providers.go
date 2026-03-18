package contextbuilder

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

func DefaultProviders() []Provider {
	return []Provider{
		patchDiffProvider{},
		fileContextProvider{},
		symbolsCallsitesProvider{},
		testsProvider{},
		importsExportsProvider{},
		commitHistoryProvider{},
		projectDocsProvider{},
	}
}

func parsePatch(content string) ([]*gitdiff.File, error) {
	files, _, err := gitdiff.Parse(strings.NewReader(content))
	if err != nil {
		return nil, err
	}
	return files, nil
}

type patchDiffProvider struct{}

func (patchDiffProvider) LayerName() string { return LayerPatchDiff }
func (patchDiffProvider) Priority() int     { return 1 }
func (patchDiffProvider) Build(_ context.Context, in BuildInput) (string, error) {
	diff := strings.TrimSpace(in.PatchEventContent)
	if diff == "" {
		return "", nil
	}
	b := []byte(diff)
	if len(b) > 40*1024 {
		b = b[:40*1024]
	}
	return string(b), nil
}

type fileContextProvider struct{}

func (fileContextProvider) LayerName() string { return LayerFileContext }
func (fileContextProvider) Priority() int     { return 2 }
func (fileContextProvider) Build(_ context.Context, in BuildInput) (string, error) {
	if in.RepoPath == "" {
		return "", nil
	}
	files, err := parsePatch(in.PatchEventContent)
	if err != nil {
		return "", nil
	}

	var out strings.Builder
	for _, f := range files {
		path := pickPath(f)
		if path == "" || isExcludedPath(path) {
			continue
		}
		abs := filepath.Join(in.RepoPath, path)
		content, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		if !isProbablyText(content) {
			continue
		}
		if out.Len() > 20*1024 {
			break
		}
		if len(content) > 4*1024 {
			content = content[:4*1024]
		}
		out.WriteString("### ")
		out.WriteString(path)
		out.WriteString("\n")
		out.Write(content)
		out.WriteString("\n\n")
	}
	return strings.TrimSpace(out.String()), nil
}

type symbolsCallsitesProvider struct{}

func (symbolsCallsitesProvider) LayerName() string { return LayerSymbolsCallsites }
func (symbolsCallsitesProvider) Priority() int     { return 3 }
func (symbolsCallsitesProvider) Build(ctx context.Context, in BuildInput) (string, error) {
	if in.RepoPath == "" {
		return "", nil
	}
	symbols := extractChangedSymbols(in.PatchEventContent)
	if len(symbols) == 0 {
		return "", nil
	}
	var out strings.Builder
	out.WriteString("symbols: ")
	out.WriteString(strings.Join(symbols, ", "))
	out.WriteString("\n")

	for _, sym := range symbols {
		lines, err := gitGrep(ctx, in.RepoPath, sym)
		if err != nil || lines == "" {
			continue
		}
		out.WriteString("\n#### ")
		out.WriteString(sym)
		out.WriteString("\n")
		out.WriteString(lines)
		out.WriteString("\n")
	}
	return strings.TrimSpace(out.String()), nil
}

type testsProvider struct{}

func (testsProvider) LayerName() string { return LayerTests }
func (testsProvider) Priority() int     { return 4 }
func (testsProvider) Build(ctx context.Context, in BuildInput) (string, error) {
	if in.RepoPath == "" {
		return "", nil
	}
	symbols := extractChangedSymbols(in.PatchEventContent)
	if len(symbols) == 0 {
		return "No tests reference modified symbols.", nil
	}
	var out strings.Builder
	foundAny := false
	for _, sym := range symbols {
		lines, _ := gitGrepTests(ctx, in.RepoPath, sym)
		if strings.TrimSpace(lines) == "" {
			continue
		}
		foundAny = true
		out.WriteString("### ")
		out.WriteString(sym)
		out.WriteString("\n")
		out.WriteString(lines)
		out.WriteString("\n")
	}
	if !foundAny {
		return "No tests reference modified symbols.", nil
	}
	return strings.TrimSpace(out.String()), nil
}

type importsExportsProvider struct{}

func (importsExportsProvider) LayerName() string { return LayerImportsExports }
func (importsExportsProvider) Priority() int     { return 5 }
func (importsExportsProvider) Build(_ context.Context, in BuildInput) (string, error) {
	lines := extractImportExportLines(in.PatchEventContent)
	if len(lines) == 0 {
		return "", nil
	}
	return strings.Join(lines, "\n"), nil
}

type commitHistoryProvider struct{}

func (commitHistoryProvider) LayerName() string { return LayerCommitHistory }
func (commitHistoryProvider) Priority() int     { return 6 }
func (commitHistoryProvider) Build(ctx context.Context, in BuildInput) (string, error) {
	if in.RepoPath == "" {
		return "", nil
	}
	files, err := parsePatch(in.PatchEventContent)
	if err != nil {
		return "", nil
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		path := pickPath(f)
		if path == "" || isExcludedPath(path) {
			continue
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return "", nil
	}

	args := []string{"-C", in.RepoPath, "log", "--oneline", "-n", "10", "--"}
	args = append(args, paths...)
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

type projectDocsProvider struct{}

func (projectDocsProvider) LayerName() string { return LayerProjectDocs }
func (projectDocsProvider) Priority() int     { return 7 }
func (projectDocsProvider) Build(_ context.Context, in BuildInput) (string, error) {
	if in.RepoPath == "" {
		return "", nil
	}

	candidates := []string{
		"CONTRIBUTING.md",
		"README.md",
		"docs/CONTRIBUTING.md",
		"docs/style-guide.md",
		"docs/STYLEGUIDE.md",
	}

	var out bytes.Buffer
	for _, rel := range candidates {
		path := filepath.Join(in.RepoPath, rel)
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 {
			continue
		}
		if !isProbablyText(data) {
			continue
		}
		if out.Len() > 15*1024 {
			break
		}
		if len(data) > 4*1024 {
			data = data[:4*1024]
		}
		out.WriteString("### ")
		out.WriteString(rel)
		out.WriteString("\n")
		out.Write(data)
		out.WriteString("\n\n")
	}
	return strings.TrimSpace(out.String()), nil
}

func pickPath(f *gitdiff.File) string {
	if f == nil {
		return ""
	}
	if f.NewName != "" && f.NewName != "/dev/null" {
		return f.NewName
	}
	if f.OldName != "" && f.OldName != "/dev/null" {
		return f.OldName
	}
	return ""
}

func extractChangedSymbols(diff string) []string {
	re := regexp.MustCompile(`(?m)^[+-]\s*(?:func|type|class|def)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	matches := re.FindAllStringSubmatch(diff, 32)
	syms := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		syms = append(syms, m[1])
	}
	slices.Sort(syms)
	syms = slices.Compact(syms)
	if len(syms) > 12 {
		syms = syms[:12]
	}
	return syms
}

func extractImportExportLines(diff string) []string {
	re := regexp.MustCompile(`(?m)^[+-]\s*(?:import|export|from|use|#include)\b.*$`)
	lines := re.FindAllString(diff, 100)
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	lines = slices.Compact(lines)
	return lines
}

func gitGrep(ctx context.Context, repoPath, symbol string) (string, error) {
	return runGit(ctx, repoPath, "grep", "-n", "-E", "\\b"+regexp.QuoteMeta(symbol)+"\\b", "--", ".")
}

func gitGrepTests(ctx context.Context, repoPath, symbol string) (string, error) {
	return runGit(ctx, repoPath, "grep", "-n", "-E", "\\b"+regexp.QuoteMeta(symbol)+"\\b", "--", "*test*", "*_test.go", "*.spec.*", "*.test.*")
}

func runGit(ctx context.Context, repoPath string, args ...string) (string, error) {
	full := append([]string{"-C", repoPath}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func isExcludedPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".proto") {
		return true
	}
	switch base {
	case "package-lock.json", "cargo.lock", "poetry.lock", "pnpm-lock.yaml", "bun.lock", "yarn.lock":
		return true
	}
	if strings.Contains(lower, "__generated__") || strings.Contains(lower, "generated/graphql") {
		return true
	}
	if strings.Contains(lower, "migration") && strings.Contains(lower, "snapshot") {
		return true
	}
	return false
}

func isProbablyText(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	if bytes.IndexByte(data, 0x00) >= 0 {
		return false
	}
	return true
}

