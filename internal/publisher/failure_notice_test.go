package publisher

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

func TestPublishFailureNoticeIsDistinctIdempotentAndDoesNotBlockReview(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	if _, err := store.BeginReview(ctx, patchID, repoID); err != nil {
		t.Fatalf("begin review: %v", err)
	}

	fakePub := &fakeRelayPublisher{}
	svc := New(Config{DefaultRelays: []string{"wss://fallback.example"}}, store,
		fakeSigner{sk: nostr.Generate()}, fakePub, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	in := PublishFailureNoticeInput{
		PatchEventID: patchID,
		RepoID:       repoID,
		FailureStage: "fetch",
		Reason:       "commit not available after fetch attempts",
	}

	firstID, err := svc.PublishFailureNotice(ctx, in)
	if err != nil {
		t.Fatalf("publish failure notice: %v", err)
	}
	secondID, err := svc.PublishFailureNotice(ctx, in)
	if err != nil {
		t.Fatalf("repeat failure notice: %v", err)
	}
	if firstID == "" || secondID != firstID {
		t.Fatalf("notice ids = %q, %q", firstID, secondID)
	}
	if len(fakePub.calls) != 1 {
		t.Fatalf("notice relay calls = %d, want 1", len(fakePub.calls))
	}
	notice := fakePub.calls[0].event
	if notice.Kind != nostr.KindComment {
		t.Fatalf("notice kind = %d, want %d", notice.Kind, nostr.KindComment)
	}
	for key, want := range map[string]string{
		"drydock-type":  FailureNoticeType,
		"failure-stage": "fetch",
	} {
		tag := notice.Tags.Find(key)
		if tag == nil || len(tag) < 2 || tag[1] != want {
			t.Fatalf("%s tag = %v, want %q", key, tag, want)
		}
	}
	for _, want := range []string{"operational notice", "review not performed", "not an automated review", "commit not available after fetch attempts"} {
		if !strings.Contains(notice.Content, want) {
			t.Fatalf("notice content missing %q: %s", want, notice.Content)
		}
	}
	if strings.Contains(notice.Content, "Automated review summary") || strings.Contains(notice.Content, "model: none") {
		t.Fatalf("failure notice looks like an ordinary review: %s", notice.Content)
	}
	if reviewID, err := store.GetReviewEventID(ctx, patchID, repoID); err != nil || reviewID != "" {
		t.Fatalf("notice reserved ordinary review id: id=%q err=%v", reviewID, err)
	}

	// A later successful retry must still use the ordinary summary outbox.
	reviewID, err := svc.PublishReview(ctx, PublishInput{
		PatchEventID: patchID,
		RepoID:       repoID,
		Summary:      "The retry completed successfully.",
		Model:        "review-model",
		Confidence:   0.9,
	})
	if err != nil {
		t.Fatalf("publish review after notice: %v", err)
	}
	if reviewID == "" || reviewID == firstID {
		t.Fatalf("review id = %q after notice %q", reviewID, firstID)
	}
	if len(fakePub.calls) != 2 {
		t.Fatalf("relay calls after review = %d, want 2", len(fakePub.calls))
	}
	if !strings.Contains(fakePub.calls[1].event.Content, "Automated review summary") {
		t.Fatalf("later review was not an ordinary review: %s", fakePub.calls[1].event.Content)
	}
}

func TestPublishReviewRejectsModelNone(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	svc := New(Config{DefaultRelays: []string{"wss://fallback.example"}}, store,
		fakeSigner{sk: nostr.Generate()}, &fakeRelayPublisher{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	if _, err := svc.PublishReview(ctx, PublishInput{
		PatchEventID: patchID,
		RepoID:       repoID,
		Summary:      "not a model review",
		Model:        "none",
	}); err == nil || !strings.Contains(err.Error(), "PublishFailureNotice") {
		t.Fatalf("model none review error = %v", err)
	}
}
