package idegateway

import (
	"testing"

	"drydock/internal/reviewengine"
)

func TestSeverityFromString(t *testing.T) {
	cases := []struct {
		input string
		want  DiagnosticSeverity
	}{
		{"critical", SeverityError},
		{"error", SeverityError},
		{"high", SeverityWarning},
		{"warning", SeverityWarning},
		{"medium", SeverityInfo},
		{"info", SeverityInfo},
		{"low", SeverityHint},
		{"hint", SeverityHint},
		{"unknown", SeverityInfo},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := SeverityFromString(tc.input)
			if got != tc.want {
				t.Errorf("SeverityFromString(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestFindingToDiagnostic(t *testing.T) {
	finding := reviewengine.Finding{
		Severity:      "high",
		Category:      "security",
		File:          "main.go",
		Line:          42,
		Evidence:      "SQL injection",
		Explanation:   "User input not sanitized",
		SuggestedDiff: "@@ -42 +42 @@\n-db.Query(input)\n+db.Query(sanitize(input))",
		Confidence:    0.95,
	}

	diag := FindingToDiagnostic(finding, "fix-123")

	if diag.File != "main.go" {
		t.Errorf("File = %q, want %q", diag.File, "main.go")
	}
	if diag.Range.StartLine != 41 { // 0-indexed
		t.Errorf("StartLine = %d, want 41", diag.Range.StartLine)
	}
	if diag.Severity != SeverityWarning {
		t.Errorf("Severity = %d, want %d", diag.Severity, SeverityWarning)
	}
	if diag.Message != "User input not sanitized" {
		t.Errorf("Message = %q, want explanation", diag.Message)
	}
	if diag.Code != "security" {
		t.Errorf("Code = %q, want %q", diag.Code, "security")
	}
	if !diag.HasFix {
		t.Error("HasFix should be true")
	}
	if diag.FixID != "fix-123" {
		t.Errorf("FixID = %q, want %q", diag.FixID, "fix-123")
	}
}

func TestParseReviewRequest(t *testing.T) {
	content := `{
		"session_id": "sess-123",
		"request_id": "req-456",
		"diff": "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new",
		"changed_files": ["main.go"],
		"full_review": true,
		"patch_event_id": "patch-123",
		"force": true
	}`

	req, err := ParseReviewRequest(content)
	if err != nil {
		t.Fatalf("ParseReviewRequest failed: %v", err)
	}

	if req.SessionID != "sess-123" {
		t.Errorf("SessionID = %q, want %q", req.SessionID, "sess-123")
	}
	if req.RequestID != "req-456" {
		t.Errorf("RequestID = %q, want %q", req.RequestID, "req-456")
	}
	if !req.FullReview {
		t.Error("FullReview should be true")
	}
	if req.PatchEventID != "patch-123" || !req.Force {
		t.Errorf("patch force fields = %q, %v", req.PatchEventID, req.Force)
	}
	if len(req.ChangedFiles) != 1 || req.ChangedFiles[0] != "main.go" {
		t.Errorf("ChangedFiles = %v, want [main.go]", req.ChangedFiles)
	}
}

func TestParseFixRequest(t *testing.T) {
	content := `{
		"session_id": "sess-123",
		"request_id": "req-789",
		"fix_id": "fix-456",
		"file": "main.go"
	}`

	req, err := ParseFixRequest(content)
	if err != nil {
		t.Fatalf("ParseFixRequest failed: %v", err)
	}

	if req.SessionID != "sess-123" {
		t.Errorf("SessionID = %q, want %q", req.SessionID, "sess-123")
	}
	if req.FixID != "fix-456" {
		t.Errorf("FixID = %q, want %q", req.FixID, "fix-456")
	}
	if req.File != "main.go" {
		t.Errorf("File = %q, want %q", req.File, "main.go")
	}
}
