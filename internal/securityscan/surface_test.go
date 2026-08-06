package securityscan

import (
	"context"
	"testing"
)

func TestSurfaceRulesAreLocators(t *testing.T) {
	rules := SurfaceRules()
	if len(rules) == 0 {
		t.Fatal("expected surface rules")
	}
	for _, rule := range rules {
		if rule.Classification != RuleClassificationSurface {
			t.Errorf("rule %s classification = %q", rule.ID, rule.Classification)
		}
		if rule.SurfaceTag == "" {
			t.Errorf("rule %s has no surface tag", rule.ID)
		}
		if rule.Pattern == nil {
			t.Errorf("rule %s has no pattern", rule.ID)
		}
	}
}

func TestLocateSurfaceTagsSecurityRelevantLocations(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "handler.go", `package main
func main() { http.HandleFunc("/users", handler) }
func handler() {
	if !authorize(user) { return }
	json.Unmarshal(body, &request)
	exec.Command("worker", arg)
	os.ReadFile(path)
	sha256.Sum256(data)
	db.QueryContext(ctx, "SELECT id FROM users")
}
`)

	result := New().LocateSurface(context.Background(), dir, []string{"handler.go"})
	if result.FilesScanned != 1 {
		t.Fatalf("files scanned = %d, want 1", result.FilesScanned)
	}

	got := make(map[string]bool)
	for _, location := range result.Locations {
		got[location.Tag] = true
		if location.File != "handler.go" || location.Line == 0 || location.Evidence == "" {
			t.Errorf("invalid location: %+v", location)
		}
	}
	for _, tag := range []string{
		"entry-point",
		"auth-check",
		"deserialization",
		"exec-subprocess",
		"file-io",
		"crypto",
		"sql",
	} {
		if !got[tag] {
			t.Errorf("missing surface tag %q", tag)
		}
	}
}

func TestSurfaceRulesDoNotProduceFindings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main\nfunc main() { exec.Command(\"worker\") }\n")

	result := NewWithRules(SurfaceRules()).ScanFiles(
		context.Background(),
		dir,
		[]string{"main.go"},
		"",
	)
	if len(result.Findings) != 0 {
		t.Fatalf("surface locator produced %d finding(s)", len(result.Findings))
	}
}
