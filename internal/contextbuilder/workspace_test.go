package contextbuilder

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectNPMWorkspaces(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"name": "my-monorepo",
		"workspaces": ["packages/*", "apps/*"]
	}`), 0o644)

	// Create workspace directories
	os.MkdirAll(filepath.Join(dir, "packages", "auth"), 0o755)
	os.MkdirAll(filepath.Join(dir, "packages", "core"), 0o755)
	os.MkdirAll(filepath.Join(dir, "apps", "web"), 0o755)

	ws := DetectWorkspaces(dir)

	roots := make(map[string]string)
	for _, w := range ws {
		roots[w.Root] = w.Type
	}

	expected := map[string]string{
		filepath.Join("packages", "auth"): "npm",
		filepath.Join("packages", "core"): "npm",
		filepath.Join("apps", "web"):      "npm",
	}

	for root, wType := range expected {
		if roots[root] != wType {
			t.Errorf("expected workspace %s (%s), got %s", root, wType, roots[root])
		}
	}
	if len(ws) != 3 {
		t.Errorf("expected 3 workspaces, got %d: %+v", len(ws), ws)
	}
}

func TestDetectNPMWorkspacesYarnObject(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"name": "yarn-mono",
		"workspaces": {
			"packages": ["packages/*"]
		}
	}`), 0o644)
	os.MkdirAll(filepath.Join(dir, "packages", "lib"), 0o755)

	ws := DetectWorkspaces(dir)
	if len(ws) != 1 || ws[0].Root != filepath.Join("packages", "lib") {
		t.Errorf("unexpected workspaces: %+v", ws)
	}
}

func TestDetectPNPMWorkspaces(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pnpm-workspace.yaml"), []byte(`packages:
  - 'packages/*'
  - 'tools/*'
`), 0o644)
	os.MkdirAll(filepath.Join(dir, "packages", "ui"), 0o755)
	os.MkdirAll(filepath.Join(dir, "tools", "cli"), 0o755)

	ws := DetectWorkspaces(dir)
	if len(ws) != 2 {
		t.Fatalf("expected 2 workspaces, got %d: %+v", len(ws), ws)
	}

	roots := make(map[string]bool)
	for _, w := range ws {
		roots[w.Root] = true
		if w.Type != "pnpm" {
			t.Errorf("expected pnpm type, got %s", w.Type)
		}
	}
	if !roots[filepath.Join("packages", "ui")] || !roots[filepath.Join("tools", "cli")] {
		t.Errorf("missing workspace roots: %+v", ws)
	}
}

func TestDetectCargoWorkspaces(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(`[workspace]
members = [
    "crates/core",
    "crates/cli",
]

[workspace.dependencies]
serde = "1"
`), 0o644)
	os.MkdirAll(filepath.Join(dir, "crates", "core"), 0o755)
	os.MkdirAll(filepath.Join(dir, "crates", "cli"), 0o755)

	ws := DetectWorkspaces(dir)
	if len(ws) != 2 {
		t.Fatalf("expected 2 workspaces, got %d: %+v", len(ws), ws)
	}

	roots := make(map[string]bool)
	for _, w := range ws {
		roots[w.Root] = true
		if w.Type != "cargo" {
			t.Errorf("expected cargo type, got %s", w.Type)
		}
	}
	if !roots[filepath.Join("crates", "core")] || !roots[filepath.Join("crates", "cli")] {
		t.Errorf("missing workspace roots: %+v", ws)
	}
}

func TestDetectGoWorkspaces(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.work"), []byte(`go 1.21

use (
	./cmd/server
	./pkg/lib
)
`), 0o644)
	os.MkdirAll(filepath.Join(dir, "cmd", "server"), 0o755)
	os.MkdirAll(filepath.Join(dir, "pkg", "lib"), 0o755)

	ws := DetectWorkspaces(dir)
	if len(ws) != 2 {
		t.Fatalf("expected 2 workspaces, got %d: %+v", len(ws), ws)
	}

	roots := make(map[string]bool)
	for _, w := range ws {
		roots[w.Root] = true
		if w.Type != "go-work" {
			t.Errorf("expected go-work type, got %s", w.Type)
		}
	}
	if !roots[filepath.Join("cmd", "server")] || !roots[filepath.Join("pkg", "lib")] {
		t.Errorf("missing workspace roots: %+v", ws)
	}
}

func TestDetectGoWorkSingleUse(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.work"), []byte(`go 1.21
use ./mymodule
`), 0o644)
	os.MkdirAll(filepath.Join(dir, "mymodule"), 0o755)

	ws := DetectWorkspaces(dir)
	if len(ws) != 1 || ws[0].Root != "mymodule" {
		t.Errorf("unexpected workspaces: %+v", ws)
	}
}

func TestDetectLernaWorkspaces(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "lerna.json"), []byte(`{
		"packages": ["packages/*"]
	}`), 0o644)
	os.MkdirAll(filepath.Join(dir, "packages", "api"), 0o755)

	ws := DetectWorkspaces(dir)
	if len(ws) != 1 || ws[0].Type != "lerna" {
		t.Errorf("unexpected workspaces: %+v", ws)
	}
}

func TestDetectNoWorkspaces(t *testing.T) {
	dir := t.TempDir()
	// Empty dir, no workspace configs
	ws := DetectWorkspaces(dir)
	if len(ws) != 0 {
		t.Errorf("expected no workspaces, got %+v", ws)
	}
}

func TestRelevantWorkspaces(t *testing.T) {
	workspaces := []Workspace{
		{Root: "packages/auth", Type: "npm"},
		{Root: "packages/core", Type: "npm"},
		{Root: "apps/web", Type: "npm"},
	}

	tests := []struct {
		name    string
		changed []string
		want    []string
	}{
		{
			name:    "single workspace",
			changed: []string{"packages/auth/src/login.ts"},
			want:    []string{"packages/auth"},
		},
		{
			name:    "multiple workspaces",
			changed: []string{"packages/auth/index.ts", "apps/web/app.tsx"},
			want:    []string{"packages/auth", "apps/web"},
		},
		{
			name:    "file outside all workspaces",
			changed: []string{"README.md", ".github/workflows/ci.yml"},
			want:    nil,
		},
		{
			name:    "mixed inside and outside",
			changed: []string{"README.md", "packages/core/lib.ts"},
			want:    []string{"packages/core"},
		},
		{
			name:    "no changed files",
			changed: nil,
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RelevantWorkspaces(workspaces, tt.changed)
			if len(got) != len(tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
			for i, w := range tt.want {
				if got[i] != w {
					t.Errorf("index %d: expected %s, got %s", i, w, got[i])
				}
			}
		})
	}
}

func TestRelevantWorkspacesNoConfig(t *testing.T) {
	got := RelevantWorkspaces(nil, []string{"src/main.go"})
	if got != nil {
		t.Errorf("expected nil for no workspaces, got %v", got)
	}
}

func TestPathIsUnder(t *testing.T) {
	tests := []struct {
		dir, file string
		want      bool
	}{
		{"packages/auth", "packages/auth/src/login.ts", true},
		{"packages/auth", "packages/core/lib.ts", false},
		{"packages/auth", "packages/auth-utils/index.ts", false}, // not a prefix match
		{"", "any/file.ts", true},
		{".", "any/file.ts", true},
	}
	for _, tt := range tests {
		got := pathIsUnder(tt.dir, tt.file)
		if got != tt.want {
			t.Errorf("pathIsUnder(%q, %q) = %v, want %v", tt.dir, tt.file, got, tt.want)
		}
	}
}
