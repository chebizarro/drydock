package contextbuilder

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/repoconfig"
	"git.sharegap.net/cascadia/drydock/internal/targetidentity"
)

func TestAddedLinesNumbering(t *testing.T) {
	for _, tc := range []struct {
		name, patch, want string
	}{
		{"multiple hunks and replacement", "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -2,3 +2,4 @@\n keep\n-old\n+first\n+second\n end\n@@ -20,2 +21,2 @@\n-old2\n+third\n last\n", "a.go:3: first\na.go:4: second\na.go:21: third\n"},
		{"insertion", "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -4,0 +5,2 @@\n+one\n+two\n", "a.go:5: one\na.go:6: two\n"},
		{"deletion then addition", "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,2 @@\n-gone\n keep\n-old\n+new\n", "a.go:2: new\n"},
		{"new file no final newline", "diff --git a/new.go b/new.go\nnew file mode 100644\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1,2 @@\n+package main\n+// end\n\\ No newline at end of file\n", "new.go:1: package main\nnew.go:2: // end\n"},
		{"rename", "diff --git a/old.go b/new.go\nsimilarity index 80%\nrename from old.go\nrename to new.go\n--- a/old.go\n+++ b/new.go\n@@ -7 +7 @@\n-old\n+new\n", "new.go:7: new\n"},
		{"deleted file", "diff --git a/old.go b/old.go\ndeleted file mode 100644\n--- a/old.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-old\n", ""},
		{"binary", "diff --git a/image.png b/image.png\nBinary files a/image.png and b/image.png differ\n", ""},
		{"rename only", "diff --git a/old.go b/new.go\nsimilarity index 100%\nrename from old.go\nrename to new.go\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (addedLinesProvider{}).Build(context.Background(), BuildInput{PatchEventContent: tc.patch, IncludeAddedLines: true})
			if err != nil || got != tc.want {
				t.Fatalf("Build = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestAddedLinesToggleAndIdentity(t *testing.T) {
	patch := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	builder, err := NewBuilder(DefaultTokenBudget, ApproxTokenCounter{}, DefaultProviders())
	if err != nil {
		t.Fatal(err)
	}
	var bundles []ContextBundle
	for _, config := range []string{"", "context:\n  include_added_lines: true\n", "context:\n  include_added_lines: false\n"} {
		cfg, err := repoconfig.Parse([]byte(config))
		if err != nil {
			t.Fatal(err)
		}
		bundle, err := builder.Build(context.Background(), BuildInput{PatchEventContent: patch, IncludeAddedLines: cfg.Context.IncludeAddedLines})
		if err != nil {
			t.Fatal(err)
		}
		bundles = append(bundles, bundle)
	}
	if slices.Contains(bundles[0].LayersUsed, LayerAddedLines) || bundles[0].Content != bundles[2].Content {
		t.Fatal("added-lines must default off and explicit false must match")
	}
	if !slices.Contains(bundles[1].LayersUsed, LayerAddedLines) || !strings.Contains(bundles[1].Content, "a.go:1: new") {
		t.Fatal("enabled layer missing")
	}
	if len(bundles[1].LayersUsed) < 2 || bundles[1].LayersUsed[0] != LayerPatchDiff || bundles[1].LayersUsed[1] != LayerAddedLines {
		t.Fatalf("unexpected ordering: %v", bundles[1].LayersUsed)
	}
	for _, bundle := range bundles {
		patchLayer := strings.Split(strings.TrimPrefix(bundle.Content, "## patch\n"), "\n\n## ")[0]
		if patchLayer != strings.TrimSpace(patch) {
			t.Fatalf("patch bytes changed: %q", patchLayer)
		}
		envelope := targetidentity.New("repo", "root", "event", "remote", "", "", targetidentity.SHA256(patch), patch, bundle.Content)
		if err := envelope.VerifyMaterials(patch, bundle.Content); err != nil {
			t.Fatal(err)
		}
	}
	envelope := targetidentity.New("repo", "root", "event", "remote", "", "", "", patch, bundles[0].Content)
	if err := envelope.VerifyMaterials(patch, bundles[1].Content); err == nil {
		t.Fatal("unannotated envelope accepted annotated bundle")
	}
}

func TestAddedLinesTokenCap(t *testing.T) {
	patch := "diff --git a/new.go b/new.go\nnew file mode 100644\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1,500 @@\n" + strings.Repeat("+// abcdefghijklmnopqrstuvwxyz\n", 500)
	for _, counter := range []TokenCounter{ApproxTokenCounter{}, byteCounter{}} {
		for _, budget := range []int{DefaultTokenBudget, 80} {
			t.Run(fmt.Sprintf("%T/%d", counter, budget), func(t *testing.T) {
				builder, err := NewBuilder(budget, counter, []Provider{addedLinesProvider{}})
				if err != nil {
					t.Fatal(err)
				}
				bundle, err := builder.Build(context.Background(), BuildInput{PatchEventContent: patch, IncludeAddedLines: true})
				if err != nil {
					t.Fatal(err)
				}
				content := strings.TrimPrefix(bundle.Content, "## added-lines\n")
				if counter.Count(content) > min(AddedLinesTokenCap, budget) || !strings.HasPrefix(content, "new.go:1: // abcdef") || strings.Contains(content, "new.go:500:") {
					t.Fatalf("invalid truncation: tokens=%d", counter.Count(content))
				}
				if len(bundle.LayerStatuses) != 1 || bundle.LayerStatuses[0].Status != "truncated" {
					t.Fatalf("missing truncation status: %v", bundle.LayerStatuses)
				}
			})
		}
	}
}
