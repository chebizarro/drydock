//go:build !cgo

package symbols

import "fmt"

// Extractor is a no-op stub when CGO is disabled.
// Tree-sitter grammars require CGO_ENABLED=1.
type Extractor struct{}

// New returns a no-op extractor (tree-sitter requires CGO).
func New() *Extractor { return &Extractor{} }

// Close is a no-op without CGO.
func (e *Extractor) Close() {}

// Extract always returns an error without CGO.
func (e *Extractor) Extract(lang string, source []byte) ([]Symbol, error) {
	return nil, fmt.Errorf("tree-sitter requires CGO_ENABLED=1: unsupported language %s", lang)
}

// ExtractChanged always returns an error without CGO.
func (e *Extractor) ExtractChanged(lang string, source []byte, changedLines []uint32) ([]Symbol, error) {
	return nil, fmt.Errorf("tree-sitter requires CGO_ENABLED=1")
}

// SupportedLanguage always returns false without CGO.
func SupportedLanguage(lang string) bool { return false }

// SupportedLanguages returns nil without CGO.
func SupportedLanguages() []string { return nil }
