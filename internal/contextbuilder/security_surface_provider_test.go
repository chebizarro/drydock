package contextbuilder

import (
	"context"
	"slices"
	"strings"
	"testing"

	"drydock/internal/securityscan/surface"
)

type fakeSecuritySurfaceLocator struct {
	result surface.Result
	files  []string
}

func (f *fakeSecuritySurfaceLocator) LocateSurface(_ context.Context, _ string, files []string) surface.Result {
	f.files = append([]string(nil), files...)
	return f.result
}

func TestSecuritySurfaceProviderBuild(t *testing.T) {
	locator := &fakeSecuritySurfaceLocator{result: surface.Result{
		Locations: []surface.Location{
			{Tag: "entry-point", File: "handler.go", Line: 3, Evidence: `http.HandleFunc("/users", handler)`},
			{Tag: "sql", File: "store.go", Line: 8, Evidence: `db.Query("SELECT id FROM users")`},
		},
	}}
	provider := NewSecuritySurfaceProvider(locator)
	content, err := provider.Build(context.Background(), BuildInput{
		RepoPath: "/repo",
		PatchEventContent: "diff --git a/handler.go b/handler.go\n--- a/handler.go\n+++ b/handler.go\n@@ -1 +1 @@\n-old\n+new\n" +
			"diff --git a/store.go b/store.go\n--- a/store.go\n+++ b/store.go\n@@ -1 +1 @@\n-old\n+new\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.LayerName() != "security-surface" || provider.Priority() != 2 {
		t.Fatalf("unexpected provider metadata: %s/%d", provider.LayerName(), provider.Priority())
	}
	if !slices.Equal(locator.files, []string{"handler.go", "store.go"}) {
		t.Fatalf("locator files = %v", locator.files)
	}
	for _, want := range []string{
		`[entry-point] handler.go:3 http.HandleFunc("/users", handler)`,
		`[sql] store.go:8 db.Query("SELECT id FROM users")`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("content missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(strings.ToLower(content), "finding") {
		t.Errorf("surface layer must not classify locators as findings: %s", content)
	}
}

func TestSecuritySurfaceProviderReportsIncompleteScan(t *testing.T) {
	locator := &fakeSecuritySurfaceLocator{result: surface.Result{FilesSkipped: 1, FilesErrored: 2}}
	provider := NewSecuritySurfaceProvider(locator)
	content, err := provider.Build(context.Background(), BuildInput{
		RepoPath:          "/repo",
		PatchEventContent: "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "SECURITY SURFACE INCOMPLETE: 1 file(s) skipped, 2 file(s) errored.") {
		t.Fatalf("unexpected content: %q", content)
	}
}
