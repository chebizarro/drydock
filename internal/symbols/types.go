package symbols

import (
	"path/filepath"
	"sort"
	"strings"
)

// SymbolKind classifies what kind of declaration a symbol is.
type SymbolKind string

const (
	KindFunction  SymbolKind = "function"
	KindMethod    SymbolKind = "method"
	KindType      SymbolKind = "type"
	KindClass     SymbolKind = "class"
	KindInterface SymbolKind = "interface"
	KindStruct    SymbolKind = "struct"
	KindEnum      SymbolKind = "enum"
	KindTrait     SymbolKind = "trait"
	KindModule    SymbolKind = "module"
)

// Symbol represents a named declaration extracted from source code.
type Symbol struct {
	Name      string     `json:"name"`
	Kind      SymbolKind `json:"kind"`
	StartLine uint32     `json:"start_line"` // 0-based
	EndLine   uint32     `json:"end_line"`   // 0-based
	Parent    string     `json:"parent,omitempty"`
}

// LangFromExt returns a language identifier from a file extension (including dot).
// Returns empty string if the extension is not recognized.
func LangFromExt(ext string) string {
	lang, ok := extToLang[ext]
	if !ok {
		return ""
	}
	return lang
}

// PrimaryLanguage returns the most common recognized language across files,
// with first-seen order winning ties so the result is deterministic. Files with
// an unrecognized extension are ignored; an empty result means none matched.
func PrimaryLanguage(files []string) string {
	counts := make(map[string]int)
	var order []string // first-seen order for deterministic tie-breaking
	for _, f := range files {
		lang := LangFromExt(strings.ToLower(filepath.Ext(f)))
		if lang == "" {
			continue
		}
		if counts[lang] == 0 {
			order = append(order, lang)
		}
		counts[lang]++
	}
	best := ""
	bestCount := 0
	for _, lang := range order {
		if counts[lang] > bestCount {
			best = lang
			bestCount = counts[lang]
		}
	}
	return best
}

// LanguageMetadata describes one recognized source language.
type LanguageMetadata struct {
	ID         string
	Extensions []string
	Available  bool
}

// LanguageMetadataList returns every recognized language in stable order.
// Available reports whether this build includes its tree-sitter grammar.
func LanguageMetadataList() []LanguageMetadata {
	extensions := make(map[string][]string)
	for ext, language := range extToLang {
		extensions[language] = append(extensions[language], ext)
	}
	languages := make([]LanguageMetadata, 0, len(extensions))
	for id, values := range extensions {
		sort.Strings(values)
		languages = append(languages, LanguageMetadata{
			ID: id, Extensions: values,
			Available: TreeSitterAvailable() && SupportedLanguage(id),
		})
	}
	sort.Slice(languages, func(i, j int) bool { return languages[i].ID < languages[j].ID })
	return languages
}

var extToLang = map[string]string{
	".go":   "go",
	".py":   "python",
	".pyw":  "python",
	".js":   "javascript",
	".jsx":  "javascript",
	".mjs":  "javascript",
	".cjs":  "javascript",
	".ts":   "typescript",
	".tsx":  "typescript",
	".mts":  "typescript",
	".rs":   "rust",
	".c":    "c",
	".h":    "c",
	".cc":   "cpp",
	".cpp":  "cpp",
	".cxx":  "cpp",
	".hpp":  "cpp",
	".hxx":  "cpp",
	".hh":   "cpp",
	".java": "java",
	".rb":   "ruby",
	".erb":  "ruby",
}
