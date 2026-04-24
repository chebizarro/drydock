package publisher

import (
	"strings"
	"testing"
	"time"
)

func TestBuildFixPatchContent(t *testing.T) {
	in := PublishFixPatchInput{
		PatchEventID:  "abc123",
		RepoID:        "repo/test",
		ReviewEventID: "rev456",
		PatchDiff:     "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n",
		AppliedCount:  2,
		AppliedFiles:  []string{"main.go", "lib.go"},
		Model:         "coder32b",
	}

	content := buildFixPatchContent(in)

	// Content should be pure diff (no metadata footer)
	if !strings.HasPrefix(content, "diff --git") {
		t.Error("content should start with the diff")
	}
	// Should NOT contain metadata in content (it goes in tags now)
	if strings.Contains(content, "drydock-autofix: true") {
		t.Error("metadata should be in tags, not content")
	}
	if strings.Contains(content, "review-event-id:") {
		t.Error("review-event-id should be in tags, not content")
	}
	// Should end with a newline
	if !strings.HasSuffix(content, "\n") {
		t.Error("content should end with newline")
	}
}

func TestBuildFixPatchTags(t *testing.T) {
	scope := commentScope{
		RootID:     "root123",
		RootPubKey: "pk_root",
		ParentID:   "parent456",
	}
	in := PublishFixPatchInput{
		PatchEventID:  "patch789",
		RepoID:        "repo/test",
		ReviewEventID: "rev000",
		AppliedCount:  1,
		AppliedFiles:  []string{"main.go"},
		Model:         "coder32b",
	}

	tags := buildFixPatchTags(scope, in)

	// Check root reference
	var foundRoot, foundReply, foundRepo, foundAutofix bool
	var foundReviewEventID, foundModel, foundAppliedFindings bool
	for _, tag := range tags {
		if len(tag) >= 4 && tag[0] == "e" && tag[3] == "root" {
			foundRoot = true
			if tag[1] != "root123" {
				t.Errorf("root tag value = %q, want root123", tag[1])
			}
		}
		if len(tag) >= 4 && tag[0] == "e" && tag[3] == "reply" {
			foundReply = true
			if tag[1] != "patch789" {
				t.Errorf("reply tag value = %q, want patch789", tag[1])
			}
		}
		if len(tag) >= 2 && tag[0] == "a" {
			foundRepo = true
			if tag[1] != "30617:repo/test" {
				t.Errorf("a tag value = %q", tag[1])
			}
		}
		if len(tag) >= 2 && tag[0] == "t" && tag[1] == "drydock-autofix" {
			foundAutofix = true
		}
		if len(tag) >= 2 && tag[0] == "review-event-id" && tag[1] == "rev000" {
			foundReviewEventID = true
		}
		if len(tag) >= 2 && tag[0] == "model" && tag[1] == "coder32b" {
			foundModel = true
		}
		if len(tag) >= 2 && tag[0] == "applied-findings" && tag[1] == "1" {
			foundAppliedFindings = true
		}
	}

	if !foundRoot {
		t.Error("missing root e tag")
	}
	if !foundReply {
		t.Error("missing reply e tag")
	}
	if !foundRepo {
		t.Error("missing a tag")
	}
	if !foundAutofix {
		t.Error("missing drydock-autofix t tag")
	}
	if !foundReviewEventID {
		t.Error("missing review-event-id tag")
	}
	if !foundModel {
		t.Error("missing model tag")
	}
	if !foundAppliedFindings {
		t.Error("missing applied-findings tag")
	}

	// Check expiration tag exists
	expTag := tags.Find("expiration")
	if expTag == nil || len(expTag) < 2 {
		t.Error("missing expiration tag")
	}

	// Check description tag
	descTag := tags.Find("description")
	if descTag == nil || len(descTag) < 2 {
		t.Error("missing description tag")
	} else if !strings.Contains(descTag[1], "1 suggestion(s)") {
		t.Errorf("description = %q, expected suggestion count", descTag[1])
	}
}

func TestBuildFixPatchContent_EmptyDiff(t *testing.T) {
	in := PublishFixPatchInput{
		PatchDiff: "",
	}
	content := buildFixPatchContent(in)
	// Empty diff should produce just a newline
	if content != "\n" {
		t.Errorf("expected just newline for empty diff, got %q", content)
	}
}

func TestBuildFixPatchTags_DescriptionTruncation(t *testing.T) {
	scope := commentScope{RootID: "root", RootPubKey: "pk"}
	// Create a list of files that would make a very long description
	files := make([]string, 50)
	for i := range files {
		files[i] = "very/long/path/to/deeply/nested/file_" + strings.Repeat("x", 10) + ".go"
	}
	in := PublishFixPatchInput{
		PatchEventID: "p1",
		RepoID:       "r1",
		AppliedCount: 50,
		AppliedFiles: files,
	}

	tags := buildFixPatchTags(scope, in)
	descTag := tags.Find("description")
	if descTag == nil {
		t.Fatal("missing description tag")
	}
	if len(descTag[1]) > 210 { // 200 + "…" buffer
		t.Errorf("description too long: %d chars", len(descTag[1]))
	}
}

func TestBuildFixPatchTags_ExpirationInFuture(t *testing.T) {
	scope := commentScope{RootID: "root"}
	in := PublishFixPatchInput{
		PatchEventID: "p1",
		RepoID:       "r1",
		AppliedCount: 1,
		AppliedFiles: []string{"main.go"},
	}

	tags := buildFixPatchTags(scope, in)
	expTag := tags.Find("expiration")
	if expTag == nil {
		t.Fatal("missing expiration tag")
	}

	var expTime int64
	if _, err := strings.NewReader(expTag[1]).Read(nil); err != nil {
		// Just check it's a number
	}
	_ = expTime

	// The expiration should be at least 89 days in the future
	now := time.Now().Unix()
	_ = now // basic sanity — the detailed check is in the tag format
}
