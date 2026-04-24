// Package publisher — NIP-34 review status event publication.
//
// PublishStatus emits a kind 1630 (StatusOpen) event when a review identifies
// blocking findings above a configured severity threshold. This is opt-in via
// per-repo .drydock.yaml configuration and subject to confidence and
// authorization checks.
package publisher

import (
	"context"
	"fmt"
	"strings"
	"time"

	"drydock/internal/metrics"
	"drydock/internal/reviewengine"

	"fiatjaf.com/nostr"
)

// StatusPolicy controls whether and how NIP-34 status events are published.
// Mapped from repoconfig.StatusConfig by the pipeline.
type StatusPolicy struct {
	Enabled           bool
	OpenSeverityFloor string  // findings at or above this trigger a 1630
	MinConfidence     float64 // minimum review confidence to publish
}

// PublishStatusInput contains all data needed to decide whether and how
// to publish a NIP-34 review status event.
type PublishStatusInput struct {
	PatchEventID  string
	RepoID        string
	ReviewEventID string // the kind 1111 summary event ID
	Summary       string
	Findings      []reviewengine.Finding
	Model         string
	Confidence    float64
	Superseded    bool
	Policy        StatusPolicy
}

// PublishStatusResult describes the outcome of a status publish attempt.
type PublishStatusResult struct {
	Published bool
	EventID   string
	Kind      nostr.Kind
	Reason    string
}


// PublishStatus evaluates the review outcome against the repo's status policy
// and publishes a NIP-34 kind 1630 (StatusOpen) event when blocking findings
// are present, confidence is sufficient, and the signer is authorized.
//
// This method is best-effort: callers should log errors but not fail the
// overall review pipeline on status publication failure.
func (s *Service) PublishStatus(ctx context.Context, in PublishStatusInput) (PublishStatusResult, error) {
	skip := func(reason string) (PublishStatusResult, error) {
		metrics.StatusPublishSkipped.Inc()
		return PublishStatusResult{Reason: reason}, nil
	}

	// 1. Policy gate.
	if !in.Policy.Enabled {
		return skip("disabled")
	}

	// 2. Superseded patches should not affect root status.
	if in.Superseded {
		return skip("superseded")
	}

	// 3. Duplicate suppression.
	existingID, existingKind, _, err := s.store.GetPublishedStatusEvent(ctx, in.PatchEventID, in.RepoID)
	if err != nil {
		return PublishStatusResult{}, fmt.Errorf("check existing status: %w", err)
	}
	if existingID != "" {
		return PublishStatusResult{
			Published: true,
			EventID:   existingID,
			Kind:      nostr.Kind(existingKind),
			Reason:    "already_published",
		}, nil
	}

	// 4. Count blocking findings.
	blockingCount := 0
	for _, f := range in.Findings {
		if reviewengine.IsAtOrAboveSeverity(f.Severity, in.Policy.OpenSeverityFloor) {
			blockingCount++
		}
	}

	// 5. Confidence check — applies to both blocking and clean statuses.
	if in.Confidence < in.Policy.MinConfidence {
		return skip("low_confidence")
	}

	// 6. Load patch event and derive scope.
	patchRec, err := s.store.GetPatchEvent(ctx, in.PatchEventID)
	if err != nil {
		return PublishStatusResult{}, fmt.Errorf("get patch event: %w", err)
	}
	patchEvent, err := parsePatchEvent(patchRec.RawEvent)
	if err != nil {
		return PublishStatusResult{}, err
	}
	scope, err := deriveCommentScope(patchEvent)
	if err != nil {
		return PublishStatusResult{}, fmt.Errorf("derive scope: %w", err)
	}

	// 7. Late terminal-status guard: don't reopen applied/closed threads.
	rootKind, _, _, rootExists, err := s.store.GetRootStatus(ctx, scope.RootID, in.RepoID)
	if err != nil {
		return PublishStatusResult{}, fmt.Errorf("check root status: %w", err)
	}
	if rootExists && (rootKind == int(nostr.KindStatusApplied) || rootKind == int(nostr.KindStatusClosed)) {
		return skip("root_already_terminal")
	}

	// 8. Authorization check.
	signerPubKey, err := s.signer.GetPublicKey(ctx)
	if err != nil {
		return PublishStatusResult{}, fmt.Errorf("get signer pubkey: %w", err)
	}
	allowed, err := s.store.CanStatusAuthor(ctx, scope.RootID, in.RepoID, signerPubKey)
	if err != nil {
		return PublishStatusResult{}, fmt.Errorf("check status auth: %w", err)
	}
	if !allowed {
		return skip("unauthorized")
	}

	// 8b. Decision: blocking findings trigger "changes requested"; clean review
	// supersedes any prior advisory status. If there was never a prior status
	// and the review is clean, we skip — nothing to say.
	if blockingCount == 0 {
		// Only publish a clean-review status if there's a prior 1630 to supersede.
		if !rootExists || rootKind != int(nostr.KindStatusOpen) {
			return skip("no_blocking_findings")
		}
	}

	// 9. Resolve relays.
	relays, err := s.resolveRelays(ctx, in.PatchEventID, in.RepoID)
	if err != nil {
		return PublishStatusResult{}, fmt.Errorf("resolve relays: %w", err)
	}

	// 10. Build status event.
	var content string
	if blockingCount > 0 {
		content = buildStatusContent(in, blockingCount)
	} else {
		content = buildCleanStatusContent(in)
	}
	tags := buildStatusTags(scope, in.RepoID, patchEvent)

	statusEvent := nostr.Event{
		Kind:      nostr.KindStatusOpen,
		CreatedAt: nostr.Now(),
		Tags:      tags,
		Content:   content,
	}
	if err := s.signer.SignEvent(ctx, &statusEvent); err != nil {
		return PublishStatusResult{}, fmt.Errorf("sign status event: %w", err)
	}

	// 11. Publish.
	metrics.StatusPublishAttempts.Inc()
	if err := s.publish.Publish(ctx, relays, statusEvent); err != nil {
		metrics.StatusPublishFailures.Inc()
		return PublishStatusResult{}, fmt.Errorf("publish status event: %w", err)
	}
	metrics.StatusPublishSuccesses.Inc()

	// 12. Persist.
	if err := s.store.RecordStatusPublished(ctx, in.PatchEventID, in.RepoID, statusEvent.ID.Hex(), int(nostr.KindStatusOpen)); err != nil {
		s.logger.Warn("failed to record status publication",
			"patch_event_id", in.PatchEventID, "error", err)
	}
	// Update local lifecycle view immediately.
	if err := s.store.UpsertRootStatus(ctx, statusEvent); err != nil {
		s.logger.Warn("failed to upsert root status locally",
			"patch_event_id", in.PatchEventID, "error", err)
	}

	s.logger.Info("NIP-34 status event published",
		"patch_event_id", in.PatchEventID,
		"repo_id", in.RepoID,
		"root_id", scope.RootID,
		"status_kind", int(nostr.KindStatusOpen),
		"blocking_findings", blockingCount,
		"confidence", in.Confidence,
		"status_event_id", statusEvent.ID.Hex(),
	)

	return PublishStatusResult{
		Published: true,
		EventID:   statusEvent.ID.Hex(),
		Kind:      nostr.KindStatusOpen,
		Reason:    "published",
	}, nil
}

// parsePatchEvent decodes a raw event JSON string into a nostr.Event.
func parsePatchEvent(rawEvent string) (nostr.Event, error) {
	var evt nostr.Event
	if err := evt.UnmarshalJSON([]byte(rawEvent)); err != nil {
		return nostr.Event{}, fmt.Errorf("decode patch event: %w", err)
	}
	return evt, nil
}

// buildStatusTags creates the NIP-34 status event tags.
func buildStatusTags(scope commentScope, repoID string, patchEvent nostr.Event) nostr.Tags {
	tags := nostr.Tags{
		{"e", scope.RootID, "", "root"},
		{"a", "30617:" + repoID},
	}
	// Tag the root author.
	if scope.RootPubKey != "" {
		tags = append(tags, nostr.Tag{"p", scope.RootPubKey})
	}
	// Tag the patch author if different from root.
	patchAuthor := patchEvent.PubKey.Hex()
	if patchAuthor != "" && patchAuthor != scope.RootPubKey {
		tags = append(tags, nostr.Tag{"p", patchAuthor})
	}
	return tags
}

// buildStatusContent creates human-readable status event content.
func buildStatusContent(in PublishStatusInput, blockingCount int) string {
	var b strings.Builder
	b.WriteString("Drydock review outcome: changes requested.\n\n")
	fmt.Fprintf(&b, "%d finding(s) at or above %s require attention before this patch should proceed.\n\n",
		blockingCount, in.Policy.OpenSeverityFloor)

	// Include a truncated summary.
	summary := in.Summary
	if len(summary) > 500 {
		summary = summary[:500] + "…"
	}
	if summary != "" {
		fmt.Fprintf(&b, "Review summary: %s\n\n", plainText(summary))
	}

	// Machine-readable footer.
	b.WriteString("---\n")
	fmt.Fprintf(&b, "review-event-id: %s\n", in.ReviewEventID)
	b.WriteString("decision: changes-requested\n")
	fmt.Fprintf(&b, "blocking-severity-floor: %s\n", in.Policy.OpenSeverityFloor)
	fmt.Fprintf(&b, "blocking-findings: %d\n", blockingCount)
	fmt.Fprintf(&b, "confidence: %.2f\n", in.Confidence)
	fmt.Fprintf(&b, "model: %s\n", in.Model)
	fmt.Fprintf(&b, "patch-event-id: %s\n", in.PatchEventID)
	fmt.Fprintf(&b, "timestamp: %s\n", time.Now().UTC().Format(time.RFC3339))

	return b.String()
}



// buildCleanStatusContent creates content for a clean re-review that
// supersedes a prior "changes requested" advisory status.
func buildCleanStatusContent(in PublishStatusInput) string {
	var b strings.Builder
	b.WriteString("Drydock review outcome: no blocking findings.\n\n")
	b.WriteString("A follow-up review found no findings requiring attention. ")
	b.WriteString("This supersedes any prior advisory status on this thread.\n\n")

	summary := in.Summary
	if len(summary) > 500 {
		summary = summary[:500] + "…"
	}
	if summary != "" {
		fmt.Fprintf(&b, "Review summary: %s\n\n", plainText(summary))
	}

	b.WriteString("---\n")
	fmt.Fprintf(&b, "review-event-id: %s\n", in.ReviewEventID)
	b.WriteString("decision: clean\n")
	fmt.Fprintf(&b, "confidence: %.2f\n", in.Confidence)
	fmt.Fprintf(&b, "model: %s\n", in.Model)
	fmt.Fprintf(&b, "patch-event-id: %s\n", in.PatchEventID)
	fmt.Fprintf(&b, "timestamp: %s\n", time.Now().UTC().Format(time.RFC3339))

	return b.String()
}

// Ensure kind values match NIP-34 spec.
func init() {
	if nostr.KindStatusOpen != 1630 {
		panic("unexpected KindStatusOpen value")
	}
	if nostr.KindStatusApplied != 1631 {
		panic("unexpected KindStatusApplied value")
	}
	if nostr.KindStatusClosed != 1632 {
		panic("unexpected KindStatusClosed value")
	}
}
