package codemap

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"drydock/internal/lspbridge"
)

type callsite struct {
	symbol string
	path   string
	line   int
}

func (b *Builder) buildReferenceGraphs(ctx context.Context, repoPath, ref string, result *Map) error {
	for _, declarations := range result.SymbolIndex {
		for _, declaration := range declarations {
			result.CallGraph[declaration.ID] = []string{}
			result.ReverseCallGraph[declaration.ID] = []string{}
		}
	}

	var sites []callsite
	if b.lsp != nil {
		resp, err := b.lsp.Analyze(ctx, lspbridge.AnalyzeRequest{
			RepoPath:     repoPath,
			ChangedFiles: sortedFilePaths(result.Files),
			Symbols:      sortedSymbolNames(result.SymbolIndex),
		})
		if err == nil && usableLSPResponse(resp) {
			for _, reference := range resp.References {
				sites = append(sites, callsite{
					symbol: reference.Symbol,
					path:   filepath.ToSlash(reference.File),
					line:   reference.Line,
				})
			}
		}
	}

	if len(sites) == 0 {
		for _, name := range sortedSymbolNames(result.SymbolIndex) {
			hits, err := searchSymbol(ctx, repoPath, ref, name)
			if err != nil {
				return fmt.Errorf("codemap: search references for %s: %w", name, err)
			}
			for _, hit := range hits {
				hit.symbol = name
				sites = append(sites, hit)
			}
		}
	}

	edges := make(map[string]map[string]struct{})
	for _, site := range sites {
		file, ok := result.Files[filepath.ToSlash(site.path)]
		if !ok {
			continue
		}
		caller, ok := enclosingSymbol(file.Symbols, site.line)
		if !ok {
			continue
		}
		for _, target := range referenceTargets(result.SymbolIndex[site.symbol], site.path) {
			if caller.ID == target.ID && uint32(site.line) == target.StartLine {
				continue
			}
			if edges[caller.ID] == nil {
				edges[caller.ID] = make(map[string]struct{})
			}
			edges[caller.ID][target.ID] = struct{}{}
		}
	}

	for caller, targets := range edges {
		for target := range targets {
			result.CallGraph[caller] = append(result.CallGraph[caller], target)
			result.ReverseCallGraph[target] = append(result.ReverseCallGraph[target], caller)
		}
	}
	for id := range result.CallGraph {
		sort.Strings(result.CallGraph[id])
	}
	for id := range result.ReverseCallGraph {
		sort.Strings(result.ReverseCallGraph[id])
	}
	return nil
}

func usableLSPResponse(resp *lspbridge.AnalyzeResponse) bool {
	if resp == nil || resp.Error != "" || resp.Status == "degraded" || resp.Status == "error" {
		return false
	}
	return resp.LSPAvailable && len(resp.LanguageErrors) == 0 && len(resp.References) > 0
}

func enclosingSymbol(declarations []Symbol, line int) (Symbol, bool) {
	var best Symbol
	found := false
	for _, declaration := range declarations {
		if line < int(declaration.StartLine) || line > int(declaration.EndLine) {
			continue
		}
		if !found || declaration.EndLine-declaration.StartLine < best.EndLine-best.StartLine {
			best = declaration
			found = true
		}
	}
	return best, found
}

func referenceTargets(candidates []Symbol, callsitePath string) []Symbol {
	if len(candidates) <= 1 {
		return candidates
	}
	callsitePath = filepath.ToSlash(callsitePath)
	var local []Symbol
	for _, candidate := range candidates {
		if candidate.Path == callsitePath {
			local = append(local, candidate)
		}
	}
	if len(local) > 0 {
		return local
	}
	return candidates
}

func sortedFilePaths(files map[string]File) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func sortedSymbolNames(index map[string][]Symbol) []string {
	names := make([]string, 0, len(index))
	for name := range index {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func searchSymbol(ctx context.Context, repoPath, ref, symbol string) ([]callsite, error) {
	if ref == "HEAD" {
		if rgPath, err := exec.LookPath("rg"); err == nil {
			pattern := `\b` + regexp.QuoteMeta(symbol) + `\b`
			cmd := exec.CommandContext(ctx, rgPath,
				"--no-heading", "--line-number", "--color=never",
				"--max-columns=200", "-e", pattern, repoPath)
			out, err := cmd.CombinedOutput()
			if err == nil {
				return parseCallsites(string(out), repoPath, ""), nil
			}
			if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1 {
				return nil, nil
			}
		}
	}
	return gitGrepSymbol(ctx, repoPath, ref, symbol)
}

func gitGrepSymbol(ctx context.Context, repoPath, ref, symbol string) ([]callsite, error) {
	prefix := ref + ":"
	args := []string{"-C", repoPath, "grep", "-n", "-P", `\b` + regexp.QuoteMeta(symbol) + `\b`, ref, "--"}
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return parseCallsites(string(out), "", prefix), nil
	}
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1 {
		return nil, nil
	}

	args = []string{"-C", repoPath, "grep", "-n", "-F", symbol, ref, "--"}
	cmd = exec.CommandContext(ctx, "git", args...)
	out, err = cmd.CombinedOutput()
	if err != nil {
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("git grep: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseCallsites(string(out), "", prefix), nil
}

func parseCallsites(raw, repoPath, refPrefix string) []callsite {
	var hits []callsite
	for _, rawLine := range strings.Split(strings.TrimSpace(raw), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if refPrefix != "" {
			line = strings.TrimPrefix(line, refPrefix)
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		lineNumber, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		path := parts[0]
		if repoPath != "" {
			if relative, err := filepath.Rel(repoPath, path); err == nil {
				path = relative
			}
		}
		hits = append(hits, callsite{path: filepath.ToSlash(path), line: lineNumber})
	}
	return hits
}

func extractImports(lang string, source []byte) []string {
	switch lang {
	case "go":
		return extractGoImports(source)
	case "python":
		return matchImports(source, []*regexp.Regexp{
			regexp.MustCompile(`(?m)^\s*from\s+([A-Za-z0-9_\.]+)\s+import\b`),
			regexp.MustCompile(`(?m)^\s*import\s+([A-Za-z0-9_\.]+)`),
		})
	case "javascript", "typescript":
		return matchImports(source, []*regexp.Regexp{
			regexp.MustCompile(`(?m)\b(?:import|export)\s+(?:[^"'\n]+\s+from\s+)?["']([^"']+)["']`),
			regexp.MustCompile(`(?m)\brequire\s*\(\s*["']([^"']+)["']\s*\)`),
		})
	case "rust":
		return matchImports(source, []*regexp.Regexp{
			regexp.MustCompile(`(?m)^\s*use\s+([^;]+)`),
			regexp.MustCompile(`(?m)^\s*mod\s+([A-Za-z_][A-Za-z0-9_]*)\s*;`),
		})
	case "c", "cpp":
		return matchImports(source, []*regexp.Regexp{
			regexp.MustCompile(`(?m)^\s*#\s*include\s*[<"]([^>"]+)[>"]`),
		})
	case "java":
		return matchImports(source, []*regexp.Regexp{
			regexp.MustCompile(`(?m)^\s*import\s+(?:static\s+)?([^;]+)`),
		})
	case "ruby":
		return matchImports(source, []*regexp.Regexp{
			regexp.MustCompile(`(?m)^\s*require(?:_relative)?\s*\(?\s*["']([^"']+)["']`),
		})
	default:
		return nil
	}
}

func extractGoImports(source []byte) []string {
	var imports []string
	scanner := bufio.NewScanner(strings.NewReader(string(source)))
	inBlock := false
	quoted := regexp.MustCompile(`"([^"]+)"`)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !inBlock && strings.HasPrefix(line, "import (") {
			inBlock = true
		}
		if inBlock {
			if line == ")" {
				inBlock = false
				continue
			}
			if match := quoted.FindStringSubmatch(line); len(match) == 2 {
				imports = append(imports, match[1])
			}
			continue
		}
		if strings.HasPrefix(line, "import ") {
			if match := quoted.FindStringSubmatch(line); len(match) == 2 {
				imports = append(imports, match[1])
			}
		}
	}
	return sortedUnique(imports)
}

func matchImports(source []byte, patterns []*regexp.Regexp) []string {
	var imports []string
	for _, pattern := range patterns {
		for _, match := range pattern.FindAllSubmatch(source, -1) {
			if len(match) > 1 {
				imports = append(imports, strings.TrimSpace(string(match[1])))
			}
		}
	}
	return sortedUnique(imports)
}

func sortedUnique(values []string) []string {
	sort.Strings(values)
	if len(values) == 0 {
		return nil
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func rankSymbols(result *Map) []RankedSymbol {
	var declarations []Symbol
	for _, file := range result.Files {
		declarations = append(declarations, file.Symbols...)
	}
	sort.Slice(declarations, func(i, j int) bool { return declarations[i].ID < declarations[j].ID })
	if len(declarations) == 0 {
		return nil
	}

	const damping = 0.85
	count := float64(len(declarations))
	score := make(map[string]float64, len(declarations))
	for _, declaration := range declarations {
		score[declaration.ID] = 1 / count
	}
	for range 50 {
		next := make(map[string]float64, len(score))
		for id := range score {
			next[id] = (1 - damping) / count
		}
		var dangling float64
		for id, value := range score {
			targets := result.CallGraph[id]
			if len(targets) == 0 {
				dangling += value
				continue
			}
			share := damping * value / float64(len(targets))
			for _, target := range targets {
				next[target] += share
			}
		}
		share := damping * dangling / count
		var delta float64
		for id := range next {
			next[id] += share
			delta += math.Abs(next[id] - score[id])
		}
		score = next
		if delta < 1e-12 {
			break
		}
	}

	ranked := make([]RankedSymbol, 0, len(declarations))
	for _, declaration := range declarations {
		ranked = append(ranked, RankedSymbol{
			SymbolID: declaration.ID,
			Name:     declaration.Name,
			Path:     declaration.Path,
			Line:     declaration.StartLine,
			Kind:     string(declaration.Kind),
			Score:    score[declaration.ID],
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		if ranked[i].Path != ranked[j].Path {
			return ranked[i].Path < ranked[j].Path
		}
		if ranked[i].Line != ranked[j].Line {
			return ranked[i].Line < ranked[j].Line
		}
		return ranked[i].SymbolID < ranked[j].SymbolID
	})
	return ranked
}
