// Package lspbridge defines types shared between the LSP bridge server and client.
//
// The LSP bridge is a standalone HTTP service that manages language server
// processes and exposes a simple REST API for code analysis.
package lspbridge

// AnalyzeRequest is sent to POST /analyze.
type AnalyzeRequest struct {
	// RepoPath is the absolute path to the repository root.
	RepoPath string `json:"repo_path"`
	// ChangedFiles lists files modified in the patch (relative to repo root).
	ChangedFiles []string `json:"changed_files"`
	// Symbols to look up (function/type/variable names).
	Symbols []string `json:"symbols,omitempty"`
}

// AnalyzeResponse is returned from POST /analyze.
type AnalyzeResponse struct {
	Status         string          `json:"status,omitempty"` // ok, degraded, or error
	LSPAvailable   bool            `json:"lsp_available"`
	Definitions    []SymbolInfo    `json:"definitions,omitempty"`
	References     []Reference     `json:"references,omitempty"`
	Diagnostics    []Diagnostic    `json:"diagnostics,omitempty"`
	LanguageErrors []LanguageError `json:"language_errors,omitempty"`
	Error          string          `json:"error,omitempty"`
}

// LanguageError describes a per-language LSP bridge failure.
type LanguageError struct {
	Language string `json:"language"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// SymbolInfo describes a symbol definition found by the language server.
type SymbolInfo struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // function, type, variable, etc.
	File     string `json:"file"` // relative to repo root
	Line     int    `json:"line"`
	Detail   string `json:"detail,omitempty"` // type signature, doc comment, etc.
	Language string `json:"language"`
}

// Reference describes a location where a symbol is used.
type Reference struct {
	Symbol string `json:"symbol"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

// Diagnostic is a warning or error reported by the language server.
type Diagnostic struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity"` // error, warning, info, hint
	Message  string `json:"message"`
	Source   string `json:"source"` // e.g. "gopls", "pyright"
}

// HealthResponse is returned from GET /healthz.
type HealthResponse struct {
	Status       string            `json:"status"`
	Processes    map[string]string `json:"processes,omitempty"` // language → status
	AllowedRoots []string          `json:"allowed_roots,omitempty"`
	AuthRequired bool              `json:"auth_required,omitempty"`
}

// Language constants.
const (
	LangGo         = "go"
	LangPython     = "python"
	LangTypeScript = "typescript"
	LangJavaScript = "javascript"
	LangRust       = "rust"
	LangC          = "c"
	LangCPP        = "cpp"
)

// LangFromExt maps file extensions to language identifiers.
func LangFromExt(ext string) string {
	switch ext {
	case ".go":
		return LangGo
	case ".py", ".pyi":
		return LangPython
	case ".ts", ".tsx":
		return LangTypeScript
	case ".js", ".jsx", ".mjs", ".cjs":
		return LangJavaScript
	case ".rs":
		return LangRust
	case ".c", ".h":
		return LangC
	case ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx":
		return LangCPP
	default:
		return ""
	}
}

// LSPCommandConfig describes how to start a language server.
type LSPCommandConfig struct {
	Command  string   `json:"command"`
	Args     []string `json:"args,omitempty"`
	Disabled bool     `json:"disabled,omitempty"`
}

// SupportedLanguages returns the language identifiers supported by the bridge.
func SupportedLanguages() []string {
	return []string{LangGo, LangPython, LangTypeScript, LangJavaScript, LangRust, LangC, LangCPP}
}

// DefaultLSPCommandConfig returns the default command and args for a language.
// A zero-value config means the language is not supported.
func DefaultLSPCommandConfig(lang string) LSPCommandConfig {
	switch lang {
	case LangGo:
		return LSPCommandConfig{Command: "gopls", Args: []string{"serve"}}
	case LangPython:
		return LSPCommandConfig{Command: "pylsp"}
	case LangTypeScript, LangJavaScript:
		return LSPCommandConfig{Command: "typescript-language-server", Args: []string{"--stdio"}}
	case LangRust:
		return LSPCommandConfig{Command: "rust-analyzer"}
	case LangC, LangCPP:
		return LSPCommandConfig{Command: "clangd"}
	default:
		return LSPCommandConfig{}
	}
}

// LSPCommand returns the command to start the language server for a language.
// Returns empty string if the language is not supported.
func LSPCommand(lang string) string {
	return DefaultLSPCommandConfig(lang).Command
}
