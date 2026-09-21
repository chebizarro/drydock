package codemap

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	internalcodemap "git.sharegap.net/cascadia/drydock/internal/codemap"
	"git.sharegap.net/cascadia/drydock/internal/symbols"
)

// TestPublicTypesMirrorInternalFields is the compile-agnostic parity guard for
// the hand-written converters below: adding a field to an internal type without
// mirroring it in the public DTO makes the converter silently drop it, which no
// golden or round-trip test catches. This asserts every exported internal field
// has a same-named public field.
func TestPublicTypesMirrorInternalFields(t *testing.T) {
	cases := []struct {
		name     string
		internal reflect.Type
		public   reflect.Type
	}{
		{"Symbol", reflect.TypeOf(internalcodemap.Symbol{}), reflect.TypeOf(Symbol{})},
		{"File", reflect.TypeOf(internalcodemap.File{}), reflect.TypeOf(File{})},
		{"RankedSymbol", reflect.TypeOf(internalcodemap.RankedSymbol{}), reflect.TypeOf(RankedSymbol{})},
		{"CacheStats", reflect.TypeOf(internalcodemap.CacheStats{}), reflect.TypeOf(CacheStats{})},
		{"Map", reflect.TypeOf(internalcodemap.Map{}), reflect.TypeOf(Map{})},
		{"SourceSymbol", reflect.TypeOf(symbols.Symbol{}), reflect.TypeOf(SourceSymbol{})},
	}
	for _, tc := range cases {
		public := make(map[string]bool)
		for i := 0; i < tc.public.NumField(); i++ {
			public[tc.public.Field(i).Name] = true
		}
		for i := 0; i < tc.internal.NumField(); i++ {
			if name := tc.internal.Field(i).Name; !public[name] {
				t.Errorf("%s: internal field %q has no public mirror; the converter will silently drop it", tc.name, name)
			}
		}
	}
}

func TestLanguageAndSymbolGolden(t *testing.T) {
	language, ok := DetectLanguage("sample.go")
	if !ok || language.ID != symbols.LangFromExt(".go") {
		t.Fatalf("language = %#v, ok=%v", language, ok)
	}
	source := []byte("package sample\n\ntype Item struct{}\nfunc Build() Item { return Item{} }\n")
	got, err := Symbols("sample.go", source)
	if err != nil {
		if !TreeSitterAvailable() {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(struct {
		Language  Language
		Languages []Language
		Symbols   []SourceSymbol
	}{language, Languages(), got}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	assertCodemapGolden(t, "testdata/language_symbols.golden.json", string(encoded)+"\n")
}

func TestParserConcurrentCallsAreSerializedSafely(t *testing.T) {
	if !TreeSitterAvailable() {
		t.Skip("tree-sitter unavailable")
	}
	parser := NewParser()
	defer parser.Close()
	source := []byte("package sample\nfunc Build() {}\n")
	var wait sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			found, err := parser.Parse("sample.go", source)
			if err == nil && len(found) != 1 {
				err = fmt.Errorf("symbols = %#v", found)
			}
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepositoryMapGolden(t *testing.T) {
	if !TreeSitterAvailable() {
		t.Skip("tree-sitter unavailable")
	}
	repo := t.TempDir()
	runCodemapGit(t, repo, "init")
	runCodemapGit(t, repo, "config", "user.email", "fixture@example.test")
	runCodemapGit(t, repo, "config", "user.name", "Fixture")
	writeCodemapFixture(t, repo, "main.go", "package main\n\nfunc main() { Helper() }\nfunc Helper() {}\n")
	writeCodemapFixture(t, repo, "lib/item.go", "package lib\n\ntype Item struct{}\nfunc NewItem() Item { return Item{} }\n")
	runCodemapGit(t, repo, "add", ".")
	runCodemapGit(t, repo, "commit", "-m", "fixture")

	publicMap, err := Build(context.Background(), repo, "HEAD", BuilderOptions{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	publicMap.Cache = CacheStats{}
	encoded, err := json.MarshalIndent(publicMap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	assertCodemapGolden(t, "testdata/repository_map.golden.json", string(encoded)+"\n")
}

func writeCodemapFixture(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runCodemapGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func assertCodemapGolden(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("golden mismatch for %s\nwant:\n%s\ngot:\n%s", path, want, got)
	}
}
