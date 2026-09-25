// Package testutil provides test doubles shared across drydock packages.
// This package must ONLY be imported by test files (*_test.go).
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// AssertGolden compares got against the golden file at path. With UPDATE_GOLDEN=1
// it (re)writes the golden file and returns. Otherwise a missing or mismatched
// golden fails the test with the regeneration hint, so no caller can drop it.
func AssertGolden(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run UPDATE_GOLDEN=1 to create): %v", path, err)
	}
	if got != string(want) {
		t.Fatalf("golden mismatch for %s (run UPDATE_GOLDEN=1 to update)", path)
	}
}
