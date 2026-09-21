package symbols

import "testing"

func TestPrimaryLanguage(t *testing.T) {
	tests := []struct {
		files []string
		want  string
	}{
		{[]string{"main.go", "util.go", "test.py"}, "go"},
		{[]string{"app.py", "models.py"}, "python"},
		{[]string{"index.ts", "App.tsx"}, "typescript"},
		{[]string{"data.csv", "config.yml"}, ""}, // no recognized language
		{nil, ""},
	}
	for _, tt := range tests {
		if got := PrimaryLanguage(tt.files); got != tt.want {
			t.Errorf("PrimaryLanguage(%v) = %q, want %q", tt.files, got, tt.want)
		}
	}
}

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
