package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"drydock/internal/metrics"

	"fiatjaf.com/nostr"
)

const (
	FailureNoticeType = "operational-notice"
)

type PublishFailureNoticeInput struct {
	PatchEventID string
	RepoID       string
	FailureStage string
	Reason       string
}

// PublishFailureNotice emits a mechanically and visibly distinct operational
// notice. It deliberately does not call PublishReview, insert a review event,
// or mark the review as published.
func (s *Service) PublishFailureNotice(ctx context.Context, in PublishFailureNoticeInput) (string, error) {
	if strings.TrimSpace(in.PatchEventID) == "" {
		return "", errors.New("patch event id is required")
	}
	if strings.TrimSpace(in.RepoID) == "" {
		return "", errors.New("repo id is required")
	}
	failureStage := strings.ToLower(strings.TrimSpace(in.FailureStage))
	if failureStage == "" {
		failureStage = "apply"
	}

	patchRec, err := s.store.GetPatchEvent(ctx, in.PatchEventID)
	if err != nil {
		return "", err
	}
	var patchEvent nostr.Event
	if err := json.Unmarshal([]byte(patchRec.RawEvent), &patchEvent); err != nil {
		return "", fmt.Errorf("decode patch event: %w", err)
	}
	scope, err := deriveCommentScope(patchEvent)
	if err != nil {
		return "", err
	}
	if scope.RootID != patchEvent.ID.Hex() {
		if rootAuthor, lookupErr := s.store.GetPatchAuthorPubKey(ctx, scope.RootID); lookupErr == nil && strings.TrimSpace(rootAuthor) != "" {
			scope.RootPubKey = rootAuthor
		}
	}
	relays, err := s.resolveRelays(ctx, in.PatchEventID, in.RepoID)
	if err != nil {
		return "", err
	}

	notice, delivered, found, err := s.store.GetReviewFailureNotice(ctx, in.PatchEventID, in.RepoID)
	if err != nil {
		return "", err
	}
	if !found {
		expiresAt := strconv.FormatInt(time.Now().Add(s.cfg.DefaultTTL).Unix(), 10)
		notice = nostr.Event{
			Kind:      nostr.KindComment,
			CreatedAt: nostr.Now(),
			Tags: append(buildCommonTags(scope, in.RepoID, expiresAt, PublishInput{}),
				nostr.Tag{"alt", "Drydock operational apply-failure notice; no review was performed"},
				nostr.Tag{"drydock-type", FailureNoticeType},
				nostr.Tag{"failure-stage", failureStage},
			),
			Content: buildFailureNoticeContent(in, failureStage),
		}
		if err := s.signer.SignEvent(ctx, &notice); err != nil {
			return "", fmt.Errorf("sign review failure notice: %w", err)
		}
		notice, delivered, err = s.store.ReserveReviewFailureNotice(ctx, in.PatchEventID, in.RepoID, notice)
		if err != nil {
			return "", err
		}
	}
	if delivered {
		return notice.ID.Hex(), nil
	}

	metrics.FailureNoticeAttempts.Inc()
	if err := s.publish.Publish(ctx, relays, notice); err != nil {
		metrics.FailureNoticeFailures.Inc()
		return "", fmt.Errorf("publish review failure notice: %w", err)
	}
	if err := s.store.MarkReviewFailureNoticeDelivered(ctx, in.PatchEventID, in.RepoID); err != nil {
		metrics.FailureNoticeFailures.Inc()
		return "", err
	}
	metrics.FailureNoticeSuccesses.Inc()
	return notice.ID.Hex(), nil
}

func buildFailureNoticeContent(in PublishFailureNoticeInput, failureStage string) string {
	reason := plainText(strings.TrimSpace(in.Reason))
	return fmt.Sprintf(
		"Drydock operational notice — review not performed.\n\n"+
			"Status: apply-failure\n\n"+
			"Drydock could not prepare this patch for review. This is an operational status notice, not an automated review or model assessment.\n\n"+
			"Reason: %s\n\n"+
			"The patch may need to be rebased or updated before review can run.\n\n"+
			"---\nnotice-type: %s\nfailure-stage: %s\nrepo-id: %s\npatch-event-id: %s\n",
		reason, FailureNoticeType, failureStage, in.RepoID, in.PatchEventID,
	)
}
