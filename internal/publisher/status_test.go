package publisher

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"drydock/internal/db"
	"drydock/internal/reviewengine"

	"fiatjaf.com/nostr"
)

func newStatusTestService(t *testing.T, ctx context.Context) (*Service, *db.Store, *fakeRelayPublisher) {
	t.Helper()
	store := mustStore(t, ctx)
	sk := nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000001")
	relay := &fakeRelayPublisher{}
	svc := New(Config{
		DefaultRelays:       []string{"wss://relay.test"},
		DetailSeverityFloor: "high",
	}, store, fakeSigner{sk: sk}, relay, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, store, relay
}

func statusTestFindings(severities ...string) []reviewengine.Finding {
	var findings []reviewengine.Finding
	for _, sev := range severities {
		findings = append(findings, reviewengine.Finding{
			Severity:    sev,
			Category:    "correctness",
			File:        "main.go",
			Line:        10,
			Explanation: "test finding",
			Confidence:  0.95,
		})
	}
	return findings
}

func TestPublishStatusDisabledPolicy(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newStatusTestService(t, ctx)

	result, err := svc.PublishStatus(ctx, PublishStatusInput{
		PatchEventID: "abcd",
		RepoID:       "repo-1",
		Policy:       StatusPolicy{Enabled: false},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Published {
		t.Error("expected not published when disabled")
	}
	if result.Reason != "disabled" {
		t.Errorf("expected reason 'disabled', got %q", result.Reason)
	}
}

func TestPublishStatusSuperseded(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newStatusTestService(t, ctx)

	result, err := svc.PublishStatus(ctx, PublishStatusInput{
		PatchEventID: "abcd",
		RepoID:       "repo-1",
		Superseded:   true,
		Policy:       StatusPolicy{Enabled: true, OpenSeverityFloor: "critical", MinConfidence: 0.90},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Published {
		t.Error("expected not published when superseded")
	}
	if result.Reason != "superseded" {
		t.Errorf("expected reason 'superseded', got %q", result.Reason)
	}
}

func TestPublishStatusNoBlockingFindings(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newStatusTestService(t, ctx)

	patchID, repoID := seedRepoAndPatch(t, ctx, store)

	result, err := svc.PublishStatus(ctx, PublishStatusInput{
		PatchEventID: patchID,
		RepoID:       repoID,
		Findings:     statusTestFindings("low", "info"), // below critical
		Confidence:   0.95,
		Policy:       StatusPolicy{Enabled: true, OpenSeverityFloor: "critical", MinConfidence: 0.90},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Published {
		t.Error("expected not published when no blocking findings")
	}
	if result.Reason != "no_blocking_findings" {
		t.Errorf("expected reason 'no_blocking_findings', got %q", result.Reason)
	}
}

func TestPublishStatusLowConfidence(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newStatusTestService(t, ctx)

	patchID, repoID := seedRepoAndPatch(t, ctx, store)

	result, err := svc.PublishStatus(ctx, PublishStatusInput{
		PatchEventID: patchID,
		RepoID:       repoID,
		Findings:     statusTestFindings("critical"),
		Confidence:   0.50, // below threshold
		Policy:       StatusPolicy{Enabled: true, OpenSeverityFloor: "critical", MinConfidence: 0.90},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Published {
		t.Error("expected not published with low confidence")
	}
	if result.Reason != "low_confidence" {
		t.Errorf("expected reason 'low_confidence', got %q", result.Reason)
	}
}

func TestPublishStatusSuccess(t *testing.T) {
	ctx := context.Background()
	svc, store, relay := newStatusTestService(t, ctx)

	patchID, repoID := seedRepoAndPatch(t, ctx, store)

	// Ensure the review_log entry exists (as it would in the real pipeline).
	seedReviewLog(t, ctx, store, patchID, repoID, "rev-event-1")

	result, err := svc.PublishStatus(ctx, PublishStatusInput{
		PatchEventID:  patchID,
		RepoID:        repoID,
		ReviewEventID: "rev-event-1",
		Summary:       "Found critical issues in patch",
		Findings:      statusTestFindings("critical", "high"),
		Model:         "test-model",
		Confidence:    0.95,
		Policy:        StatusPolicy{Enabled: true, OpenSeverityFloor: "high", MinConfidence: 0.90},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Published {
		t.Fatalf("expected published, got reason: %s", result.Reason)
	}
	if result.Kind != nostr.KindStatusOpen {
		t.Errorf("expected kind 1630, got %d", result.Kind)
	}
	if result.EventID == "" {
		t.Error("expected non-empty event ID")
	}

	// Verify the relay received the status event.
	if len(relay.calls) == 0 {
		t.Fatal("expected relay publish call")
	}
	statusEvt := relay.calls[len(relay.calls)-1].event
	if statusEvt.Kind != nostr.KindStatusOpen {
		t.Errorf("expected kind 1630, got %d", statusEvt.Kind)
	}

	// Verify tags.
	rootTag := findTagValue(statusEvt.Tags, "e")
	if rootTag == "" {
		t.Error("expected e tag with root event ID")
	}
	aTag := findTagValue(statusEvt.Tags, "a")
	if aTag == "" || !strings.HasPrefix(aTag, "30617:") {
		t.Errorf("expected a tag with repo address, got %q", aTag)
	}

	// Verify content.
	if !strings.Contains(statusEvt.Content, "changes requested") {
		t.Error("expected 'changes requested' in content")
	}
	if !strings.Contains(statusEvt.Content, "review-event-id: rev-event-1") {
		t.Error("expected review-event-id in footer")
	}
	if !strings.Contains(statusEvt.Content, "blocking-findings: 2") {
		t.Error("expected blocking-findings count in footer")
	}

	// Verify persisted in review_log.
	eventID, kind, _, err := store.GetPublishedStatusEvent(ctx, patchID, repoID)
	if err != nil {
		t.Fatalf("get published status: %v", err)
	}
	if eventID == "" {
		t.Error("expected status event ID in review_log")
	}
	if kind != int(nostr.KindStatusOpen) {
		t.Errorf("expected kind 1630, got %d", kind)
	}
}

func TestPublishStatusDuplicateSuppression(t *testing.T) {
	ctx := context.Background()
	svc, store, relay := newStatusTestService(t, ctx)

	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	seedReviewLog(t, ctx, store, patchID, repoID, "rev-event-1")

	input := PublishStatusInput{
		PatchEventID:  patchID,
		RepoID:        repoID,
		ReviewEventID: "rev-event-1",
		Findings:      statusTestFindings("critical"),
		Model:         "test-model",
		Confidence:    0.95,
		Policy:        StatusPolicy{Enabled: true, OpenSeverityFloor: "critical", MinConfidence: 0.90},
	}

	// First publish should succeed.
	result1, err := svc.PublishStatus(ctx, input)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if !result1.Published {
		t.Fatalf("first publish should succeed, reason: %s", result1.Reason)
	}
	firstCallCount := len(relay.calls)

	// Second publish should be suppressed (already_published).
	result2, err := svc.PublishStatus(ctx, input)
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if !result2.Published {
		t.Fatal("second should report published")
	}
	if result2.Reason != "already_published" {
		t.Errorf("expected reason 'already_published', got %q", result2.Reason)
	}
	if len(relay.calls) != firstCallCount {
		t.Error("expected no additional relay calls on duplicate")
	}
}

func TestPublishStatusRootAlreadyClosed(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newStatusTestService(t, ctx)

	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	seedReviewLog(t, ctx, store, patchID, repoID, "rev-event-1")

	// Simulate root being closed by inserting a status event.
	rootID := "3333333333333333333333333333333333333333333333333333333333333333"
	closedEvt := nostr.Event{
		ID:        nostr.MustIDFromHex("4444444444444444444444444444444444444444444444444444444444444444"),
		PubKey:    nostr.MustPubKeyFromHex("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"),
		Kind:      nostr.KindStatusClosed,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", rootID, "", "root"},
			{"a", "30617:" + repoID},
		},
	}
	_ = store.UpsertRootStatus(ctx, closedEvt)

	result, err := svc.PublishStatus(ctx, PublishStatusInput{
		PatchEventID:  patchID,
		RepoID:        repoID,
		ReviewEventID: "rev-event-1",
		Findings:      statusTestFindings("critical"),
		Confidence:    0.95,
		Policy:        StatusPolicy{Enabled: true, OpenSeverityFloor: "critical", MinConfidence: 0.90},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Published {
		t.Error("expected not published when root is closed")
	}
	if result.Reason != "root_already_terminal" {
		t.Errorf("expected reason 'root_already_terminal', got %q", result.Reason)
	}
}

func TestPublishStatusUnauthorized(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newStatusTestService(t, ctx)

	// Create a repo owned by a different key than the signer.
	differentOwner := nostr.MustPubKeyFromHex("5555555555555555555555555555555555555555555555555555555555555555")
	repoEvt := nostr.Event{
		ID:        nostr.MustIDFromHex("6666666666666666666666666666666666666666666666666666666666666666"),
		PubKey:    differentOwner,
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "other-repo"},
			{"relays", "wss://relay.test"},
		},
	}
	if err := store.UpsertRepositoryAnnouncement(ctx, repoEvt); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	// Create a patch from yet another author.
	patchAuthor := nostr.MustPubKeyFromHex("7777777777777777777777777777777777777777777777777777777777777777")
	patchEvt := nostr.Event{
		ID:        nostr.MustIDFromHex("8888888888888888888888888888888888888888888888888888888888888888"),
		PubKey:    patchAuthor,
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + differentOwner.Hex() + ":other-repo"},
			{"e", "9999999999999999999999999999999999999999999999999999999999999999", "", "root"},
		},
		Content: "diff --git a/file.go b/file.go\n",
	}
	if err := store.InsertPatchEvent(ctx, patchEvt); err != nil {
		t.Fatalf("seed patch: %v", err)
	}
	if err := store.RecordPatchEventRelay(ctx, patchEvt.ID.Hex(), "wss://relay.test"); err != nil {
		t.Fatalf("seed relay: %v", err)
	}
	patchID := patchEvt.ID.Hex()
	repoID := db.RepoIDFromPatch(patchEvt)
	seedReviewLog(t, ctx, store, patchID, repoID, "rev-1")

	result, err := svc.PublishStatus(ctx, PublishStatusInput{
		PatchEventID:  patchID,
		RepoID:        repoID,
		ReviewEventID: "rev-1",
		Findings:      statusTestFindings("critical"),
		Confidence:    0.95,
		Policy:        StatusPolicy{Enabled: true, OpenSeverityFloor: "critical", MinConfidence: 0.90},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Published {
		t.Error("expected not published when unauthorized")
	}
	if result.Reason != "unauthorized" {
		t.Errorf("expected reason 'unauthorized', got %q", result.Reason)
	}
}

// seedReviewLog creates a review_log entry in published state for testing.
func seedReviewLog(t *testing.T, ctx context.Context, store *db.Store, patchID, repoID, reviewEventID string) {
	t.Helper()
	db := store.DB()
	now := int64(1000)
	_, err := db.ExecContext(ctx,
		`INSERT INTO review_log(patch_event_id, repo_id, status, review_event_id, created_at, updated_at)
		 VALUES (?, ?, 'published', ?, ?, ?)`,
		patchID, repoID, reviewEventID, now, now)
	if err != nil {
		t.Fatalf("seed review log: %v", err)
	}
}

func TestBuildStatusContent(t *testing.T) {
	content := buildStatusContent(PublishStatusInput{
		PatchEventID:  "patch-1",
		ReviewEventID: "review-1",
		Summary:       "Found issues",
		Model:         "qwen-coder",
		Confidence:    0.93,
		Policy:        StatusPolicy{OpenSeverityFloor: "critical"},
	}, 3)

	if !strings.Contains(content, "changes requested") {
		t.Error("missing 'changes requested'")
	}
	if !strings.Contains(content, "3 finding(s) at or above critical") {
		t.Error("missing finding count")
	}
	if !strings.Contains(content, "review-event-id: review-1") {
		t.Error("missing review-event-id footer")
	}
	if !strings.Contains(content, "confidence: 0.93") {
		t.Error("missing confidence footer")
	}
	if !strings.Contains(content, "decision: changes-requested") {
		t.Error("missing decision footer")
	}
}

func TestBuildStatusTags(t *testing.T) {
	rootID := "aaaa"
	rootPK := "bbbb"
	patchPK := nostr.MustPubKeyFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	scope := commentScope{
		RootID:     rootID,
		RootPubKey: rootPK,
	}
	patchEvt := nostr.Event{PubKey: patchPK}

	tags := buildStatusTags(scope, "owner:repo-1", patchEvt)

	// Check e tag with root marker.
	eTag := tags.Find("e")
	if eTag == nil || len(eTag) < 4 || eTag[1] != rootID || eTag[3] != "root" {
		t.Errorf("expected e tag with root marker, got %v", eTag)
	}

	// Check a tag.
	aTag := findTagValue(tags, "a")
	if !strings.HasPrefix(aTag, "30617:") {
		t.Errorf("expected a tag with 30617: prefix, got %q", aTag)
	}

	// Check p tags.
	pTags := []string{}
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == "p" {
			pTags = append(pTags, tag[1])
		}
	}
	if len(pTags) != 2 {
		t.Errorf("expected 2 p tags, got %d: %v", len(pTags), pTags)
	}
}

func TestPublishStatusCleanReReviewSupersedesAdvisory(t *testing.T) {
	ctx := context.Background()
	svc, store, relay := newStatusTestService(t, ctx)

	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	seedReviewLog(t, ctx, store, patchID, repoID, "rev-1")

	// First: publish a "changes requested" status.
	result1, err := svc.PublishStatus(ctx, PublishStatusInput{
		PatchEventID:  patchID,
		RepoID:        repoID,
		ReviewEventID: "rev-1",
		Findings:      statusTestFindings("critical"),
		Confidence:    0.95,
		Policy:        StatusPolicy{Enabled: true, OpenSeverityFloor: "critical", MinConfidence: 0.90},
	})
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if !result1.Published {
		t.Fatalf("expected first publish to succeed, reason: %s", result1.Reason)
	}

	// Now simulate a second patch+review with no blocking findings.
	// We need a new patch event and review_log entry for the re-review.
	patchEvt2 := nostr.Event{
		ID:        nostr.MustIDFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		PubKey:    nostr.MustPubKeyFromHex("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798:repo-1"},
			{"e", "3333333333333333333333333333333333333333333333333333333333333333", "", "root"},
		},
		Content: "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-package main\n+package main // v2\n",
	}
	if err := store.InsertPatchEvent(ctx, patchEvt2); err != nil {
		t.Fatalf("seed patch2: %v", err)
	}
	if err := store.RecordPatchEventRelay(ctx, patchEvt2.ID.Hex(), "wss://relay.test"); err != nil {
		t.Fatalf("seed relay2: %v", err)
	}
	patch2ID := patchEvt2.ID.Hex()
	repo2ID := db.RepoIDFromPatch(patchEvt2)
	seedReviewLog(t, ctx, store, patch2ID, repo2ID, "rev-2")

	// Clean re-review: no blocking findings.
	result2, err := svc.PublishStatus(ctx, PublishStatusInput{
		PatchEventID:  patch2ID,
		RepoID:        repo2ID,
		ReviewEventID: "rev-2",
		Summary:       "All good, no issues found",
		Findings:      statusTestFindings("info", "low"), // below critical
		Confidence:    0.95,
		Policy:        StatusPolicy{Enabled: true, OpenSeverityFloor: "critical", MinConfidence: 0.90},
	})
	if err != nil {
		t.Fatalf("clean re-review: %v", err)
	}
	if !result2.Published {
		t.Fatalf("expected clean re-review to publish, reason: %s", result2.Reason)
	}
	if result2.Kind != nostr.KindStatusOpen {
		t.Errorf("expected kind 1630, got %d", result2.Kind)
	}

	// Verify content indicates clean outcome.
	lastEvt := relay.calls[len(relay.calls)-1].event
	if !strings.Contains(lastEvt.Content, "no blocking findings") {
		t.Error("expected 'no blocking findings' in clean status content")
	}
	if !strings.Contains(lastEvt.Content, "decision: clean") {
		t.Error("expected 'decision: clean' in footer")
	}
}

func TestPublishStatusCleanReviewSkipsWithoutPriorAdvisory(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newStatusTestService(t, ctx)

	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	seedReviewLog(t, ctx, store, patchID, repoID, "rev-1")

	// Clean review with no prior 1630 advisory — should skip.
	result, err := svc.PublishStatus(ctx, PublishStatusInput{
		PatchEventID:  patchID,
		RepoID:        repoID,
		ReviewEventID: "rev-1",
		Findings:      statusTestFindings("low"),
		Confidence:    0.95,
		Policy:        StatusPolicy{Enabled: true, OpenSeverityFloor: "critical", MinConfidence: 0.90},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Published {
		t.Error("expected skip when no prior advisory status exists")
	}
	if result.Reason != "no_blocking_findings" {
		t.Errorf("expected 'no_blocking_findings', got %q", result.Reason)
	}
}

func TestBuildCleanStatusContent(t *testing.T) {
	content := buildCleanStatusContent(PublishStatusInput{
		PatchEventID:  "patch-1",
		ReviewEventID: "review-1",
		Summary:       "All good",
		Model:         "qwen-coder",
		Confidence:    0.95,
	})
	if !strings.Contains(content, "no blocking findings") {
		t.Error("missing 'no blocking findings'")
	}
	if !strings.Contains(content, "decision: clean") {
		t.Error("missing decision: clean")
	}
	if !strings.Contains(content, "review-event-id: review-1") {
		t.Error("missing review-event-id")
	}
}

func TestPublishStatusHighSeverityFloor(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newStatusTestService(t, ctx)

	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	seedReviewLog(t, ctx, store, patchID, repoID, "rev-1")

	// Has high findings but floor is critical — should skip.
	result, err := svc.PublishStatus(ctx, PublishStatusInput{
		PatchEventID:  patchID,
		RepoID:        repoID,
		ReviewEventID: "rev-1",
		Findings:      statusTestFindings("high", "medium"),
		Confidence:    0.95,
		Policy:        StatusPolicy{Enabled: true, OpenSeverityFloor: "critical", MinConfidence: 0.90},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Published {
		t.Error("expected not published when findings below critical floor")
	}
	if result.Reason != "no_blocking_findings" {
		t.Errorf("expected 'no_blocking_findings', got %q", result.Reason)
	}
}
