// Package publisher — NIP-34 auto-fix patch publication.
//
// PublishFixPatch emits a kind 1617 (Patch) event containing a combined diff
// of auto-fix suggestions that apply cleanly. The event is threaded as a reply
// to the original patch under review, allowing maintainers to apply it with
// standard NIP-34 tooling.
package publisher

import (
	"context"
	"fmt"
	"strings"
	"time"

	"drydock/internal/metrics"

	"fiatjaf.com/nostr"
)

// PublishFixPatchInput contains the data needed to publish an auto-fix patch.
type PublishFixPatchInput struct {
	PatchEventID  string   // the original patch event being reviewed
	RepoID        string
	ReviewEventID string   // the review comment event ID
	PatchDiff     string   // combined unified diff of all applied fixes
	AppliedCount  int      // number of findings with applied fixes
	AppliedFiles  []string // files modified by the fix
	Model         string   // model that generated the suggestions
}

// PublishFixPatchResult describes the outcome of a fix-patch publish attempt.
type PublishFixPatchResult struct {
	Published bool
	EventID   string
	Reason    string
}

// PublishFixPatch creates and publishes a NIP-34 kind 1617 patch event
// containing auto-fix suggestions as a reply in the same patch thread.
func (s *Service) PublishFixPatch(ctx context.Context, in PublishFixPatchInput) (PublishFixPatchResult, error) {
	if strings.TrimSpace(in.PatchDiff) == "" {
		return PublishFixPatchResult{Reason: "empty_diff"}, nil
	}
	if strings.TrimSpace(in.PatchEventID) == "" {
		return PublishFixPatchResult{}, fmt.Errorf("patch event id is required")
	}
	if strings.TrimSpace(in.RepoID) == "" {
		return PublishFixPatchResult{}, fmt.Errorf("repo id is required")
	}

	// Load the original patch event for threading.
	patchRec, err := s.store.GetPatchEvent(ctx, in.PatchEventID)
	if err != nil {
		return PublishFixPatchResult{}, fmt.Errorf("get patch event: %w", err)
	}
	patchEvent, err := parsePatchEvent(patchRec.RawEvent)
	if err != nil {
		return PublishFixPatchResult{}, err
	}
	scope, err := deriveCommentScope(patchEvent)
	if err != nil {
		return PublishFixPatchResult{}, fmt.Errorf("derive scope: %w", err)
	}

	// Resolve relays.
	relays, err := s.resolveRelays(ctx, in.PatchEventID, in.RepoID)
	if err != nil {
		return PublishFixPatchResult{}, fmt.Errorf("resolve relays: %w", err)
	}

	// Build the fix patch event.
	content := buildFixPatchContent(in)
	tags := buildFixPatchTags(scope, in)

	fixEvent := nostr.Event{
		Kind:      nostr.KindPatch,
		CreatedAt: nostr.Now(),
		Tags:      tags,
		Content:   content,
	}
	if err := s.signer.SignEvent(ctx, &fixEvent); err != nil {
		return PublishFixPatchResult{}, fmt.Errorf("sign fix patch event: %w", err)
	}

	// Publish.
	metrics.AutoFixPublishAttempts.Inc()
	if err := s.publish.Publish(ctx, relays, fixEvent); err != nil {
		metrics.AutoFixPublishFailures.Inc()
		return PublishFixPatchResult{}, fmt.Errorf("publish fix patch event: %w", err)
	}
	metrics.AutoFixPublishSuccesses.Inc()

	s.logger.Info("auto-fix patch published",
		"patch_event_id", in.PatchEventID,
		"repo_id", in.RepoID,
		"fix_event_id", fixEvent.ID.Hex(),
		"applied_count", in.AppliedCount,
		"applied_files", len(in.AppliedFiles),
	)

	return PublishFixPatchResult{
		Published: true,
		EventID:   fixEvent.ID.Hex(),
		Reason:    "published",
	}, nil
}

// buildFixPatchContent returns the raw unified diff as the event content.
// Metadata is carried in tags (not in the diff body) so the content remains
// a pure patch payload that standard NIP-34 / git-apply consumers can use.
func buildFixPatchContent(in PublishFixPatchInput) string {
	return strings.TrimRight(in.PatchDiff, "\n") + "\n"
}

// buildFixPatchTags creates the NIP-34 patch event tags for threading.
// All metadata is carried in tags so the event content stays a pure diff.
func buildFixPatchTags(scope commentScope, in PublishFixPatchInput) nostr.Tags {
	tags := nostr.Tags{
		// Thread as a reply to the root patch thread
		{"e", scope.RootID, "", "root"},
		// Reference the specific patch event being fixed
		{"e", in.PatchEventID, "", "reply"},
		// Repository reference
		{"a", "30617:" + in.RepoID},
		// Mark this as an autofix patch (used for loop suppression + filtering)
		{"t", "drydock-autofix"},
		// Expiration
		{"expiration", fmt.Sprintf("%d", time.Now().Add(90*24*time.Hour).Unix())},
	}

	// Tag the root author
	if scope.RootPubKey != "" {
		tags = append(tags, nostr.Tag{"p", scope.RootPubKey})
	}

	// Machine-readable metadata in tags (not content)
	tags = append(tags, nostr.Tag{"review-event-id", in.ReviewEventID})
	tags = append(tags, nostr.Tag{"model", in.Model})
	tags = append(tags, nostr.Tag{"applied-findings", fmt.Sprintf("%d", in.AppliedCount)})

	// Description tag for NIP-34 tooling
	desc := fmt.Sprintf("Drydock auto-fix: %d suggestion(s) applied to %s",
		in.AppliedCount, strings.Join(in.AppliedFiles, ", "))
	if len(desc) > 200 {
		desc = desc[:200] + "…"
	}
	tags = append(tags, nostr.Tag{"description", desc})

	return tags
}
