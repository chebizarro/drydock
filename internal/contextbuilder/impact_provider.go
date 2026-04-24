package contextbuilder

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"drydock/internal/symbols"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

// changeImpactProvider computes blast-radius analysis for changed files
// using reverse-import edges and symbol-consumer edges. It produces a
// compact "change-impact" context layer that surfaces downstream risk
// to the planner and reviewer.
type changeImpactProvider struct {
	search *searcher
}

func (changeImpactProvider) LayerName() string { return LayerChangeImpact }
func (changeImpactProvider) Priority() int     { return 2 }

// impactSummary collects blast-radius analysis results.
type impactSummary struct {
	Score               int
	Level               string
	ChangedSymbols      []impactSymbol
	DownstreamFiles     []string
	DownstreamTests     []string
	CrossWorkspaceRoots []string
	DirectImporters     []string
	SymbolConsumers     []string
	StructuralChanges   int
}

type impactSymbol struct {
	Name string
	Kind symbols.SymbolKind
	File string
}

func (p changeImpactProvider) Build(ctx context.Context, in BuildInput) (string, error) {
	files, err := parsePatch(in.PatchEventContent)
	if err != nil || len(files) == 0 {
		return "", nil
	}

	// 1. Collect changed file paths.
	var changedFiles []string
	for _, f := range files {
		path := pickPath(f)
		if path == "" || isExcludedPath(path) {
			continue
		}
		changedFiles = append(changedFiles, path)
	}
	if len(changedFiles) == 0 {
		return "", nil
	}
	changedSet := toSet(changedFiles)

	// 2. Extract changed symbols via tree-sitter (with regex fallback).
	changedSyms := p.extractChangedSymbols(ctx, in, files)

	// 3. Build reverse-import graph and find direct importers.
	importers := p.findDirectImporters(ctx, in.RepoPath, changedFiles, changedSet, in.WorkspaceRoots)

	// 4. Find symbol consumers via grep.
	consumers, testConsumers := p.findSymbolConsumers(ctx, in.RepoPath, changedSyms, changedSet, in.WorkspaceRoots)

	// 5. Merge downstream files (deduped).
	downstreamSet := make(map[string]bool)
	for _, f := range importers {
		downstreamSet[f] = true
	}
	for _, f := range consumers {
		downstreamSet[f] = true
	}
	var downstream []string
	for f := range downstreamSet {
		downstream = append(downstream, f)
	}

	// 6. Compute cross-workspace impact.
	crossRoots := crossWorkspaceImpact(changedFiles, downstream, in.WorkspaceRoots)

	// 7. Count structural changes.
	structuralCount := 0
	for _, s := range changedSyms {
		if isStructuralKind(s.Kind) {
			structuralCount++
		}
	}

	// 8. Compute risk score.
	summary := impactSummary{
		ChangedSymbols:      changedSyms,
		DownstreamFiles:     downstream,
		DownstreamTests:     testConsumers,
		CrossWorkspaceRoots: crossRoots,
		DirectImporters:     importers,
		SymbolConsumers:     consumers,
		StructuralChanges:   structuralCount,
	}
	summary.Score = computeRiskScore(summary, len(changedFiles))
	summary.Level = riskLevel(summary.Score)

	// 9. Suppress low/no-impact results.
	if summary.Score <= 2 && len(downstream) == 0 {
		return "", nil
	}

	return renderImpact(summary), nil
}

// --- Changed symbol extraction ---

const maxTrackedSymbols = 6

// noisySymbols are too generic for meaningful grep-based dependency search.
var noisySymbols = map[string]bool{
	"init": true, "main": true, "run": true, "new": true, "test": true,
	"open": true, "close": true, "read": true, "write": true,
	"start": true, "stop": true, "handle": true, "get": true, "set": true,
	"do": true, "make": true, "len": true, "err": true, "ok": true,
}

func (p changeImpactProvider) extractChangedSymbols(ctx context.Context, in BuildInput, files []*gitdiff.File) []impactSymbol {
	ext := symbols.New()
	defer ext.Close()

	var result []impactSymbol
	seen := make(map[string]bool)

	for _, f := range files {
		if len(result) >= maxTrackedSymbols {
			break
		}
		path := pickPath(f)
		if path == "" {
			continue
		}
		lang := symbols.LangFromExt(filepath.Ext(path))
		if lang == "" || !symbols.SupportedLanguage(lang) {
			continue
		}

		// Read file from working tree.
		fullPath := filepath.Join(in.RepoPath, path)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}

		changedLines := extractChangedLineNumbers(f)
		syms, err := ext.ExtractChanged(lang, data, changedLines)
		if err != nil || len(syms) == 0 {
			continue
		}

		for _, sym := range syms {
			if len(result) >= maxTrackedSymbols {
				break
			}
			name := sym.Name
			if len(name) < 4 || noisySymbols[strings.ToLower(name)] || seen[name] {
				continue
			}
			seen[name] = true
			result = append(result, impactSymbol{
				Name: name,
				Kind: sym.Kind,
				File: path,
			})
		}
	}

	// Regex fallback if tree-sitter yielded nothing.
	if len(result) == 0 {
		for _, name := range extractChangedSymbols(in.PatchEventContent) {
			if len(result) >= maxTrackedSymbols {
				break
			}
			if len(name) < 4 || noisySymbols[strings.ToLower(name)] || seen[name] {
				continue
			}
			seen[name] = true
			result = append(result, impactSymbol{Name: name, Kind: symbols.SymbolKind("unknown")})
		}
	}

	return result
}



// --- Reverse import graph ---

// Import patterns by language.
var (
	goImportRe = regexp.MustCompile(`^\s*"([^"]+)"`)
	pyImportRe = regexp.MustCompile(`(?:^import\s+([\w.]+)|^from\s+([\w.]+)\s+import)`)
	jsImportRe = regexp.MustCompile(`(?:from\s+['"](\.[^'"]+)['"]|require\(['"](\.[^'"]+)['"]\))`)
)

func (p changeImpactProvider) findDirectImporters(ctx context.Context, repoPath string, changedFiles []string, changedSet map[string]bool, workspaceRoots []string) []string {
	// Build module IDs for changed files.
	changedModules := make(map[string]bool)
	goModPath := findGoModulePath(repoPath, workspaceRoots)

	for _, path := range changedFiles {
		for _, modID := range fileModuleIDs(path, goModPath, repoPath) {
			changedModules[modID] = true
		}
	}
	if len(changedModules) == 0 {
		return nil
	}

	// List tracked files.
	args := []string{"ls-files"}
	if len(workspaceRoots) > 0 {
		args = append(args, "--")
		for _, r := range workspaceRoots {
			args = append(args, r+"/")
		}
	}
	out, err := runGit(ctx, repoPath, args...)
	if err != nil {
		return nil
	}

	var importers []string
	importerSet := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		path := strings.TrimSpace(scanner.Text())
		if path == "" || changedSet[path] {
			continue
		}
		ext := filepath.Ext(path)
		lang := langCategory(ext)
		if lang == "" {
			continue
		}

		fullPath := filepath.Join(repoPath, path)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}

		if importsAny(data, lang, path, changedModules, goModPath) && !importerSet[path] {
			importerSet[path] = true
			importers = append(importers, path)
		}
	}
	return importers
}

// langCategory returns the import-analysis language category.
func langCategory(ext string) string {
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".jsx", ".ts", ".tsx", ".mjs":
		return "js"
	default:
		return ""
	}
}

// fileModuleIDs returns the module identifiers that other files would use to import this file.
func fileModuleIDs(path, goModPath, repoRoot string) []string {
	ext := filepath.Ext(path)
	lang := langCategory(ext)
	dir := filepath.Dir(path)

	var ids []string
	switch lang {
	case "go":
		if goModPath != "" && dir != "." {
			ids = append(ids, goModPath+"/"+filepath.ToSlash(dir))
		}
	case "python":
		modPath := strings.TrimSuffix(filepath.ToSlash(path), ".py")
		modPath = strings.TrimSuffix(modPath, "/__init__")
		modPath = strings.ReplaceAll(modPath, "/", ".")
		ids = append(ids, modPath)
	case "js":
		base := strings.TrimSuffix(filepath.ToSlash(path), ext)
		base = strings.TrimSuffix(base, "/index")
		ids = append(ids, "./"+base)
		ids = append(ids, "./"+base+ext)
	}
	return ids
}

// findGoModulePath reads the module path from go.mod.
func findGoModulePath(repoRoot string, workspaceRoots []string) string {
	candidates := []string{repoRoot}
	for _, r := range workspaceRoots {
		candidates = append(candidates, filepath.Join(repoRoot, r))
	}
	for _, dir := range candidates {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "module ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "module"))
			}
		}
	}
	return ""
}

// importsAny checks if a file imports any of the changed modules.
func importsAny(data []byte, lang, filePath string, changedModules map[string]bool, goModPath string) bool {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		switch lang {
		case "go":
			if m := goImportRe.FindStringSubmatch(line); len(m) > 1 {
				// Only match imports within our module.
				if goModPath != "" && strings.HasPrefix(m[1], goModPath+"/") {
					if changedModules[m[1]] {
						return true
					}
				}
			}
		case "python":
			if m := pyImportRe.FindStringSubmatch(line); len(m) > 0 {
				mod := m[1]
				if mod == "" {
					mod = m[2]
				}
				if changedModules[mod] {
					return true
				}
			}
		case "js":
			if m := jsImportRe.FindStringSubmatch(line); len(m) > 0 {
				ref := m[1]
				if ref == "" {
					ref = m[2]
				}
				// Resolve relative import from the file's directory.
				dir := filepath.Dir(filePath)
				resolved := filepath.ToSlash(filepath.Join(dir, ref))
				resolved = "./" + resolved
				if changedModules[resolved] {
					return true
				}
				// Try with extension stripped.
				for _, ext := range []string{".js", ".ts", ".jsx", ".tsx", ".mjs"} {
					if changedModules[resolved+ext] {
						return true
					}
				}
			}
		}
	}
	return false
}

// --- Symbol consumer search ---

func (p changeImpactProvider) findSymbolConsumers(ctx context.Context, repoPath string, syms []impactSymbol, changedSet map[string]bool, workspaceRoots []string) (consumers, tests []string) {
	consumerSet := make(map[string]bool)
	testSet := make(map[string]bool)

	for _, sym := range syms {
		hits, err := p.search.SearchSymbolHits(ctx, repoPath, sym.Name, workspaceRoots)
		if err != nil {
			continue
		}
		for _, hit := range hits {
			if changedSet[hit.File] {
				continue
			}
			if isTestFile(hit.File) {
				if !testSet[hit.File] {
					testSet[hit.File] = true
					tests = append(tests, hit.File)
				}
			} else {
				if !consumerSet[hit.File] {
					consumerSet[hit.File] = true
					consumers = append(consumers, hit.File)
				}
			}
		}
	}
	return consumers, tests
}

func isTestFile(path string) bool {
	base := filepath.Base(path)
	if strings.HasSuffix(base, "_test.go") {
		return true
	}
	if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true
	}
	if strings.HasPrefix(base, "test_") {
		return true
	}
	dir := filepath.Dir(path) + "/"
	return strings.Contains(dir, "/test/") || strings.Contains(dir, "/tests/")
}

// --- Cross-workspace impact ---

func crossWorkspaceImpact(changedFiles, downstreamFiles []string, workspaceRoots []string) []string {
	if len(workspaceRoots) == 0 {
		return nil
	}
	originRoots := make(map[string]bool)
	for _, f := range changedFiles {
		originRoots[workspaceOf(f, workspaceRoots)] = true
	}
	impactedRoots := make(map[string]bool)
	for _, f := range downstreamFiles {
		ws := workspaceOf(f, workspaceRoots)
		if ws != "." && !originRoots[ws] {
			impactedRoots[ws] = true
		}
	}
	var result []string
	for r := range impactedRoots {
		result = append(result, r)
	}
	return result
}

func workspaceOf(path string, workspaceRoots []string) string {
	best := "."
	bestLen := 0
	for _, root := range workspaceRoots {
		if strings.HasPrefix(path, root+"/") && len(root) > bestLen {
			best = root
			bestLen = len(root)
		}
	}
	return best
}

// --- Risk scoring ---

func computeRiskScore(s impactSummary, changedFileCount int) int {
	score := 0
	score += min(4, len(s.DownstreamFiles))
	score += min(3, len(s.CrossWorkspaceRoots))
	score += min(2, s.StructuralChanges)
	if changedFileCount >= 5 {
		score++
	}
	if len(s.DownstreamTests) >= 3 {
		score++
	}
	if score > 10 {
		score = 10
	}
	return score
}

func riskLevel(score int) string {
	switch {
	case score <= 2:
		return "low"
	case score <= 5:
		return "medium"
	case score <= 8:
		return "high"
	default:
		return "critical"
	}
}

func isStructuralKind(kind symbols.SymbolKind) bool {
	switch kind {
	case symbols.KindType, symbols.KindClass, symbols.KindInterface,
		symbols.KindStruct, symbols.KindEnum, symbols.KindTrait, symbols.KindModule:
		return true
	}
	return false
}

// --- Rendering ---

const maxRenderBytes = 3072
const maxListedFiles = 5

func renderImpact(s impactSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Blast radius: %d/10 (%s)\n\n", s.Score, s.Level)

	if len(s.ChangedSymbols) > 0 {
		b.WriteString("Changed symbols:\n")
		for _, sym := range s.ChangedSymbols {
			if sym.File != "" {
				fmt.Fprintf(&b, "- %s (%s) in %s\n", sym.Name, sym.Kind, sym.File)
			} else {
				fmt.Fprintf(&b, "- %s (%s)\n", sym.Name, sym.Kind)
			}
		}
		b.WriteString("\n")
	}

	if len(s.DownstreamFiles) > 0 {
		fmt.Fprintf(&b, "Downstream non-test consumers: %d file(s)\n", len(s.DownstreamFiles))
	}
	if len(s.CrossWorkspaceRoots) > 0 {
		fmt.Fprintf(&b, "Cross-workspace impact: %d workspace(s)\n", len(s.CrossWorkspaceRoots))
	}
	b.WriteString("\n")

	if len(s.DirectImporters) > 0 {
		b.WriteString("Direct importers:\n")
		writeFileList(&b, s.DirectImporters)
		b.WriteString("\n")
	}

	if len(s.SymbolConsumers) > 0 {
		b.WriteString("Symbol consumers:\n")
		writeFileList(&b, s.SymbolConsumers)
		b.WriteString("\n")
	}

	if len(s.DownstreamTests) > 0 {
		fmt.Fprintf(&b, "Affected tests: %d file(s)\n\n", len(s.DownstreamTests))
	}

	if s.Level == "high" || s.Level == "critical" {
		b.WriteString("Planner hint: prioritize compatibility, architecture, and downstream correctness.\n")
	}

	result := b.String()
	if len(result) > maxRenderBytes {
		result = result[:maxRenderBytes] + "\n[truncated]\n"
	}
	return result
}

func writeFileList(b *strings.Builder, files []string) {
	for i, f := range files {
		if i >= maxListedFiles {
			fmt.Fprintf(b, "... and %d more\n", len(files)-maxListedFiles)
			break
		}
		fmt.Fprintf(b, "- %s\n", f)
	}
}

// --- Helpers ---

func toSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, item := range items {
		m[item] = true
	}
	return m
}
