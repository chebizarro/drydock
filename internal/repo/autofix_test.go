package repo

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeSuggestedPatch(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		diff    string
		wantErr string
		check   func(t *testing.T, out string)
	}{
		{
			name:    "empty diff",
			file:    "main.go",
			diff:    "",
			wantErr: "empty suggested diff",
		},
		{
			name:    "whitespace only",
			file:    "main.go",
			diff:    "   \n  ",
			wantErr: "empty suggested diff",
		},
		{
			name: "well-formed full diff",
			file: "main.go",
			diff: "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,3 +1,3 @@\n-old\n+new\n",
			check: func(t *testing.T, out string) {
				if !strings.HasPrefix(out, "diff --git") {
					t.Errorf("expected diff --git prefix, got %q", out[:20])
				}
			},
		},
		{
			name: "hunk header only (adds headers)",
			file: "src/lib.rs",
			diff: "@@ -10,3 +10,3 @@\n-old_line\n+new_line\n context",
			check: func(t *testing.T, out string) {
				if !strings.Contains(out, "diff --git a/src/lib.rs b/src/lib.rs") {
					t.Error("expected diff --git header")
				}
				if !strings.Contains(out, "--- a/src/lib.rs") {
					t.Error("expected --- header")
				}
				if !strings.Contains(out, "+++ b/src/lib.rs") {
					t.Error("expected +++ header")
				}
				if !strings.Contains(out, "@@ -10,3 +10,3 @@") {
					t.Error("expected hunk header preserved")
				}
			},
		},
		{
			name:    "bare +/- lines without hunk header",
			file:    "foo.go",
			diff:    "-old\n+new\n",
			wantErr: "missing hunk header",
		},
		{
			name:    "plain text (no diff content)",
			file:    "foo.go",
			diff:    "this is just a comment",
			wantErr: "does not resemble a unified diff",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := normalizeSuggestedPatch(tt.file, tt.diff)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want containing %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, out)
			}
		})
	}
}

func TestBuildAutoFixPatch_EmptySuggestions(t *testing.T) {
	svc := NewService(nil, NewManager(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil))), slog.New(slog.NewTextHandler(io.Discard, nil)))
	result, err := svc.BuildAutoFixPatch(context.Background(), "/nonexistent", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.AppliedCount != 0 {
		t.Errorf("expected 0 applied, got %d", result.AppliedCount)
	}
}

// initTestRepo creates a git repo with a single committed file for testing.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	// Create a test file
	testFile := filepath.Join(dir, "main.go")
	content := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"
	if err := os.WriteFile(testFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// Commit it
	for _, args := range [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "-m", "initial", "--no-verify"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestBuildAutoFixPatch_AppliesCleanly(t *testing.T) {
	repoDir := initTestRepo(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := NewManager(t.TempDir(), logger)

	// The test file has:
	// line 1: package main
	// line 2: (empty)
	// line 3: func main() {
	// line 4: \tfmt.Println("hello")
	// line 5: }

	suggestions := []AutoFixSuggestion{
		{
			FilePath: "main.go",
			SuggestedDiff: "@@ -3,3 +3,3 @@\n func main() {\n-\tfmt.Println(\"hello\")\n+\tfmt.Println(\"world\")\n }\n",
			Confidence: 0.99,
		},
	}

	result, err := mgr.buildAutoFixPatch(context.Background(), repoDir, suggestions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.AppliedCount != 1 {
		t.Errorf("expected 1 applied, got %d", result.AppliedCount)
	}
	if !strings.Contains(result.PatchDiff, "world") {
		t.Errorf("expected diff to contain 'world', got:\n%s", result.PatchDiff)
	}
	if len(result.AppliedFiles) != 1 || result.AppliedFiles[0] != "main.go" {
		t.Errorf("expected applied files [main.go], got %v", result.AppliedFiles)
	}

	// Verify the working tree is clean after autofix
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("working tree should be clean after autofix, got:\n%s", out)
	}
}

func TestBuildAutoFixPatch_SkipsMalformed(t *testing.T) {
	repoDir := initTestRepo(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := NewManager(t.TempDir(), logger)

	suggestions := []AutoFixSuggestion{
		{
			FilePath:      "main.go",
			SuggestedDiff: "this is not a diff at all",
			Confidence:    0.99,
		},
	}

	result, err := mgr.buildAutoFixPatch(context.Background(), repoDir, suggestions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.AppliedCount != 0 {
		t.Errorf("expected 0 applied for malformed diff, got %d", result.AppliedCount)
	}
}

func TestBuildAutoFixPatch_SkipsNonApplicable(t *testing.T) {
	repoDir := initTestRepo(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := NewManager(t.TempDir(), logger)

	// This diff references lines that don't exist
	suggestions := []AutoFixSuggestion{
		{
			FilePath: "main.go",
			SuggestedDiff: `@@ -100,1 +100,1 @@
-nonexistent line
+replaced line
`,
			Confidence: 0.99,
		},
	}

	result, err := mgr.buildAutoFixPatch(context.Background(), repoDir, suggestions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.AppliedCount != 0 {
		t.Errorf("expected 0 applied for non-applicable diff, got %d", result.AppliedCount)
	}
}

func TestBuildAutoFixPatch_MultiplePartialApply(t *testing.T) {
	repoDir := initTestRepo(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := NewManager(t.TempDir(), logger)

	suggestions := []AutoFixSuggestion{
		{
			// This one applies
			FilePath: "main.go",
			SuggestedDiff: "@@ -3,3 +3,3 @@\n func main() {\n-\tfmt.Println(\"hello\")\n+\tfmt.Println(\"world\")\n }\n",
			Confidence: 0.99,
		},
		{
			// This one doesn't apply (wrong context)
			FilePath: "main.go",
			SuggestedDiff: `@@ -200,1 +200,1 @@
-nonexistent
+replaced
`,
			Confidence: 0.99,
		},
	}

	result, err := mgr.buildAutoFixPatch(context.Background(), repoDir, suggestions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.AppliedCount != 1 {
		t.Errorf("expected 1 applied (partial), got %d", result.AppliedCount)
	}
}
