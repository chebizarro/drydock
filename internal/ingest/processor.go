package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"drydock/internal/db"
	"drydock/internal/metrics"

	"fiatjaf.com/nostr"
)

// ConversationHandler processes reply events targeting Drydock reviews.
type ConversationHandler interface {
	HandleReply(ctx context.Context, replyEvent nostr.Event, relayURL string) error
	IsReplyToUs(ctx context.Context, event nostr.Event) bool
}

// CodeChatHandler processes encrypted DM events for codebase Q&A.
type CodeChatHandler interface {
	HandleDM(ctx context.Context, event nostr.Event, relayURL string) error
	IsDMToUs(ctx context.Context, event nostr.Event) bool
}

type Processor struct {
	store              *db.Store
	logger             *slog.Logger
	ReviewQueue        chan db.ReviewTask
	conversation       ConversationHandler
	codeChat           CodeChatHandler
	localAutofixPubKey string // if set, skip review of patches from this pubkey
}

// WithConversation sets the conversation handler for processing reply events.
func WithConversation(ch ConversationHandler) func(*Processor) {
	return func(p *Processor) {
		p.conversation = ch
	}
}

// WithLocalAutofixAuthor configures the processor to skip review of patch
// events authored by the given public key. This prevents Drydock from
// recursively reviewing its own auto-fix patches.
func WithLocalAutofixAuthor(pubkey string) func(*Processor) {
	return func(p *Processor) {
		p.localAutofixPubKey = pubkey
	}
}

// WithCodeChat sets the codechat handler for processing encrypted DM events.
func WithCodeChat(ch CodeChatHandler) func(*Processor) {
	return func(p *Processor) {
		p.codeChat = ch
	}
}

func NewProcessor(store *db.Store, logger *slog.Logger, opts ...func(*Processor)) *Processor {
	p := &Processor{
		store:       store,
		logger:      logger,
		ReviewQueue: make(chan db.ReviewTask, 256),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *Processor) ProcessEvent(ctx context.Context, event nostr.Event, relayURL string) error {
	// Verify event signature before processing. Reject forged or unsigned events.
	if !event.VerifySignature() {
		metrics.EventsRejected.Inc()
		p.logger.Warn("rejected event with invalid signature",
			"event_id", event.ID.Hex(),
			"kind", int(event.Kind),
			"relay", relayURL,
		)
		return nil // drop silently — do not propagate invalid events
	}

	inserted, err := p.store.InsertIngestedEvent(ctx, event)
	if err != nil {
		return err
	}
	if !inserted {
		p.logger.Debug("skipping duplicate event", "event_id", event.ID.Hex(), "kind", int(event.Kind))
		return nil
	}

	metrics.EventsIngested.With(fmt.Sprintf("%d", int(event.Kind))).Inc()

	switch event.Kind {
	case nostr.KindRepositoryAnnouncement:
		return p.store.UpsertRepositoryAnnouncement(ctx, event)
	case nostr.KindRepositoryState:
		return p.store.UpsertRepositorySnapshot(ctx, event)
	case 1630, 1631, 1632, 1633:
		return p.store.UpsertRootStatus(ctx, event)
	case 1617, 1618, 1619:
		if err := p.store.InsertPatchEvent(ctx, event); err != nil {
			return err
		}
		if err := p.store.RecordPatchEventRelay(ctx, event.ID.Hex(), relayURL); err != nil {
			return err
		}
		// Loop suppression: skip autofix patches we published ourselves.
		// Requires BOTH conditions: authored by our signer AND tagged as autofix.
		// This avoids suppressing legitimate patches from the same identity.
		if p.localAutofixPubKey != "" && event.PubKey.Hex() == p.localAutofixPubKey && hasAutofixTag(event) {
			p.logger.Info("skipping self-authored autofix patch",
				"event_id", event.ID.Hex(),
				"pubkey", p.localAutofixPubKey)
			return nil
		}
		repoID := db.RepoIDFromPatch(event)
		if repoID == "" {
			p.logger.Warn("patch event missing resolvable repository pointer", "event_id", event.ID.Hex(), "kind", int(event.Kind))
			return nil
		}
		stale, reason, err := p.store.IsPatchStaleBySnapshot(ctx, event)
		if err != nil {
			return err
		}
		if stale {
			p.logger.Info("skipping stale patch from snapshot", "event_id", event.ID.Hex(), "repo_id", repoID, "reason", reason)
			return nil
		}
		closed, closedReason, err := p.store.IsRootClosedByStatus(ctx, db.RootEventID(event), repoID)
		if err != nil {
			return err
		}
		if closed {
			p.logger.Info("skipping review for closed/applied root", "event_id", event.ID.Hex(), "repo_id", repoID, "reason", closedReason)
			return nil
		}

		acquired, err := p.store.BeginReview(ctx, event.ID.Hex(), repoID)
		if err != nil {
			if errors.Is(err, db.ErrReviewAlreadyPublished) {
				p.logger.Debug("skipping already-published review target", "event_id", event.ID.Hex(), "repo_id", repoID)
				return nil
			}
			return err
		}
		if acquired {
			task := db.ReviewTask{PatchEventID: event.ID.Hex(), RepoID: repoID}
			select {
			case p.ReviewQueue <- task:
				metrics.ReviewQueuePushed.Inc()
				metrics.ReviewQueueDepth.Inc()
				p.logger.Info("queued patch review", "event_id", event.ID.Hex(), "repo_id", repoID, "kind", int(event.Kind))
			default:
				// Queue is full — mark task back to failed so it can be retried
				// by the next startup's ResetStuckReviews or a future re-enqueue sweep.
				metrics.ReviewQueueFull.Inc()
				p.logger.Warn("review queue full, marking task for retry", "event_id", event.ID.Hex(), "repo_id", repoID)
				_ = p.store.MarkReviewFailed(ctx, event.ID.Hex(), repoID, "review queue full")
			}
		}
		return nil
	case nostr.KindComment:
		// Reply to one of our reviews? Route to conversation handler.
		if p.conversation != nil && p.conversation.IsReplyToUs(ctx, event) {
			go func() {
				if err := p.conversation.HandleReply(ctx, event, relayURL); err != nil {
					p.logger.Error("conversation handler failed",
						"event_id", event.ID.Hex(),
						"error", err,
					)
				}
			}()
		}
		return nil
	case nostr.KindEncryptedDirectMessage, 14: // NIP-04 kind 4 or NIP-17 kind 14
		// Encrypted DM to us? Route to codechat handler.
		if p.codeChat != nil && p.codeChat.IsDMToUs(ctx, event) {
			go func() {
				if err := p.codeChat.HandleDM(ctx, event, relayURL); err != nil {
					p.logger.Error("codechat handler failed",
						"event_id", event.ID.Hex(),
						"error", err,
					)
				}
			}()
		}
		return nil
	default:
		return nil
	}
}

// hasAutofixTag checks if an event carries the drydock-autofix tag.
func hasAutofixTag(event nostr.Event) bool {
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "t" && tag[1] == "drydock-autofix" {
			return true
		}
	}
	return false
}
