package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"drydock/internal/contextvm"
	"drydock/internal/db"
	"drydock/internal/eventkind"
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

// IDEGatewayHandler processes IDE integration events.
type IDEGatewayHandler interface {
	HandleEvent(ctx context.Context, event nostr.Event, relayURL string) error
}

// MarketplaceHandler processes review marketplace events.
type MarketplaceHandler interface {
	HandleEvent(ctx context.Context, event nostr.Event, relayURL string) error
}

// ContextVMResponder publishes ContextVM JSON-RPC responses.
type ContextVMResponder interface {
	SendResponseToEvent(ctx context.Context, requestEventID, id string, result any, rpcErr *contextvm.Error, recipients ...nostr.PubKey) error
}

type Processor struct {
	store              *db.Store
	logger             *slog.Logger
	ReviewQueue        chan db.ReviewTask
	conversation       ConversationHandler
	codeChat           CodeChatHandler
	ideGateway         IDEGatewayHandler
	marketplace        MarketplaceHandler
	contextVMRouter    *contextvm.Router
	contextVMResponder ContextVMResponder
	localAutofixPubKey string // if set, skip review of patches from this pubkey
	maxEventFutureSkew time.Duration
	maxEventPastAge    time.Duration
}

// WithTimingPolicy configures accepted event timestamp skew and age.
func WithTimingPolicy(maxFutureSkew, maxPastAge time.Duration) func(*Processor) {
	return func(p *Processor) {
		if maxFutureSkew > 0 {
			p.maxEventFutureSkew = maxFutureSkew
		}
		if maxPastAge > 0 {
			p.maxEventPastAge = maxPastAge
		}
	}
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

// WithIDEGateway sets the IDE gateway handler for processing IDE events.
func WithIDEGateway(h IDEGatewayHandler) func(*Processor) {
	return func(p *Processor) {
		p.ideGateway = h
	}
}

// WithMarketplace sets the marketplace handler for processing reviewer events.
func WithMarketplace(h MarketplaceHandler) func(*Processor) {
	return func(p *Processor) {
		p.marketplace = h
	}
}

// WithContextVM sets the ContextVM router and responder for kind 25910 events.
func WithContextVM(router *contextvm.Router, responder ContextVMResponder) func(*Processor) {
	return func(p *Processor) {
		p.contextVMRouter = router
		p.contextVMResponder = responder
	}
}

func NewProcessor(store *db.Store, logger *slog.Logger, opts ...func(*Processor)) *Processor {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Processor{
		store:              store,
		logger:             logger,
		ReviewQueue:        make(chan db.ReviewTask, 256),
		maxEventFutureSkew: maxEventFutureSkew,
		maxEventPastAge:    maxEventPastAge,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

const (
	maxEventFutureSkew = 10 * time.Minute
	maxEventPastAge    = 365 * 24 * time.Hour
)

func (p *Processor) ProcessEvent(ctx context.Context, event nostr.Event, relayURL string) error {
	if !p.validateEventForIngest(event, relayURL) {
		return nil // drop silently — do not propagate invalid events
	}

	inserted, err := p.store.InsertIngestedEvent(ctx, event)
	if err != nil {
		return err
	}
	if !inserted {
		p.logger.Debug("reprocessing duplicate event through idempotent handler", "event_id", event.ID.Hex(), "kind", int(event.Kind))
		if !isTrackedHandlerKind(event.Kind) {
			return nil
		}
	} else {
		metrics.EventsIngested.With(fmt.Sprintf("%d", int(event.Kind))).Inc()
	}

	switch event.Kind {
	case eventkind.RepositoryAnnouncement:
		return p.store.UpsertRepositoryAnnouncement(ctx, event)
	case eventkind.RepositoryState:
		return p.store.UpsertRepositorySnapshot(ctx, event)
	case eventkind.StatusOpen, eventkind.StatusApplied, eventkind.StatusClosed, eventkind.StatusDraft:
		return p.store.UpsertRootStatus(ctx, event)
	case eventkind.Patch, eventkind.GitPullRequest, eventkind.GitPullRequestUpdate:
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
				queueErr := errors.New("review queue full")
				p.logger.Warn("review queue full, marking task for retry", "event_id", event.ID.Hex(), "repo_id", repoID)
				if err := p.store.MarkReviewFailed(ctx, event.ID.Hex(), repoID, queueErr.Error()); err != nil {
					return errors.Join(queueErr, fmt.Errorf("mark review failed: %w", err))
				}
				return queueErr
			}
		}
		return nil
	case eventkind.Comment:
		// Reply to one of our reviews? Route to conversation handler.
		if p.conversation != nil && p.conversation.IsReplyToUs(ctx, event) {
			if err := p.conversation.HandleReply(ctx, event, relayURL); err != nil {
				metrics.ConversationErrors.Inc()
				p.logger.Error("conversation handler failed",
					"event_id", event.ID.Hex(),
					"error", err,
				)
				return err
			}
		}
		return nil
	case eventkind.EncryptedDirectMessage, eventkind.SealedDirectMessage:
		// Encrypted DM to us? Route to codechat handler.
		if p.codeChat != nil && p.codeChat.IsDMToUs(ctx, event) {
			if err := p.codeChat.HandleDM(ctx, event, relayURL); err != nil {
				metrics.CodeChatErrors.Inc()
				p.logger.Error("codechat handler failed",
					"event_id", event.ID.Hex(),
					"error", err,
				)
				return err
			}
		}
		return nil
	case eventkind.ContextVM:
		return p.handleContextVM(ctx, event, relayURL)
	case eventkind.IDESession:
		// Route to IDE gateway handler.
		if p.ideGateway != nil {
			if err := p.ideGateway.HandleEvent(ctx, event, relayURL); err != nil {
				metrics.IDEReviewErrors.Inc()
				p.logger.Error("IDE gateway handler failed",
					"event_id", event.ID.Hex(),
					"kind", int(event.Kind),
					"error", err,
				)
				return err
			}
		}
		return nil
	case eventkind.ReviewerProfile, eventkind.ReviewFeedback:
		// Route to marketplace handler.
		if p.marketplace != nil {
			if err := p.marketplace.HandleEvent(ctx, event, relayURL); err != nil {
				metrics.MarketplaceRoutingFailures.Inc()
				p.logger.Error("marketplace handler failed",
					"event_id", event.ID.Hex(),
					"kind", int(event.Kind),
					"error", err,
				)
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

func (p *Processor) validateEventForIngest(event nostr.Event, relayURL string) bool {
	reason := ""
	switch {
	case !event.CheckID():
		reason = "id_mismatch"
	case !event.VerifySignature():
		reason = "invalid_signature"
	case !eventTimestampPlausible(event.CreatedAt, p.maxEventFutureSkew, p.maxEventPastAge):
		reason = "implausible_timestamp"
	}
	if reason == "" {
		return true
	}

	metrics.EventsRejected.Inc()
	p.logger.Warn("rejected invalid ingest event",
		"event_id", event.ID.Hex(),
		"kind", int(event.Kind),
		"relay", relayURL,
		"reason", reason,
		"created_at", int64(event.CreatedAt),
	)
	return false
}

func (p *Processor) handleContextVM(ctx context.Context, event nostr.Event, relayURL string) error {
	if p.contextVMRouter == nil {
		return nil
	}

	var msg contextvm.Message
	if err := json.Unmarshal([]byte(event.Content), &msg); err != nil {
		p.logger.Warn("invalid ContextVM message", "event_id", event.ID.Hex(), "error", err)
		return nil
	}
	// Responses do not carry a method and should not be dispatched to method handlers.
	if msg.Method == "" {
		return nil
	}

	resp, err := p.contextVMRouter.Handle(ctx, contextvm.Request{
		Event:  event,
		Relay:  relayURL,
		Sender: event.PubKey,
		Msg:    msg,
	})
	if err != nil {
		return err
	}
	if p.contextVMResponder == nil || resp.ID == "" {
		return nil
	}
	return p.contextVMResponder.SendResponseToEvent(ctx, event.ID.Hex(), resp.ID, resp.Result, resp.Error, event.PubKey)
}

func isTrackedHandlerKind(kind nostr.Kind) bool {
	switch kind {
	case eventkind.Comment, eventkind.EncryptedDirectMessage, eventkind.SealedDirectMessage, eventkind.IDESession, eventkind.ReviewerProfile, eventkind.ReviewFeedback:
		return true
	default:
		return false
	}
}

func eventTimestampPlausible(ts nostr.Timestamp, maxFutureSkew, maxPastAge time.Duration) bool {
	now := time.Now()
	createdAt := time.Unix(int64(ts), 0)
	if createdAt.After(now.Add(maxFutureSkew)) {
		return false
	}
	if createdAt.Before(now.Add(-maxPastAge)) {
		return false
	}
	return true
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
