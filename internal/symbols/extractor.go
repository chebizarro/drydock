//go:build cgo

package symbols

import (
	"context"
	"fmt"

	sitter "github.com/smacker/go-tree-sitter"
)

// Extractor uses tree-sitter to parse source code and extract symbol declarations.
type Extractor struct {
	parser *sitter.Parser
}

// New creates a new tree-sitter based symbol extractor.
func New() *Extractor {
	return &Extractor{
		parser: sitter.NewParser(),
	}
}

// Close releases the parser resources.
func (e *Extractor) Close() {
	if e.parser != nil {
		e.parser.Close()
	}
}

// Extract parses the source with the given language's tree-sitter grammar
// and returns all symbol declarations found.
func (e *Extractor) Extract(lang string, source []byte) ([]Symbol, error) {
	cfg, ok := languages[lang]
	if !ok {
		return nil, fmt.Errorf("unsupported language: %s", lang)
	}

	e.parser.SetLanguage(cfg.language)
	tree, err := e.parser.ParseCtx(context.Background(), nil, source)
	if err != nil {
		return nil, fmt.Errorf("tree-sitter parse failed: %w", err)
	}
	defer tree.Close()

	var symbols []Symbol
	walkNode(tree.RootNode(), source, cfg, "", &symbols)
	return symbols, nil
}

// ExtractChanged returns only symbols whose line ranges overlap with changedLines (0-based).
func (e *Extractor) ExtractChanged(lang string, source []byte, changedLines []uint32) ([]Symbol, error) {
	all, err := e.Extract(lang, source)
	if err != nil {
		return nil, err
	}
	if len(changedLines) == 0 {
		return all, nil
	}

	lineSet := make(map[uint32]struct{}, len(changedLines))
	for _, l := range changedLines {
		lineSet[l] = struct{}{}
	}

	var changed []Symbol
	for _, sym := range all {
		for line := sym.StartLine; line <= sym.EndLine; line++ {
			if _, hit := lineSet[line]; hit {
				changed = append(changed, sym)
				break
			}
		}
	}
	return changed, nil
}

// SupportedLanguage returns true if the language has tree-sitter grammar support.
func SupportedLanguage(lang string) bool {
	_, ok := languages[lang]
	return ok
}

// SupportedLanguages returns the list of supported language identifiers.
func SupportedLanguages() []string {
	langs := make([]string, 0, len(languages))
	for k := range languages {
		langs = append(langs, k)
	}
	return langs
}

func walkNode(node *sitter.Node, source []byte, cfg *langConfig, parent string, symbols *[]Symbol) {
	if node == nil {
		return
	}

	nodeType := node.Type()

	if spec, ok := cfg.declarations[nodeType]; ok {
		var name string
		if spec.nameFunc != nil {
			name = spec.nameFunc(node, source)
		} else {
			name = fieldText(node, source, spec.nameField)
		}

		if name != "" {
			// Container-only nodes (e.g. Rust impl blocks) set parent scope
			// without emitting themselves as symbols.
			if !spec.containerOnly {
				sym := Symbol{
					Name:      name,
					Kind:      spec.kind,
					StartLine: node.StartPoint().Row,
					EndLine:   node.EndPoint().Row,
					Parent:    parent,
				}
				*symbols = append(*symbols, sym)
			}

			// For classes/types that contain methods, recurse with this as parent
			if spec.isContainer {
				for i := 0; i < int(node.ChildCount()); i++ {
					walkNode(node.Child(i), source, cfg, name, symbols)
				}
				return
			}
		}
	}

	// Recurse into children
	for i := 0; i < int(node.ChildCount()); i++ {
		walkNode(node.Child(i), source, cfg, parent, symbols)
	}
}

// fieldText returns the text content of a named field child.
func fieldText(node *sitter.Node, source []byte, field string) string {
	if field == "" {
		return ""
	}
	child := node.ChildByFieldName(field)
	if child == nil {
		return ""
	}
	return child.Content(source)
}
