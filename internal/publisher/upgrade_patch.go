package publisher

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/eventkind"
	"git.sharegap.net/cascadia/drydock/internal/metrics"

	"fiatjaf.com/nostr"
)

// PublishUpgradePatchInput carries the data needed to publish a dependency
// upgrade as a root NIP-34 patch. Unlike PublishFixPatchInput it has no
// originating PatchEventID: an upgrade is proposed by drydock, not in reply to a
// patch under review, so the emitted event is a thread root, not a reply.
type PublishUpgradePatchInput struct {
	UpgradeID   int64  // durable dependency_upgrades row id (outbox key)
	RepoID      string // "<pubkey>:<identifier>" repository id
	Ecosystem   string
	Package     string
	FromVersion string
	ToVersion   string
	Advisories  []string
	PatchDiff   string // authoritative unified diff produced in drydock's worktree
	Model       string // deterministic producer label, e.g. "drydock-depupgrade"
	// Degraded marks a target version that fell back to the scanner's fixed
	// version because the registry was unreachable, so the patch is honest about
	// not being a registry-verified next-patch resolution.
	Degraded       bool
	DegradedReason string
}

// PublishUpgradePatch creates and publishes a root NIP-34 kind 1617 patch event
// carrying a dependency upgrade. It is a sibling of PublishFixPatch, not a
// parameterization of it: the fix-patch publisher is reply-threaded (it requires
// an originating patch, calls GetPatchEvent + deriveCommentScope, and tags an
// {"e", root} / {"e", patch, "", "reply"} thread), none of which an upgrade has.
//
// Publication is idempotent on UpgradeID via the dependency-upgrade outbox: the
// signed event is reserved before relay delivery and the exact reserved event is
// reused on retry, so a repeated publish keeps the same Nostr event id. Recording
// that event id on the upgrade row and its lifecycle state are the caller's
// concern; this method only signs, reserves, and delivers.
//
// A consequence of reserve-then-deliver: the reserved event carries the diff from
// the first attempt. If a delivery-failed attempt is retried after the default
// branch has moved, the retry republishes that original diff (stable event id)
// rather than a freshly recomputed one — the same idempotency trade-off the review
// outboxes make. The upgrade identity (from→to) is unchanged by a HEAD move, so
// this only matters if the manifest itself changed under the same version pair.
func (s *Service) PublishUpgradePatch(ctx context.Context, in PublishUpgradePatchInput) (PublishFixPatchResult, error) {
	if strings.TrimSpace(in.PatchDiff) == "" {
		return PublishFixPatchResult{Reason: "empty_diff"}, nil
	}
	if in.UpgradeID <= 0 {
		return PublishFixPatchResult{}, fmt.Errorf("upgrade id is required")
	}
	if strings.TrimSpace(in.RepoID) == "" {
		return PublishFixPatchResult{}, fmt.Errorf("repo id is required")
	}

	// An upgrade has no originating patch event; resolveRelays degrades to the
	// repository's announcement relays when the patch id is empty.
	relays, err := s.resolveRelays(ctx, "", in.RepoID)
	if err != nil {
		return PublishFixPatchResult{}, fmt.Errorf("resolve relays: %w", err)
	}

	// Reuse the exact reserved event across retries so the Nostr event id stays
	// stable; only build and sign a fresh event on the first attempt.
	event, delivered, found, err := s.store.GetDependencyUpgradePublication(ctx, in.UpgradeID)
	if err != nil {
		return PublishFixPatchResult{}, fmt.Errorf("load upgrade publication reservation: %w", err)
	}
	if !found {
		event = nostr.Event{
			Kind:      nostr.KindPatch,
			CreatedAt: nostr.Now(),
			Tags:      buildUpgradePatchTags(in, s.cfg.DefaultTTL),
			Content:   buildFixPatchContent(PublishFixPatchInput{PatchDiff: in.PatchDiff}),
		}
		if err := s.signer.SignEvent(ctx, &event); err != nil {
			return PublishFixPatchResult{}, fmt.Errorf("sign upgrade patch event: %w", err)
		}
		event, delivered, err = s.store.ReserveDependencyUpgradePublication(ctx, in.UpgradeID, event)
		if err != nil {
			return PublishFixPatchResult{}, fmt.Errorf("reserve upgrade patch event: %w", err)
		}
	}

	metrics.DependencyUpgradePublishAttempts.Inc()
	if !delivered {
		if err := s.publish.Publish(ctx, relays, event); err != nil {
			metrics.DependencyUpgradePublishFailures.Inc()
			return PublishFixPatchResult{}, fmt.Errorf("publish upgrade patch event: %w", err)
		}
		if err := s.store.MarkDependencyUpgradePublicationDelivered(ctx, in.UpgradeID); err != nil {
			metrics.DependencyUpgradePublishFailures.Inc()
			return PublishFixPatchResult{}, fmt.Errorf("persist upgrade patch delivery: %w", err)
		}
	}
	metrics.DependencyUpgradePublishSuccesses.Inc()

	s.logger.Info("dependency-upgrade patch published",
		"upgrade_id", in.UpgradeID,
		"repo_id", in.RepoID,
		"ecosystem", in.Ecosystem,
		"package", in.Package,
		"from_version", in.FromVersion,
		"to_version", in.ToVersion,
		"patch_event_id", event.ID.Hex(),
		"degraded", in.Degraded,
	)

	return PublishFixPatchResult{
		Published: true,
		EventID:   event.ID.Hex(),
		Reason:    "published",
	}, nil
}

// buildUpgradePatchTags builds the tags for a root NIP-34 dependency-upgrade
// patch. It carries no thread ("e") tags: the event is a root. Machine-readable
// upgrade metadata lives in tags so the content stays a pure diff.
func buildUpgradePatchTags(in PublishUpgradePatchInput, ttl time.Duration) nostr.Tags {
	if ttl <= 0 {
		ttl = 90 * 24 * time.Hour
	}
	tags := nostr.Tags{
		{"a", repositoryAddress(in.RepoID)},
		// Loop-suppression marker: ingest skips self-authored patches carrying it.
		{"t", eventkind.DependencyUpgradeTagValue},
		{"expiration", strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)},
		{"ecosystem", in.Ecosystem},
		{"package", in.Package},
		{"from_version", in.FromVersion},
		{"to_version", in.ToVersion},
		{"model", in.Model},
	}
	for _, advisory := range in.Advisories {
		if a := strings.TrimSpace(advisory); a != "" {
			tags = append(tags, nostr.Tag{"advisory", a})
		}
	}
	if in.Degraded {
		tags = append(tags, nostr.Tag{"resolution", "degraded"})
	}

	desc := fmt.Sprintf("Drydock dependency upgrade: %s %s %s→%s",
		in.Ecosystem, in.Package, in.FromVersion, in.ToVersion)
	if len(in.Advisories) > 0 {
		desc += " (" + strings.Join(in.Advisories, ", ") + ")"
	}
	if in.Degraded {
		desc += " [unverified: registry unavailable, using scanner-reported fix]"
	}
	if len(desc) > 200 {
		desc = desc[:200] + "…"
	}
	tags = append(tags, nostr.Tag{"description", desc})
	return tags
}
