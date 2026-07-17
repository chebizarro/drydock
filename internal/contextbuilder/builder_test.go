package contextbuilder

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeProvider struct {
	name     string
	priority int
	content  string
	err      error
}

func (f fakeProvider) LayerName() string { return f.name }
func (f fakeProvider) Priority() int     { return f.priority }
func (f fakeProvider) Build(context.Context, BuildInput) (string, error) {
	return f.content, f.err
}

type byteCounter struct{}

func (byteCounter) Count(text string) int { return len(text) }

func TestBuilderSkipsOversizedLayerAndContinuesPacking(t *testing.T) {
	b := &Builder{
		TokenBudget: 10,
		Counter:     byteCounter{},
		Providers: []Provider{
			fakeProvider{name: LayerPatchDiff, priority: 1, content: "12345"},
			fakeProvider{name: LayerSymbolsCallsites, priority: 3, content: "123456"},
			fakeProvider{name: LayerTests, priority: 4, content: "xx"},
		},
	}

	out, err := b.Build(context.Background(), BuildInput{})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if out.TokenCount != 7 {
		t.Fatalf("expected token count 7, got %d", out.TokenCount)
	}
	if len(out.LayersUsed) != 2 || out.LayersUsed[0] != LayerPatchDiff || out.LayersUsed[1] != LayerTests {
		t.Fatalf("unexpected layers used: %#v", out.LayersUsed)
	}
	if len(out.LayersDropped) != 1 || out.LayersDropped[0] != LayerSymbolsCallsites {
		t.Fatalf("unexpected dropped layers: %#v", out.LayersDropped)
	}
}

func TestBuilderSurfacesDegradedLayerStatus(t *testing.T) {
	b := &Builder{
		TokenBudget: 100,
		Counter:     byteCounter{},
		Providers: []Provider{
			fakeProvider{name: LayerCommitHistory, priority: 6, err: &LayerWarning{Err: errors.New("git unavailable")}},
		},
	}

	out, err := b.Build(context.Background(), BuildInput{})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if len(out.LayerStatuses) != 1 || out.LayerStatuses[0].Status != "degraded" || !strings.Contains(out.LayerStatuses[0].Message, "git unavailable") {
		t.Fatalf("degraded status not surfaced: %#v", out.LayerStatuses)
	}
}

func TestBuilderExcludedFilesNotification(t *testing.T) {
	// Minimal unified diff touching a lock file and a normal Go file.
	patch := "diff --git a/go.sum b/go.sum\n" +
		"--- a/go.sum\n" +
		"+++ b/go.sum\n" +
		"@@ -1,1 +1,2 @@\n" +
		" existing\n" +
		"+added\n" +
		"diff --git a/package-lock.json b/package-lock.json\n" +
		"--- a/package-lock.json\n" +
		"+++ b/package-lock.json\n" +
		"@@ -1,1 +1,2 @@\n" +
		" {}\n" +
		"+{\"a\": 1}\n" +
		"diff --git a/main.go b/main.go\n" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,1 +1,2 @@\n" +
		" package main\n" +
		"+// comment\n"

	b := &Builder{
		TokenBudget: 100_000,
		Counter:     byteCounter{},
		Providers: []Provider{
			// Minimal provider that returns something so we have content.
			fakeProvider{name: "test", priority: 1, content: "ok"},
		},
	}

	out, err := b.Build(context.Background(), BuildInput{PatchEventContent: patch})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	// Should detect package-lock.json as excluded.
	if len(out.ExcludedFiles) != 1 {
		t.Fatalf("expected 1 excluded file, got %d: %v", len(out.ExcludedFiles), out.ExcludedFiles)
	}
	if out.ExcludedFiles[0] != "package-lock.json" {
		t.Fatalf("expected package-lock.json excluded, got %q", out.ExcludedFiles[0])
	}

	// Content should contain the notification.
	if !strings.Contains(out.Content, "excluded-files") {
		t.Fatal("expected excluded-files notification in content")
	}
	if !strings.Contains(out.Content, "package-lock.json") {
		t.Fatal("expected package-lock.json mentioned in notification")
	}
}

func TestBuilderExcludedFilesOmittedWhenBudgetExhausted(t *testing.T) {
	patch := "diff --git a/package-lock.json b/package-lock.json\n" +
		"--- a/package-lock.json\n" +
		"+++ b/package-lock.json\n" +
		"@@ -1,1 +1,2 @@\n" +
		" {}\n" +
		"+{\"a\": 1}\n"

	b := &Builder{
		// Budget barely fits the provider content (2 bytes) but not the note.
		TokenBudget: 4,
		Counter:     byteCounter{},
		Providers: []Provider{
			fakeProvider{name: "test", priority: 1, content: "ok"},
		},
	}

	out, err := b.Build(context.Background(), BuildInput{PatchEventContent: patch})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	// ExcludedFiles should still be populated (metadata).
	if len(out.ExcludedFiles) != 1 {
		t.Fatalf("expected 1 excluded file, got %d", len(out.ExcludedFiles))
	}
	// But notification should NOT be in content because of budget.
	if strings.Contains(out.Content, "excluded-files") {
		t.Fatal("notification should be omitted when budget is exhausted")
	}
}

func TestBuilderNoExcludedFilesWhenAllSourceCode(t *testing.T) {
	patch := "diff --git a/main.go b/main.go\n" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,1 +1,2 @@\n" +
		" package main\n" +
		"+// comment\n"

	b := &Builder{
		TokenBudget: 100_000,
		Counter:     byteCounter{},
		Providers: []Provider{
			fakeProvider{name: "test", priority: 1, content: "ok"},
		},
	}

	out, err := b.Build(context.Background(), BuildInput{PatchEventContent: patch})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	if len(out.ExcludedFiles) != 0 {
		t.Fatalf("expected no excluded files, got %v", out.ExcludedFiles)
	}
	if strings.Contains(out.Content, "excluded-files") {
		t.Fatal("should not include excluded-files notification")
	}
}

func TestBuilderTestCoverageGapsExtracted(t *testing.T) {
	// The tests layer content includes finding-candidate markers.
	testsContent := "### Foo\nfoo_test.go:12: func TestFoo ...\n\n" +
		TestCoverageGapPrefix + "Bar\n" +
		TestCoverageGapPrefix + "Baz"

	b := &Builder{
		TokenBudget: 100_000,
		Counter:     byteCounter{},
		Providers: []Provider{
			fakeProvider{name: LayerTests, priority: 4, content: testsContent},
		},
	}

	out, err := b.Build(context.Background(), BuildInput{})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	if len(out.TestCoverageGaps) != 2 {
		t.Fatalf("expected 2 coverage gaps, got %d: %v", len(out.TestCoverageGaps), out.TestCoverageGaps)
	}
	if out.TestCoverageGaps[0] != "Bar" || out.TestCoverageGaps[1] != "Baz" {
		t.Fatalf("unexpected gaps: %v", out.TestCoverageGaps)
	}
}

func TestBuilderNoCoverageGapsWhenAllCovered(t *testing.T) {
	testsContent := "### Foo\nfoo_test.go:12: func TestFoo ...\n\n" +
		"### Bar\nbar_test.go:5: func TestBar ..."

	b := &Builder{
		TokenBudget: 100_000,
		Counter:     byteCounter{},
		Providers: []Provider{
			fakeProvider{name: LayerTests, priority: 4, content: testsContent},
		},
	}

	out, err := b.Build(context.Background(), BuildInput{})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	if len(out.TestCoverageGaps) != 0 {
		t.Fatalf("expected no coverage gaps, got %v", out.TestCoverageGaps)
	}
}

func TestBuilderDeterministicOrderByPriorityThenName(t *testing.T) {
	b := &Builder{
		TokenBudget: 100,
		Counter:     byteCounter{},
		Providers: []Provider{
			fakeProvider{name: "zeta", priority: 3, content: "1"},
			fakeProvider{name: "alpha", priority: 3, content: "1"},
			fakeProvider{name: "beta", priority: 1, content: "1"},
		},
	}
	out, err := b.Build(context.Background(), BuildInput{})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	want := []string{"beta", "alpha", "zeta"}
	if len(out.LayersUsed) != len(want) {
		t.Fatalf("expected %d used layers, got %d", len(want), len(out.LayersUsed))
	}
	for i := range want {
		if out.LayersUsed[i] != want[i] {
			t.Fatalf("unexpected order at %d: want %s got %s", i, want[i], out.LayersUsed[i])
		}
	}
}
