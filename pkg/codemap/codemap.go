// Package codemap detects source languages, extracts symbols, and builds
// immutable whole-repository Git codemaps with owned public DTOs.
//
// Parser and Builder are safe for concurrent callers, but each instance
// serializes tree-sitter work because the underlying parser is stateful.
// Use one Parser or Builder per worker when parallel parsing throughput matters.
package codemap

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	internalcodemap "git.sharegap.net/cascadia/drydock/internal/codemap"
	"git.sharegap.net/cascadia/drydock/internal/symbols"
)

var (
	// ErrParserClosed reports use of a Parser after Close.
	ErrParserClosed = errors.New("drydock codemap: parser is closed")
)

// SymbolKind classifies a source declaration.
type SymbolKind string

const (
	// KindFunction identifies a free function.
	KindFunction SymbolKind = "function"
	// KindMethod identifies a method.
	KindMethod SymbolKind = "method"
	// KindType identifies a type declaration.
	KindType SymbolKind = "type"
	// KindClass identifies a class.
	KindClass SymbolKind = "class"
	// KindInterface identifies an interface.
	KindInterface SymbolKind = "interface"
	// KindStruct identifies a struct.
	KindStruct SymbolKind = "struct"
	// KindEnum identifies an enum.
	KindEnum SymbolKind = "enum"
	// KindTrait identifies a trait.
	KindTrait SymbolKind = "trait"
	// KindModule identifies a module.
	KindModule SymbolKind = "module"
)

// Language describes one recognized source language.
type Language struct {
	// ID is the stable language identifier used by ParseLanguage.
	ID string
	// Extensions are recognized lowercase file extensions including the dot.
	Extensions []string
	// Available reports whether this build includes the tree-sitter grammar.
	Available bool
}

// Languages returns recognized language metadata in stable ID order.
func Languages() []Language {
	metadata := symbols.LanguageMetadataList()
	out := make([]Language, 0, len(metadata))
	for _, language := range metadata {
		out = append(out, Language{
			ID:         language.ID,
			Extensions: append([]string(nil), language.Extensions...),
			Available:  language.Available,
		})
	}
	return out
}

// DetectLanguage detects a language from a path extension.
// The boolean reports whether the extension is recognized.
func DetectLanguage(path string) (Language, bool) {
	id := symbols.LangFromExt(strings.ToLower(filepath.Ext(path)))
	if id == "" {
		return Language{}, false
	}
	for _, language := range Languages() {
		if language.ID == id {
			return language, true
		}
	}
	return Language{ID: id, Available: symbols.TreeSitterAvailable() && symbols.SupportedLanguage(id)}, true
}

// TreeSitterAvailable reports whether this build includes CGO tree-sitter support.
func TreeSitterAvailable() bool {
	return symbols.TreeSitterAvailable()
}

// SourceSymbol is a declaration extracted directly from source.
// StartLine and EndLine are zero-based to match tree-sitter coordinates.
type SourceSymbol struct {
	// Name is the declaration name.
	Name string
	// Kind classifies the declaration.
	Kind SymbolKind
	// StartLine is the zero-based first source line.
	StartLine uint32
	// EndLine is the zero-based last source line.
	EndLine uint32
	// Parent names the containing declaration when available.
	Parent string
}

// Parser reuses one tree-sitter parser.
// Methods are concurrency-safe but serialized; Close may run concurrently.
type Parser struct {
	mu        sync.Mutex
	extractor *symbols.Extractor
	closed    bool
}

// NewParser creates a reusable source symbol parser.
func NewParser() *Parser {
	return &Parser{extractor: symbols.New()}
}

// Parse detects the language from path and extracts declarations.
func (p *Parser) Parse(path string, source []byte) ([]SourceSymbol, error) {
	language, ok := DetectLanguage(path)
	if !ok {
		return nil, fmt.Errorf("drydock codemap: unsupported source extension for %s", path)
	}
	return p.ParseLanguage(language.ID, source)
}

// ParseLanguage extracts declarations using a stable language identifier.
func (p *Parser) ParseLanguage(language string, source []byte) ([]SourceSymbol, error) {
	if p == nil {
		return nil, ErrParserClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.extractor == nil {
		return nil, ErrParserClosed
	}
	found, err := p.extractor.Extract(language, source)
	if err != nil {
		return nil, err
	}
	return sourceSymbols(found), nil
}

// Close releases parser resources. It is safe and idempotent.
func (p *Parser) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.extractor.Close()
	p.extractor = nil
	p.closed = true
	return nil
}

// Symbols detects a path's language and extracts declarations with a short-lived parser.
func Symbols(path string, source []byte) ([]SourceSymbol, error) {
	parser := NewParser()
	defer parser.Close()
	return parser.Parse(path, source)
}

// BuilderOptions configures whole-repository codemap generation.
type BuilderOptions struct {
	// CacheDir overrides the repository-local codemap cache.
	CacheDir string
}

// Builder constructs whole-tree Git codemaps.
// Calls on one Builder are concurrency-safe but serialized.
type Builder struct {
	mu    sync.Mutex
	inner *internalcodemap.Builder
}

// NewBuilder creates a whole-repository codemap builder.
func NewBuilder(opts BuilderOptions) *Builder {
	var options []internalcodemap.Option
	if strings.TrimSpace(opts.CacheDir) != "" {
		options = append(options, internalcodemap.WithCacheDir(opts.CacheDir))
	}
	return &Builder{inner: internalcodemap.New(options...)}
}

// Build constructs a codemap for a Git ref. An empty ref means HEAD.
func (b *Builder) Build(ctx context.Context, repoPath, ref string) (*Map, error) {
	if b == nil || b.inner == nil {
		return nil, fmt.Errorf("drydock codemap: builder is not initialized")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	internal, err := b.inner.Build(ctx, repoPath, ref)
	if err != nil {
		return nil, err
	}
	return fromInternalMap(internal), nil
}

// Build constructs a codemap with a short-lived Builder.
func Build(ctx context.Context, repoPath, ref string, opts BuilderOptions) (*Map, error) {
	return NewBuilder(opts).Build(ctx, repoPath, ref)
}

// Symbol is a repository declaration. Lines are one-based.
type Symbol struct {
	// ID is stable within a repository tree.
	ID string
	// Name is the declaration name.
	Name string
	// Kind classifies the declaration.
	Kind SymbolKind
	// Path is repository-relative.
	Path string
	// Language is the stable language identifier.
	Language string
	// StartLine is the one-based first source line.
	StartLine uint32
	// EndLine is the one-based last source line.
	EndLine uint32
	// Parent names the containing declaration when available.
	Parent string
}

// File contains codemap intelligence for one repository file.
type File struct {
	// Path is repository-relative.
	Path string
	// BlobHash is the Git blob object ID.
	BlobHash string
	// Language is the stable language identifier.
	Language string
	// Symbols contains declarations in source order.
	Symbols []Symbol
	// Imports contains stable sorted import targets.
	Imports []string
}

// RankedSymbol is one PageRank-ranked repository symbol.
type RankedSymbol struct {
	// SymbolID identifies the declaration.
	SymbolID string
	// Name is the declaration name.
	Name string
	// Path is repository-relative.
	Path string
	// Line is the one-based declaration line.
	Line uint32
	// Kind classifies the declaration.
	Kind SymbolKind
	// Score is the normalized graph rank.
	Score float64
}

// CacheStats describes how a build used its disk cache.
type CacheStats struct {
	// TreeHit reports reuse of a complete tree map.
	TreeHit bool
	// ReusedFiles counts reused blob analyses.
	ReusedFiles int
	// ParsedFiles counts newly parsed blobs.
	ParsedFiles int
}

// Map is the complete owned codemap for one Git tree.
type Map struct {
	// Version is the codemap schema version.
	Version int
	// TreeHash is the Git tree object ID.
	TreeHash string
	// Ref is the requested Git ref.
	Ref string
	// Files maps repository-relative paths to file intelligence.
	Files map[string]File
	// SymbolIndex maps names to declarations.
	SymbolIndex map[string][]Symbol
	// CallGraph maps caller symbol IDs to callee IDs.
	CallGraph map[string][]string
	// ReverseCallGraph maps callee symbol IDs to caller IDs.
	ReverseCallGraph map[string][]string
	// ImportGraph maps file paths to imported targets.
	ImportGraph map[string][]string
	// RepoMap contains ranked symbols.
	RepoMap []RankedSymbol
	// Cache describes reuse during this build.
	Cache CacheStats
}

// Callees returns a copy of the symbol IDs referenced by symbolID.
func (m *Map) Callees(symbolID string) []string {
	if m == nil {
		return nil
	}
	return append([]string(nil), m.CallGraph[symbolID]...)
}

// Callers returns a copy of the symbol IDs that reference symbolID.
func (m *Map) Callers(symbolID string) []string {
	if m == nil {
		return nil
	}
	return append([]string(nil), m.ReverseCallGraph[symbolID]...)
}

// Top returns a copy of the highest-ranked symbols, up to limit.
// A non-positive limit returns all ranked symbols.
func (m *Map) Top(limit int) []RankedSymbol {
	if m == nil {
		return nil
	}
	if limit <= 0 || limit >= len(m.RepoMap) {
		return append([]RankedSymbol(nil), m.RepoMap...)
	}
	return append([]RankedSymbol(nil), m.RepoMap[:limit]...)
}

func sourceSymbols(in []symbols.Symbol) []SourceSymbol {
	out := make([]SourceSymbol, 0, len(in))
	for _, symbol := range in {
		out = append(out, SourceSymbol{
			Name: symbol.Name, Kind: SymbolKind(symbol.Kind),
			StartLine: symbol.StartLine, EndLine: symbol.EndLine, Parent: symbol.Parent,
		})
	}
	return out
}

func fromInternalMap(in *internalcodemap.Map) *Map {
	if in == nil {
		return nil
	}
	out := &Map{
		Version: in.Version, TreeHash: in.TreeHash, Ref: in.Ref,
		Files:            make(map[string]File, len(in.Files)),
		SymbolIndex:      make(map[string][]Symbol, len(in.SymbolIndex)),
		CallGraph:        cloneStringMap(in.CallGraph),
		ReverseCallGraph: cloneStringMap(in.ReverseCallGraph),
		ImportGraph:      cloneStringMap(in.ImportGraph),
		Cache: CacheStats{
			TreeHit: in.Cache.TreeHit, ReusedFiles: in.Cache.ReusedFiles, ParsedFiles: in.Cache.ParsedFiles,
		},
	}
	for path, file := range in.Files {
		out.Files[path] = File{
			Path: file.Path, BlobHash: file.BlobHash, Language: file.Language,
			Symbols: mapSymbols(file.Symbols), Imports: append([]string(nil), file.Imports...),
		}
	}
	for name, indexed := range in.SymbolIndex {
		out.SymbolIndex[name] = mapSymbols(indexed)
	}
	for _, ranked := range in.RepoMap {
		out.RepoMap = append(out.RepoMap, RankedSymbol{
			SymbolID: ranked.SymbolID, Name: ranked.Name, Path: ranked.Path,
			Line: ranked.Line, Kind: SymbolKind(ranked.Kind), Score: ranked.Score,
		})
	}
	return out
}

func mapSymbols(in []internalcodemap.Symbol) []Symbol {
	out := make([]Symbol, 0, len(in))
	for _, symbol := range in {
		out = append(out, Symbol{
			ID: symbol.ID, Name: symbol.Name, Kind: SymbolKind(symbol.Kind),
			Path: symbol.Path, Language: symbol.Language, StartLine: symbol.StartLine,
			EndLine: symbol.EndLine, Parent: symbol.Parent,
		})
	}
	return out
}

func cloneStringMap(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for key, values := range in {
		out[key] = append([]string(nil), values...)
	}
	return out
}
