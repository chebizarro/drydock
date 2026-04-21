//go:build cgo

package symbols

import (
	sitter "github.com/smacker/go-tree-sitter"
	langc "github.com/smacker/go-tree-sitter/c"
	langcpp "github.com/smacker/go-tree-sitter/cpp"
	langgo "github.com/smacker/go-tree-sitter/golang"
	langjava "github.com/smacker/go-tree-sitter/java"
	langjs "github.com/smacker/go-tree-sitter/javascript"
	langpy "github.com/smacker/go-tree-sitter/python"
	langruby "github.com/smacker/go-tree-sitter/ruby"
	langrs "github.com/smacker/go-tree-sitter/rust"
	langts "github.com/smacker/go-tree-sitter/typescript/typescript"
)

// declSpec describes how to extract a symbol from a tree-sitter AST node.
type declSpec struct {
	kind          SymbolKind
	nameField     string                                      // tree-sitter field name to read
	nameFunc      func(node *sitter.Node, src []byte) string  // custom extraction (overrides nameField)
	isContainer   bool                                        // recurse into children with this node as parent
	containerOnly bool                                        // don't emit as symbol, only use as parent scope
}

// langConfig holds the tree-sitter language and its declaration node mappings.
type langConfig struct {
	language     *sitter.Language
	declarations map[string]declSpec
}

var languages = map[string]*langConfig{
	"go": {
		language: langgo.GetLanguage(),
		declarations: map[string]declSpec{
			"function_declaration": {kind: KindFunction, nameField: "name"},
			"method_declaration":   {kind: KindMethod, nameField: "name"},
			"type_spec":            {kind: KindType, nameField: "name"},
		},
	},

	"python": {
		language: langpy.GetLanguage(),
		declarations: map[string]declSpec{
			"function_definition": {kind: KindFunction, nameField: "name"},
			"class_definition":    {kind: KindClass, nameField: "name", isContainer: true},
		},
	},

	"javascript": {
		language: langjs.GetLanguage(),
		declarations: map[string]declSpec{
			"function_declaration": {kind: KindFunction, nameField: "name"},
			"class_declaration":    {kind: KindClass, nameField: "name", isContainer: true},
			"method_definition":    {kind: KindMethod, nameField: "name"},
		},
	},

	"typescript": {
		language: langts.GetLanguage(),
		declarations: map[string]declSpec{
			"function_declaration":   {kind: KindFunction, nameField: "name"},
			"class_declaration":      {kind: KindClass, nameField: "name", isContainer: true},
			"method_definition":      {kind: KindMethod, nameField: "name"},
			"interface_declaration":  {kind: KindInterface, nameField: "name"},
			"type_alias_declaration": {kind: KindType, nameField: "name"},
			"enum_declaration":       {kind: KindEnum, nameField: "name"},
		},
	},

	"rust": {
		language: langrs.GetLanguage(),
		declarations: map[string]declSpec{
			"function_item": {kind: KindFunction, nameField: "name"},
			"struct_item":   {kind: KindStruct, nameField: "name"},
			"enum_item":     {kind: KindEnum, nameField: "name"},
			"trait_item":    {kind: KindTrait, nameField: "name"},
			"impl_item":     {nameField: "type", isContainer: true, containerOnly: true},
		},
	},

	"c": {
		language: langc.GetLanguage(),
		declarations: map[string]declSpec{
			"function_definition": {kind: KindFunction, nameFunc: cFuncName},
			"struct_specifier":    {kind: KindStruct, nameField: "name"},
			"enum_specifier":      {kind: KindEnum, nameField: "name"},
		},
	},

	"cpp": {
		language: langcpp.GetLanguage(),
		declarations: map[string]declSpec{
			"function_definition": {kind: KindFunction, nameFunc: cFuncName},
			"class_specifier":     {kind: KindClass, nameField: "name", isContainer: true},
			"struct_specifier":    {kind: KindStruct, nameField: "name"},
			"enum_specifier":      {kind: KindEnum, nameField: "name"},
		},
	},

	"java": {
		language: langjava.GetLanguage(),
		declarations: map[string]declSpec{
			"class_declaration":     {kind: KindClass, nameField: "name", isContainer: true},
			"interface_declaration": {kind: KindInterface, nameField: "name", isContainer: true},
			"method_declaration":    {kind: KindMethod, nameField: "name"},
			"enum_declaration":      {kind: KindEnum, nameField: "name"},
		},
	},

	"ruby": {
		language: langruby.GetLanguage(),
		declarations: map[string]declSpec{
			"method":  {kind: KindMethod, nameField: "name"},
			"class":   {kind: KindClass, nameField: "name", isContainer: true},
			"module":  {kind: KindModule, nameField: "name"},
		},
	},
}

// cFuncName extracts the function name from C/C++ function_definition nodes
// where the name is nested: function_definition → declarator (function_declarator) → declarator (identifier).
func cFuncName(node *sitter.Node, source []byte) string {
	decl := node.ChildByFieldName("declarator")
	if decl == nil {
		return ""
	}
	// function_declarator → declarator (identifier)
	if decl.Type() == "function_declarator" {
		inner := decl.ChildByFieldName("declarator")
		if inner != nil {
			return inner.Content(source)
		}
	}
	return ""
}
