package codemap

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/gitexec"
	"git.sharegap.net/cascadia/drydock/internal/hashutil"
	"git.sharegap.net/cascadia/drydock/internal/lspbridge"
	"git.sharegap.net/cascadia/drydock/internal/symbols"
)

const maxSourceBytes = 2 * 1024 * 1024

type analyzer interface {
	Analyze(context.Context, lspbridge.AnalyzeRequest) (*lspbridge.AnalyzeResponse, error)
}

// Option configures a Builder.
type Option func(*Builder)

// WithCacheDir stores code-map data in dir instead of the repository-local
// .git/drydock-codemap directory.
func WithCacheDir(dir string) Option {
	return func(b *Builder) { b.cacheDir = dir }
}

// WithLSPClient enables type-aware reference discovery before grep fallback.
func WithLSPClient(client *lspbridge.Client) Option {
	return func(b *Builder) { b.lsp = client }
}

// WithLSPAnalyzer enables an Analyze-compatible LSP bridge implementation.
// It is primarily useful for deterministic tests and alternative bridges.
func WithLSPAnalyzer(a interface {
	Analyze(context.Context, lspbridge.AnalyzeRequest) (*lspbridge.AnalyzeResponse, error)
}) Option {
	return func(b *Builder) { b.lsp = a }
}

// Builder constructs whole-tree code maps.
type Builder struct {
	cacheDir string
	lsp      analyzer
}

// New returns a code-map builder.
func New(opts ...Option) *Builder {
	b := &Builder{}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Build constructs a code map for ref. An empty ref means HEAD.
func Build(ctx context.Context, repoPath, ref string, opts ...Option) (*Map, error) {
	return New(opts...).Build(ctx, repoPath, ref)
}

// Build constructs a code map for ref. An empty ref means HEAD.
func (b *Builder) Build(ctx context.Context, repoPath, ref string) (*Map, error) {
	if repoPath == "" {
		return nil, fmt.Errorf("codemap: repo path is required")
	}
	if ref == "" {
		ref = "HEAD"
	}
	if !symbols.TreeSitterAvailable() {
		return nil, fmt.Errorf("codemap: symbol extraction unavailable; rebuild with CGO_ENABLED=1")
	}

	treeHash, err := gitexec.Output(ctx, repoPath, "rev-parse", ref+"^{tree}")
	if err != nil {
		return nil, fmt.Errorf("codemap: resolve tree for %s: %w", ref, err)
	}

	cacheDir, err := b.resolveCacheDir(ctx, repoPath)
	if err != nil {
		return nil, err
	}
	treePath := filepath.Join(cacheDir, "trees", treeHash+".json")
	if cached, err := readTreeCache(treePath, treeHash); err == nil && cached != nil {
		cached.Ref = ref
		cached.Cache = CacheStats{TreeHit: true, ReusedFiles: len(cached.Files)}
		return cached, nil
	}

	entries, err := listTree(ctx, repoPath, ref)
	if err != nil {
		return nil, fmt.Errorf("codemap: list tree: %w", err)
	}

	extractor := symbols.New()
	defer extractor.Close()

	result := &Map{
		Version:          CacheVersion,
		TreeHash:         treeHash,
		Ref:              ref,
		Files:            make(map[string]File),
		SymbolIndex:      make(map[string][]Symbol),
		CallGraph:        make(map[string][]string),
		ReverseCallGraph: make(map[string][]string),
		ImportGraph:      make(map[string][]string),
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lang := symbols.LangFromExt(strings.ToLower(filepath.Ext(entry.path)))
		if lang == "" {
			continue
		}

		blob, reused, err := b.loadBlob(ctx, cacheDir, repoPath, entry.hash, lang, extractor)
		if err != nil {
			return nil, fmt.Errorf("codemap: index %s: %w", entry.path, err)
		}
		if reused {
			result.Cache.ReusedFiles++
		} else {
			result.Cache.ParsedFiles++
		}

		file := File{
			Path:     entry.path,
			BlobHash: entry.hash,
			Language: lang,
			Imports:  append([]string(nil), blob.Imports...),
		}
		for _, declaration := range blob.Symbols {
			qualified := declaration.Name
			if declaration.Parent != "" {
				qualified = declaration.Parent + "." + declaration.Name
			}
			sym := Symbol{
				ID:        fmt.Sprintf("%s:%d:%s", entry.path, declaration.StartLine+1, qualified),
				Name:      declaration.Name,
				Kind:      declaration.Kind,
				Path:      entry.path,
				Language:  lang,
				StartLine: declaration.StartLine + 1,
				EndLine:   declaration.EndLine + 1,
				Parent:    declaration.Parent,
			}
			file.Symbols = append(file.Symbols, sym)
			result.SymbolIndex[sym.Name] = append(result.SymbolIndex[sym.Name], sym)
		}
		result.Files[entry.path] = file
		if len(file.Imports) > 0 {
			result.ImportGraph[entry.path] = append([]string(nil), file.Imports...)
		}
	}

	sortMapSymbols(result.SymbolIndex)
	if err := b.buildReferenceGraphs(ctx, repoPath, ref, result); err != nil {
		return nil, err
	}
	result.RepoMap = rankSymbols(result)

	if err := WriteJSONAtomic(treePath, result); err != nil {
		return nil, fmt.Errorf("codemap: write tree cache: %w", err)
	}
	return result, nil
}

type treeEntry struct {
	hash string
	path string
}

func listTree(ctx context.Context, repoPath, ref string) ([]treeEntry, error) {
	out, err := gitexec.RunBytes(ctx, repoPath, "ls-tree", "-r", "-z", ref)
	if err != nil {
		return nil, err
	}
	var entries []treeEntry
	for _, record := range strings.Split(string(out), "\x00") {
		if record == "" {
			continue
		}
		metaPath := strings.SplitN(record, "\t", 2)
		if len(metaPath) != 2 {
			continue
		}
		meta := strings.Fields(metaPath[0])
		if len(meta) != 3 || meta[1] != "blob" || !strings.HasPrefix(meta[0], "100") {
			continue
		}
		entries = append(entries, treeEntry{hash: meta[2], path: metaPath[1]})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return entries, nil
}

type blobCache struct {
	Version  int              `json:"version"`
	BlobHash string           `json:"blob_hash"`
	Language string           `json:"language"`
	Symbols  []symbols.Symbol `json:"symbols,omitempty"`
	Imports  []string         `json:"imports,omitempty"`
}

func (b *Builder) loadBlob(
	ctx context.Context,
	cacheDir, repoPath, hash, lang string,
	extractor *symbols.Extractor,
) (blobCache, bool, error) {
	path := filepath.Join(cacheDir, "blobs", hash[:2], hash+".json")
	var cached blobCache
	if err := readJSON(path, &cached); err == nil &&
		cached.Version == CacheVersion && cached.BlobHash == hash && cached.Language == lang {
		return cached, true, nil
	}

	source, err := gitBlob(ctx, repoPath, hash)
	if err != nil {
		return blobCache{}, false, err
	}
	if len(source) > maxSourceBytes || !hashutil.IsProbablyText(source) {
		empty := blobCache{Version: CacheVersion, BlobHash: hash, Language: lang}
		if err := WriteJSONAtomic(path, empty); err != nil {
			return blobCache{}, false, err
		}
		return empty, false, nil
	}

	declarations, err := extractor.Extract(lang, source)
	if err != nil {
		return blobCache{}, false, err
	}
	cached = blobCache{
		Version:  CacheVersion,
		BlobHash: hash,
		Language: lang,
		Symbols:  declarations,
		Imports:  extractImports(lang, source),
	}
	if err := WriteJSONAtomic(path, cached); err != nil {
		return blobCache{}, false, err
	}
	return cached, false, nil
}

func (b *Builder) resolveCacheDir(ctx context.Context, repoPath string) (string, error) {
	if b.cacheDir != "" {
		return b.cacheDir, nil
	}
	return ResolveCacheDir(ctx, repoPath)
}

// ResolveCacheDir returns the checkout's shared drydock-codemap directory inside
// the common git dir. nostrscan caches its profiles alongside code maps here, so
// this is the single source of the directory path both packages write into.
func ResolveCacheDir(ctx context.Context, repoPath string) (string, error) {
	gitDir, err := gitexec.Output(ctx, repoPath, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("codemap: resolve git directory: %w", err)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoPath, gitDir)
	}
	return filepath.Join(filepath.Clean(gitDir), "drydock-codemap"), nil
}

func readTreeCache(path, treeHash string) (*Map, error) {
	var cached Map
	if err := readJSON(path, &cached); err != nil {
		return nil, err
	}
	if cached.Version != CacheVersion || cached.TreeHash != treeHash {
		return nil, fmt.Errorf("stale cache")
	}
	return &cached, nil
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

// WriteJSONAtomic marshals value to path via a temp file and rename so a
// concurrent reader never observes a partial cache entry. It is exported so
// nostrscan, which caches into the same directory, shares one writer.
func WriteJSONAtomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func gitBlob(ctx context.Context, repoPath, hash string) ([]byte, error) {
	return gitexec.RunBytes(ctx, repoPath, "cat-file", "blob", hash)
}

func sortMapSymbols(index map[string][]Symbol) {
	for name := range index {
		sort.Slice(index[name], func(i, j int) bool {
			a, b := index[name][i], index[name][j]
			if a.Path != b.Path {
				return a.Path < b.Path
			}
			if a.StartLine != b.StartLine {
				return a.StartLine < b.StartLine
			}
			return a.ID < b.ID
		})
	}
}
