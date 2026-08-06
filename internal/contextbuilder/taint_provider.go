package contextbuilder

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"drydock/internal/lspbridge"
	"drydock/internal/symbols"
)

const LayerTaint = "taint"

const (
	maxTaintFiles = 2000
	maxTaintPaths = 40
	maxTaintDepth = 8
)

type taintProvider struct {
	lspClient *lspbridge.Client
}

type taintFunction struct {
	key       string
	name      string
	file      string
	startLine int
	endLine   int
	body      string
	calls     []string
	source    string
	sinks     []taintSink
}

type taintSink struct {
	name     string
	category string
	file     string
	line     int
}

type taintPath struct {
	nodes []string
	sink  taintSink
}

type taintPattern struct {
	category string
	re       *regexp.Regexp
}

var taintSinkPatterns = []taintPattern{
	{"command-execution", regexp.MustCompile(`(?i)\b(?:exec\.Command(?:Context)?|os\.system|subprocess\.(?:run|call|Popen)|child_process\.(?:exec|spawn)|Runtime\.getRuntime\(\)\.exec|eval)\s*\(`)},
	{"sql", regexp.MustCompile(`(?i)\b(?:Query|QueryRow|Exec|Raw|execute|executemany)\s*\(`)},
	{"file-write", regexp.MustCompile(`(?i)\b(?:os\.(?:WriteFile|Create)|ioutil\.WriteFile|fs\.(?:writeFile|writeFileSync)|File\.write|open)\s*\(`)},
	{"outbound-request", regexp.MustCompile(`(?i)\b(?:http\.(?:Get|Post|NewRequest)|requests\.(?:get|post|request)|fetch|urlopen)\s*\(`)},
	{"unsafe-deserialization", regexp.MustCompile(`(?i)\b(?:pickle\.loads?|yaml\.load|ObjectInputStream|Marshal\.load)\s*\(`)},
	{"template-render", regexp.MustCompile(`(?i)\b(?:template\.HTML|render_template_string|dangerouslySetInnerHTML)\b`)},
}

var taintSourcePatterns = []taintPattern{
	{"request input", regexp.MustCompile(`(?i)\b(?:req(?:uest)?|input|payload|body|query|params?|headers?|cookies?|form|argv)\b`)},
	{"environment input", regexp.MustCompile(`(?i)\b(?:os\.(?:Getenv|LookupEnv)|process\.env|std::env::var)\b`)},
	{"network input", regexp.MustCompile(`(?i)\b(?:ReadString|ReadBytes|ReadFrom|recv|receive|request\.json|request\.args|URL\.Query)\b`)},
}

var (
	callExpressionRE = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	fallbackFuncRE   = regexp.MustCompile(`(?m)^\s*(?:func\s+(?:\([^)]*\)\s*)?|def\s+|(?:public\s+|private\s+|protected\s+|static\s+|async\s+)*(?:[A-Za-z_][A-Za-z0-9_<>,\[\]?*&:]*\s+))([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
)

func (taintProvider) LayerName() string { return LayerTaint }
func (taintProvider) Priority() int     { return 2 }

func (p taintProvider) Build(ctx context.Context, in BuildInput) (string, error) {
	if in.RepoPath == "" {
		return "", nil
	}

	changed, err := taintChangedFiles(in.PatchEventContent)
	if err != nil {
		return "", &LayerWarning{Err: fmt.Errorf("parse patch: %w", err)}
	}

	functions, warnings := scanTaintFunctions(ctx, in)
	if len(functions) == 0 {
		if len(warnings) > 0 {
			return "", &LayerWarning{Err: errors.Join(warnings...)}
		}
		return "", nil
	}

	lspUsed := false
	if p.lspClient != nil {
		used, lspErr := p.addLSPEdges(ctx, in, functions)
		lspUsed = used
		if lspErr != nil {
			warnings = append(warnings, lspErr)
		}
	}

	paths := enumerateTaintPaths(functions, changed)
	if len(paths) == 0 {
		if len(warnings) > 0 {
			return "", &LayerWarning{Err: errors.Join(warnings...)}
		}
		return "", nil
	}
	content := renderTaintPaths(functions, paths, lspUsed)
	if len(warnings) > 0 {
		return content, &LayerWarning{Err: errors.Join(warnings...)}
	}
	return content, nil
}

func scanTaintFunctions(ctx context.Context, in BuildInput) (map[string]*taintFunction, []error) {
	out := make(map[string]*taintFunction)
	var warnings []error
	extractor := symbols.New()
	defer extractor.Close()

	roots := []string{in.RepoPath}
	if len(in.WorkspaceRoots) > 0 {
		roots = roots[:0]
		for _, root := range in.WorkspaceRoots {
			roots = append(roots, filepath.Join(in.RepoPath, root))
		}
	}

	filesSeen := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				if path != root && isTaintExcludedDir(entry.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			lang := symbols.LangFromExt(strings.ToLower(filepath.Ext(path)))
			if lang == "" || filesSeen >= maxTaintFiles {
				return nil
			}
			filesSeen++
			data, err := os.ReadFile(path)
			if err != nil || len(data) == 0 || len(data) > 512*1024 || !isProbablyText(data) {
				return nil
			}
			rel, err := filepath.Rel(in.RepoPath, path)
			if err != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			extracted, extractErr := extractor.Extract(lang, data)
			if extractErr != nil {
				extracted = fallbackTaintSymbols(data)
			}
			addTaintFunctions(out, rel, data, extracted)
			return nil
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			warnings = append(warnings, fmt.Errorf("scan %s: %w", root, err))
		}
		if err := ctx.Err(); err != nil {
			warnings = append(warnings, err)
			break
		}
	}
	return out, warnings
}

func isTaintExcludedDir(name string) bool {
	switch strings.ToLower(name) {
	case ".git", "vendor", "node_modules", "dist", "build", "target", ".venv", "venv", "__pycache__":
		return true
	default:
		return false
	}
}

func fallbackTaintSymbols(data []byte) []symbols.Symbol {
	lines := strings.Split(string(data), "\n")
	matches := fallbackFuncRE.FindAllStringSubmatchIndex(string(data), -1)
	result := make([]symbols.Symbol, 0, len(matches))
	for i, match := range matches {
		if len(match) < 4 {
			continue
		}
		start := strings.Count(string(data[:match[0]]), "\n")
		end := len(lines) - 1
		if i+1 < len(matches) {
			end = strings.Count(string(data[:matches[i+1][0]]), "\n") - 1
		}
		result = append(result, symbols.Symbol{Name: string(data[match[2]:match[3]]), Kind: symbols.KindFunction, StartLine: uint32(start), EndLine: uint32(max(start, end))})
	}
	return result
}

func addTaintFunctions(dst map[string]*taintFunction, file string, data []byte, extracted []symbols.Symbol) {
	lines := strings.Split(string(data), "\n")
	for _, sym := range extracted {
		if sym.Kind != symbols.KindFunction && sym.Kind != symbols.KindMethod {
			continue
		}
		start, end := int(sym.StartLine), int(sym.EndLine)
		if start < 0 || start >= len(lines) {
			continue
		}
		if end < start {
			end = start
		}
		if end >= len(lines) {
			end = len(lines) - 1
		}
		body := strings.Join(lines[start:end+1], "\n")
		key := fmt.Sprintf("%s:%d:%s", file, start+1, sym.Name)
		fn := &taintFunction{key: key, name: sym.Name, file: file, startLine: start + 1, endLine: end + 1, body: body}
		for _, pattern := range taintSourcePatterns {
			if pattern.re.MatchString(body) {
				fn.source = pattern.category
				break
			}
		}
		for _, pattern := range taintSinkPatterns {
			for _, loc := range pattern.re.FindAllStringIndex(body, -1) {
				line := start + 1 + strings.Count(body[:loc[0]], "\n")
				fn.sinks = append(fn.sinks, taintSink{name: strings.TrimSpace(body[loc[0]:loc[1]]), category: pattern.category, file: file, line: line})
			}
		}
		for _, match := range callExpressionRE.FindAllStringSubmatch(body, -1) {
			if len(match) > 1 && match[1] != sym.Name {
				fn.calls = append(fn.calls, match[1])
			}
		}
		slices.Sort(fn.calls)
		fn.calls = slices.Compact(fn.calls)
		dst[key] = fn
	}
}

func (p taintProvider) addLSPEdges(ctx context.Context, in BuildInput, functions map[string]*taintFunction) (bool, error) {
	var sinkNames []string
	for _, pattern := range taintSinkPatterns {
		for _, fn := range functions {
			for _, sink := range fn.sinks {
				if pattern.category == sink.category {
					name := callName(sink.name)
					if name != "" {
						sinkNames = append(sinkNames, name)
					}
				}
			}
		}
	}
	slices.Sort(sinkNames)
	sinkNames = slices.Compact(sinkNames)
	if len(sinkNames) == 0 {
		return false, nil
	}
	if len(sinkNames) > 32 {
		sinkNames = sinkNames[:32]
	}

	changed, _ := taintChangedFiles(in.PatchEventContent)
	resp, err := p.lspClient.Analyze(ctx, lspbridge.AnalyzeRequest{RepoPath: in.RepoPath, ChangedFiles: setKeys(changed), Symbols: sinkNames})
	if err != nil {
		return false, fmt.Errorf("LSP call hierarchy unavailable: %w; using tree-sitter call graph", err)
	}
	if resp == nil || resp.Error != "" || resp.Status == "error" || resp.Status == "degraded" {
		message := "no usable response"
		if resp != nil && resp.Error != "" {
			message = resp.Error
		}
		return false, fmt.Errorf("LSP call hierarchy unavailable: %s; using tree-sitter call graph", message)
	}

	used := false
	for _, ref := range resp.References {
		fn := functionAt(functions, filepath.ToSlash(ref.File), ref.Line)
		if fn == nil || ref.Symbol == "" {
			continue
		}
		fn.calls = append(fn.calls, ref.Symbol)
		slices.Sort(fn.calls)
		fn.calls = slices.Compact(fn.calls)
		used = true
	}
	return used, nil
}

func taintChangedFiles(patch string) (map[string]bool, error) {
	result := make(map[string]bool)
	if strings.TrimSpace(patch) == "" {
		return result, nil
	}
	files, err := parsePatch(patch)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if path := pickPath(file); path != "" {
			result[filepath.ToSlash(path)] = true
		}
	}
	return result, nil
}

func enumerateTaintPaths(functions map[string]*taintFunction, changed map[string]bool) []taintPath {
	byName := make(map[string][]string)
	for key, fn := range functions {
		byName[fn.name] = append(byName[fn.name], key)
	}
	for name := range byName {
		slices.Sort(byName[name])
	}

	var sources []string
	for key, fn := range functions {
		if fn.source != "" {
			sources = append(sources, key)
		}
	}
	slices.Sort(sources)

	var paths []taintPath
	for _, source := range sources {
		var visit func(string, []string, map[string]bool)
		visit = func(key string, path []string, seen map[string]bool) {
			if len(paths) >= maxTaintPaths || len(path) >= maxTaintDepth || seen[key] {
				return
			}
			fn := functions[key]
			nextPath := append(append([]string(nil), path...), key)
			nextSeen := make(map[string]bool, len(seen)+1)
			for k, v := range seen {
				nextSeen[k] = v
			}
			nextSeen[key] = true
			for _, sink := range fn.sinks {
				if len(changed) == 0 || pathTouchesChanged(nextPath, functions, changed) {
					paths = append(paths, taintPath{nodes: nextPath, sink: sink})
					if len(paths) >= maxTaintPaths {
						return
					}
				}
			}
			for _, call := range fn.calls {
				for _, target := range byName[call] {
					visit(target, nextPath, nextSeen)
				}
			}
		}
		visit(source, nil, map[string]bool{})
		if len(paths) >= maxTaintPaths {
			break
		}
	}
	return paths
}

func pathTouchesChanged(path []string, functions map[string]*taintFunction, changed map[string]bool) bool {
	for _, key := range path {
		if changed[functions[key].file] {
			return true
		}
	}
	return false
}

func renderTaintPaths(functions map[string]*taintFunction, paths []taintPath, lspUsed bool) string {
	mode := "tree-sitter call-graph approximation"
	if lspUsed {
		mode = "LSP-assisted call hierarchy"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Approximate source -> sink paths (%s):\n", mode)
	for i, path := range paths {
		source := functions[path.nodes[0]]
		fmt.Fprintf(&out, "%d. SOURCE [%s] %s:%d %s", i+1, source.source, source.file, source.startLine, source.name)
		for _, key := range path.nodes[1:] {
			fn := functions[key]
			fmt.Fprintf(&out, " -> %s:%d %s", fn.file, fn.startLine, fn.name)
		}
		fmt.Fprintf(&out, " -> SINK [%s] %s:%d %s\n", path.sink.category, path.sink.file, path.sink.line, path.sink.name)
	}
	return strings.TrimSpace(out.String())
}

func functionAt(functions map[string]*taintFunction, file string, line int) *taintFunction {
	var best *taintFunction
	for _, fn := range functions {
		if fn.file != file || line < fn.startLine || line > fn.endLine {
			continue
		}
		if best == nil || fn.endLine-fn.startLine < best.endLine-best.startLine {
			best = fn
		}
	}
	return best
}

func callName(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.LastIndex(value, "."); idx >= 0 {
		value = value[idx+1:]
	}
	value = strings.TrimSuffix(value, "(")
	return strings.TrimSpace(value)
}

func setKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}
