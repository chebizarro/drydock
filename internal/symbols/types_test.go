package symbols

import "testing"

func TestLangFromExtTable(t *testing.T) {
	tests := []struct {
		ext  string
		want string
	}{
		{".go", "go"},
		{".py", "python"},
		{".js", "javascript"},
		{".ts", "typescript"},
		{".tsx", "typescript"},
		{".rs", "rust"},
		{".c", "c"},
		{".h", "c"},
		{".cpp", "cpp"},
		{".java", "java"},
		{".rb", "ruby"},
		{".unknown", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := LangFromExt(tt.ext)
		if got != tt.want {
			t.Errorf("LangFromExt(%q) = %q, want %q", tt.ext, got, tt.want)
		}
	}
}
