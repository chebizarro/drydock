// Package codemap builds and caches whole-repository symbol and reference maps.
package codemap

import "drydock/internal/symbols"

const cacheVersion = 1

// Symbol is a repository symbol declaration. Lines are one-based.
type Symbol struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Kind      symbols.SymbolKind `json:"kind"`
	Path      string             `json:"path"`
	Language  string             `json:"language"`
	StartLine uint32             `json:"start_line"`
	EndLine   uint32             `json:"end_line"`
	Parent    string             `json:"parent,omitempty"`
}

// File contains the cached intelligence derived from one repository file.
type File struct {
	Path     string   `json:"path"`
	BlobHash string   `json:"blob_hash"`
	Language string   `json:"language"`
	Symbols  []Symbol `json:"symbols,omitempty"`
	Imports  []string `json:"imports,omitempty"`
}

// RankedSymbol is one entry in the PageRank-ranked repository map.
type RankedSymbol struct {
	SymbolID string  `json:"symbol_id"`
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Line     uint32  `json:"line"`
	Kind     string  `json:"kind"`
	Score    float64 `json:"score"`
}

// CacheStats describes how a build used the disk cache.
type CacheStats struct {
	TreeHit     bool `json:"-"`
	ReusedFiles int  `json:"-"`
	ParsedFiles int  `json:"-"`
}

// Map is the complete repository code map for one Git tree.
type Map struct {
	Version          int                 `json:"version"`
	TreeHash         string              `json:"tree_hash"`
	Ref              string              `json:"ref"`
	Files            map[string]File     `json:"files"`
	SymbolIndex      map[string][]Symbol `json:"symbol_index"`
	CallGraph        map[string][]string `json:"call_graph"`
	ReverseCallGraph map[string][]string `json:"reverse_call_graph"`
	ImportGraph      map[string][]string `json:"import_graph"`
	RepoMap          []RankedSymbol      `json:"repo_map"`
	Cache            CacheStats          `json:"-"`
}

// Callees returns the stable symbol IDs referenced by symbolID.
func (m *Map) Callees(symbolID string) []string {
	return append([]string(nil), m.CallGraph[symbolID]...)
}

// Callers returns the stable symbol IDs that reference symbolID.
func (m *Map) Callers(symbolID string) []string {
	return append([]string(nil), m.ReverseCallGraph[symbolID]...)
}

// Top returns the highest-ranked repository symbols, up to limit.
// A non-positive limit returns all ranked symbols.
func (m *Map) Top(limit int) []RankedSymbol {
	if limit <= 0 || limit >= len(m.RepoMap) {
		return append([]RankedSymbol(nil), m.RepoMap...)
	}
	return append([]RankedSymbol(nil), m.RepoMap[:limit]...)
}
