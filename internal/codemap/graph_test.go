package codemap

import (
	"context"
	"reflect"
	"testing"

	"drydock/internal/lspbridge"
)

func TestExtractImports(t *testing.T) {
	tests := []struct {
		name   string
		lang   string
		source string
		want   []string
	}{
		{
			name: "go block and alias",
			lang: "go",
			source: `import (
	"fmt"
	alias "example.com/project/pkg"
)
`,
			want: []string{"example.com/project/pkg", "fmt"},
		},
		{
			name:   "python",
			lang:   "python",
			source: "import os\nfrom package.module import Thing\n",
			want:   []string{"os", "package.module"},
		},
		{
			name:   "typescript",
			lang:   "typescript",
			source: "import x from './x';\nexport { y } from \"./y\";\nconst z = require('z');\n",
			want:   []string{"./x", "./y", "z"},
		},
		{
			name:   "rust",
			lang:   "rust",
			source: "use crate::module::Thing;\nmod local;\n",
			want:   []string{"crate::module::Thing", "local"},
		},
		{
			name:   "c",
			lang:   "c",
			source: "#include <stdio.h>\n#include \"local.h\"\n",
			want:   []string{"local.h", "stdio.h"},
		},
		{
			name:   "java",
			lang:   "java",
			source: "import java.util.List;\nimport static java.util.Collections.emptyList;\n",
			want:   []string{"java.util.Collections.emptyList", "java.util.List"},
		},
		{
			name:   "ruby",
			lang:   "ruby",
			source: "require 'json'\nrequire_relative \"support/helper\"\n",
			want:   []string{"json", "support/helper"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractImports(tt.lang, []byte(tt.source)); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("extractImports() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseCallsites(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		repoPath  string
		refPrefix string
		want      []callsite
	}{
		{
			name:     "ripgrep absolute path",
			raw:      "/repo/main.go:7: helper()\n",
			repoPath: "/repo",
			want:     []callsite{{path: "main.go", line: 7}},
		},
		{
			name:      "git grep ref",
			raw:       "HEAD:pkg/main.go:9:helper()\n",
			refPrefix: "HEAD:",
			want:      []callsite{{path: "pkg/main.go", line: 9}},
		},
		{
			name: "malformed ignored",
			raw:  "not-a-hit\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseCallsites(tt.raw, tt.repoPath, tt.refPrefix); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseCallsites() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestBuildUsesLSPReferences(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"main.go": `package main

func helper() {}

func main() {
	helper()
}
`,
	})
	fake := fakeAnalyzer{response: &lspbridge.AnalyzeResponse{
		Status:       "ok",
		LSPAvailable: true,
		References: []lspbridge.Reference{
			{Symbol: "helper", File: "main.go", Line: 6, Column: 2},
		},
	}}
	result, err := Build(context.Background(), repo, "HEAD",
		WithCacheDir(t.TempDir()),
		WithLSPAnalyzer(fake),
	)
	if err != nil {
		t.Fatal(err)
	}
	mainSymbol := onlySymbol(t, result, "main")
	helperSymbol := onlySymbol(t, result, "helper")
	if got := result.Callees(mainSymbol.ID); !reflect.DeepEqual(got, []string{helperSymbol.ID}) {
		t.Fatalf("LSP call graph = %#v, want helper", got)
	}
}

type fakeAnalyzer struct {
	response *lspbridge.AnalyzeResponse
	err      error
}

func (f fakeAnalyzer) Analyze(context.Context, lspbridge.AnalyzeRequest) (*lspbridge.AnalyzeResponse, error) {
	return f.response, f.err
}
