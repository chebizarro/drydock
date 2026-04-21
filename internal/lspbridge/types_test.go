package lspbridge

import "testing"

func TestLangFromExt(t *testing.T) {
	tests := []struct {
		ext  string
		want string
	}{
		{".go", LangGo},
		{".py", LangPython},
		{".pyi", LangPython},
		{".ts", LangTypeScript},
		{".tsx", LangTypeScript},
		{".js", LangJavaScript},
		{".jsx", LangJavaScript},
		{".mjs", LangJavaScript},
		{".rs", LangRust},
		{".c", LangC},
		{".h", LangC},
		{".cpp", LangCPP},
		{".hpp", LangCPP},
		{".txt", ""},
		{".md", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := LangFromExt(tt.ext)
		if got != tt.want {
			t.Errorf("LangFromExt(%q) = %q, want %q", tt.ext, got, tt.want)
		}
	}
}

func TestLSPCommand(t *testing.T) {
	tests := []struct {
		lang string
		want string
	}{
		{LangGo, "gopls"},
		{LangPython, "pylsp"},
		{LangTypeScript, "typescript-language-server"},
		{LangJavaScript, "typescript-language-server"},
		{LangRust, "rust-analyzer"},
		{LangC, "clangd"},
		{LangCPP, "clangd"},
		{"unknown", ""},
	}
	for _, tt := range tests {
		got := LSPCommand(tt.lang)
		if got != tt.want {
			t.Errorf("LSPCommand(%q) = %q, want %q", tt.lang, got, tt.want)
		}
	}
}
