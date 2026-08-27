package context

import (
	stdcontext "context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/agenttools"
	"git.sharegap.net/cascadia/drydock/internal/contextbuilder"
	"git.sharegap.net/cascadia/drydock/internal/workspacesnapshot"
)

type goldenTokenizer struct{}

func (goldenTokenizer) Count(text string) int { return len(text) }
func (goldenTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: "bytes", Version: "test-v1", Encoding: "utf-8"}
}

type mapSource map[string]string

func (m mapSource) ReadFile(_ stdcontext.Context, path string) ([]byte, error) {
	return []byte(m[path]), nil
}

func TestGoldenParityWithLegacySelectionRenderer(t *testing.T) {
	source := t.TempDir()
	writeFixture(t, source, "changed.go", "package sample\nfunc Changed() {}\n")
	writeFixture(t, source, "extra.go", "one\ntwo\nthree\nfour\n")

	manager, err := workspacesnapshot.NewManager(workspacesnapshot.Config{StorageRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.CreateMutable(stdcontext.Background(), workspacesnapshot.MutableCopyOptions{
		WorkspacePath: source, Allowlist: []string{"."},
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := agenttools.NewSelection(agenttools.SelectionConfig{
		Snapshot: snapshot, ChangedFiles: []string{"changed.go"},
		Counter: goldenTokenizer{}, TokenBudget: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Add(stdcontext.Background(),
		agenttools.SelectionArtifact{
			Kind: agenttools.ArtifactLineRange, Path: "extra.go", StartLine: 2, EndLine: 3,
		},
		agenttools.SelectionArtifact{Kind: agenttools.ArtifactCodemap, Path: "extra.go"},
	); err != nil {
		t.Fatal(err)
	}
	legacyBundle, err := legacy.Finalize(stdcontext.Background())
	if err != nil {
		if strings.Contains(err.Error(), "tree-sitter is unavailable") {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	marker := "## file: changed.go"
	at := strings.Index(legacyBundle.Content, marker)
	if at < 0 {
		t.Fatalf("legacy content missing %q", marker)
	}

	assembler, err := NewAssembler(Options{Tokenizer: goldenTokenizer{}, TokenBudget: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := assembler.Assemble(stdcontext.Background(), Selection{
		Roots: []Root{{ID: "repository", Digest: snapshot.ManifestDigest(), Source: snapshotSource{snapshot}}},
		Entries: []Entry{
			LineSlice("repository", "extra.go", 2, 3),
			WholeFile("repository", "changed.go"),
			Codemap("repository", "extra.go"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if assembly.Content() != legacyBundle.Content[at:] {
		t.Fatalf("facade content diverged from legacy renderer\nfacade:\n%s\nlegacy suffix:\n%s",
			assembly.Content(), legacyBundle.Content[at:])
	}
}

func TestMultiRootManifestGolden(t *testing.T) {
	assembler, err := NewAssembler(Options{
		Tokenizer: goldenTokenizer{}, TokenBudget: 2_000, Headroom: 0.05,
	})
	if err != nil {
		t.Fatal(err)
	}
	selection := Selection{
		Roots: []Root{
			{ID: "alpha", Order: 20, Digest: "digest-alpha", Source: mapSource{
				"a.go":      "package alpha\nfunc Alpha() {}\n",
				"slice.txt": "zero\none\ntwo\nthree\n",
			}},
			{ID: "beta", Order: 10, Digest: "digest-beta", Source: mapSource{
				"b.go": "package beta\nfunc Beta() {}\n",
			}},
		},
		Entries: []Entry{
			LineSlice("alpha", "slice.txt", 3, 4),
			WholeFile("alpha", "a.go"),
			WholeFile("beta", "b.go"),
			LineSlice("alpha", "slice.txt", 2, 3),
		},
		Exclusions: []Exclusion{{
			RootID: "beta", Path: "generated.go", Variant: VariantWholeFile,
			Reason: "generated file policy",
		}},
	}
	first, err := assembler.Assemble(stdcontext.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	second, err := assembler.Assemble(stdcontext.Background(), Selection{
		Roots:      []Root{selection.Roots[1], selection.Roots[0]},
		Entries:    []Entry{selection.Entries[2], selection.Entries[3], selection.Entries[0], selection.Entries[1]},
		Exclusions: append([]Exclusion(nil), selection.Exclusions...),
	})
	if err != nil {
		t.Fatal(err)
	}
	firstJSON := goldenJSON(t, struct {
		Content  string
		Manifest Manifest
	}{first.Content(), first.Manifest()})
	secondJSON := goldenJSON(t, struct {
		Content  string
		Manifest Manifest
	}{second.Content(), second.Manifest()})
	if firstJSON != secondJSON {
		t.Fatalf("assembly changed with input ordering\nfirst:\n%s\nsecond:\n%s", firstJSON, secondJSON)
	}
	mutated := first.Manifest()
	mutated.Roots[0].Digest = "mutated"
	mutated.Included[0].Path = "mutated"
	fresh := first.Manifest()
	if fresh.Roots[0].Digest == "mutated" || fresh.Included[0].Path == "mutated" {
		t.Fatal("manifest accessor exposed mutable assembly state")
	}
	manifestDigest := fresh.ManifestSHA256
	fresh.ManifestSHA256 = ""
	encodedManifest, err := json.Marshal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encodedManifest)
	if hex.EncodeToString(sum[:]) != manifestDigest {
		t.Fatalf("manifest digest does not bind returned public DTO")
	}
	assertGolden(t, "testdata/multi_root.golden.json", firstJSON)
}

func TestBudgetErrorIsOwned(t *testing.T) {
	assembler, err := NewAssembler(Options{Tokenizer: goldenTokenizer{}, TokenBudget: 10, Headroom: 0.10})
	if err != nil {
		t.Fatal(err)
	}
	_, err = assembler.Assemble(stdcontext.Background(), Selection{
		Roots:   []Root{{ID: "root", Digest: "digest", Source: mapSource{"large.txt": strings.Repeat("x", 20)}}},
		Entries: []Entry{WholeFile("root", "large.txt")},
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("budget error = %v", err)
	}
}

type snapshotSource struct{ snapshot *workspacesnapshot.Snapshot }

func (s snapshotSource) ReadFile(ctx stdcontext.Context, path string) ([]byte, error) {
	return s.snapshot.ReadFile(ctx, path)
}

func writeFixture(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func goldenJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n"
}

func assertGolden(t *testing.T, path, got string) {
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

var _ contextbuilder.TokenCounter = goldenTokenizer{}
