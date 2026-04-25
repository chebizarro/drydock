// Package idegateway provides a Nostr-native protocol for IDE integration.
// IDEs subscribe to review findings for their workspace and can request reviews
// on uncommitted changes without needing a REST API.
//
// # Nostr Event Kinds
//
//   - kind 31650: IDE workspace session (addressable, a=<session-id>)
//   - kind 1651:  Review request from IDE (includes diff of uncommitted changes)
//   - kind 1652:  Review findings response (diagnostics for the IDE)
//   - kind 1653:  Fix apply request (apply a suggested fix)
//   - kind 1654:  Fix apply result (success/failure)
//
// # Workflow
//
//  1. IDE connects and announces a workspace session (kind 31650)
//  2. IDE sends a review request with uncommitted diff (kind 1651)
//  3. Drydock processes the diff and publishes findings (kind 1652)
//  4. IDE displays findings as diagnostics inline
//  5. User clicks "apply fix" → IDE sends fix request (kind 1653)
//  6. Drydock resolves the stored suggested fix and returns it (kind 1654)
package idegateway

import (
	"encoding/json"

	"drydock/internal/reviewengine"
)

// Event kinds for IDE integration.
const (
	KindIDESession        = 31650 // Addressable workspace session
	KindIDEReviewRequest  = 1651  // Review request from IDE
	KindIDEReviewResponse = 1652  // Review findings for IDE
	KindIDEFixRequest     = 1653  // Apply fix request
	KindIDEFixResponse    = 1654  // Apply fix result
)

// IDESession represents an IDE workspace session announcement.
// Published as kind 31650 with "d" tag for session ID.
type IDESession struct {
	SessionID     string   `json:"session_id"`     // Unique session identifier
	WorkspacePath string   `json:"workspace_path"` // Absolute path to workspace root
	RepoID        string   `json:"repo_id"`        // NIP-34 repo identifier (if available)
	Editor        string   `json:"editor"`         // e.g., "vscode", "neovim"
	Version       string   `json:"version"`        // Extension version
	Languages     []string `json:"languages"`      // Languages detected in workspace
}

// ReviewRequest represents an IDE request to review uncommitted changes.
// Published as kind 1651 with reference to the session.
type ReviewRequest struct {
	SessionID    string   `json:"session_id"`     // Reference to IDESession
	RequestID    string   `json:"request_id"`     // Unique request identifier
	Diff         string   `json:"diff"`           // Unified diff of uncommitted changes
	ChangedFiles []string `json:"changed_files"`  // List of changed file paths
	FullReview   bool     `json:"full_review"`    // Request full review vs quick diagnostics
}

// ReviewResponse contains findings for the IDE to display.
// Published as kind 1652 as a reply to the review request.
type ReviewResponse struct {
	RequestID    string       `json:"request_id"`   // Reference to the ReviewRequest
	SessionID    string       `json:"session_id"`   // Reference to IDESession
	Diagnostics  []Diagnostic `json:"diagnostics"`  // Findings formatted as LSP diagnostics
	Summary      string       `json:"summary"`      // Brief summary of the review
	ReviewTimeMs int64        `json:"review_time_ms"` // Time taken to process
}

// Diagnostic represents a single finding in LSP-compatible format.
// This maps directly to the LSP DiagnosticSeverity and structure.
type Diagnostic struct {
	File     string           `json:"file"`     // Relative path to the file
	Range    DiagnosticRange  `json:"range"`    // Line/character range
	Severity DiagnosticSeverity `json:"severity"` // 1=Error, 2=Warning, 3=Info, 4=Hint
	Message  string           `json:"message"`  // Description of the issue
	Source   string           `json:"source"`   // "drydock" or the model name
	Code     string           `json:"code,omitempty"` // Category code (e.g., "security")
	
	// Fix information (if available)
	HasFix       bool   `json:"has_fix,omitempty"`
	SuggestedFix string `json:"suggested_fix,omitempty"` // Diff or replacement text
	FixID        string `json:"fix_id,omitempty"`        // Identifier for the fix
}

// DiagnosticRange specifies the location in the file.
type DiagnosticRange struct {
	StartLine   int `json:"start_line"`   // 0-indexed
	StartColumn int `json:"start_column"` // 0-indexed
	EndLine     int `json:"end_line"`     // 0-indexed
	EndColumn   int `json:"end_column"`   // 0-indexed
}

// DiagnosticSeverity matches LSP severity levels.
type DiagnosticSeverity int

const (
	SeverityError   DiagnosticSeverity = 1
	SeverityWarning DiagnosticSeverity = 2
	SeverityInfo    DiagnosticSeverity = 3
	SeverityHint    DiagnosticSeverity = 4
)

// FixRequest represents an IDE request to apply a suggested fix.
// Published as kind 1653 with reference to the original finding.
type FixRequest struct {
	SessionID string `json:"session_id"` // Reference to IDESession
	RequestID string `json:"request_id"` // Unique request identifier
	FixID     string `json:"fix_id"`     // Reference to the diagnostic fix
	File      string `json:"file"`       // File to apply the fix to
}

// FixResponse contains the result of applying a fix.
// Published as kind 1654 as a reply to the fix request.
type FixResponse struct {
	RequestID string `json:"request_id"` // Reference to the FixRequest
	SessionID string `json:"session_id"` // Reference to IDESession
	Success   bool   `json:"success"`    // Whether the fix was resolved and returned
	Diff      string `json:"diff,omitempty"` // Suggested diff to apply (if successful)
	Error     string `json:"error,omitempty"` // Error message (if failed)
}

// ParseReviewRequest parses a ReviewRequest from event content.
func ParseReviewRequest(content string) (ReviewRequest, error) {
	var req ReviewRequest
	err := json.Unmarshal([]byte(content), &req)
	return req, err
}

// ParseFixRequest parses a FixRequest from event content.
func ParseFixRequest(content string) (FixRequest, error) {
	var req FixRequest
	err := json.Unmarshal([]byte(content), &req)
	return req, err
}

// SeverityFromString converts a string severity to DiagnosticSeverity.
func SeverityFromString(s string) DiagnosticSeverity {
	switch s {
	case "critical", "error":
		return SeverityError
	case "high", "warning":
		return SeverityWarning
	case "medium", "info":
		return SeverityInfo
	case "low", "hint":
		return SeverityHint
	default:
		return SeverityInfo
	}
}

// FindingToDiagnostic converts a review engine finding to an IDE diagnostic.
func FindingToDiagnostic(f reviewengine.Finding, fixID string) Diagnostic {
	return Diagnostic{
		File: f.File,
		Range: DiagnosticRange{
			StartLine:   f.Line - 1, // Convert to 0-indexed
			StartColumn: 0,
			EndLine:     f.Line - 1,
			EndColumn:   1000, // Full line
		},
		Severity:     SeverityFromString(f.Severity),
		Message:      f.Explanation,
		Source:       "drydock",
		Code:         f.Category,
		HasFix:       f.HasSuggestion(),
		SuggestedFix: f.SuggestedDiff,
		FixID:        fixID,
	}
}
