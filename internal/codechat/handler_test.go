package codechat

import (
	"testing"
)

func TestParseMessage_RepoPrefix(t *testing.T) {
	h := &Handler{}

	cases := []struct {
		input    string
		wantRepo string
		wantQ    string
	}{
		{
			input:    "repo:npub1abc/myrepo what does foo do?",
			wantRepo: "npub1abc/myrepo",
			wantQ:    "what does foo do?",
		},
		{
			input:    "@npub1xyz/project how does auth work?",
			wantRepo: "npub1xyz/project",
			wantQ:    "how does auth work?",
		},
		{
			input:    "what does the main function do?",
			wantRepo: "",
			wantQ:    "what does the main function do?",
		},
		{
			input:    "repo:myrepo",
			wantRepo: "myrepo",
			wantQ:    "",
		},
		{
			input:    "",
			wantRepo: "",
			wantQ:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			result := h.parseMessage(tc.input)
			if result.repoID != tc.wantRepo {
				t.Errorf("repoID: got %q, want %q", result.repoID, tc.wantRepo)
			}
			if result.question != tc.wantQ {
				t.Errorf("question: got %q, want %q", result.question, tc.wantQ)
			}
		})
	}
}

func TestPayloadInt(t *testing.T) {
	cases := []struct {
		payload map[string]any
		key     string
		want    int
	}{
		{map[string]any{"line": float64(42)}, "line", 42},
		{map[string]any{"line": 42}, "line", 42},
		{map[string]any{"line": int64(42)}, "line", 42},
		{map[string]any{}, "line", 0},
		{map[string]any{"line": "not a number"}, "line", 0},
	}

	for _, tc := range cases {
		got := payloadInt(tc.payload, tc.key)
		if got != tc.want {
			t.Errorf("payloadInt(%v, %q) = %d, want %d", tc.payload, tc.key, got, tc.want)
		}
	}
}
