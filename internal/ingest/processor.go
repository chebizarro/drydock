package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"drydock/internal/contextvm"
	"drydock/internal/db"
	"drydock/internal/eventkind"
	"drydock/internal/metrics"
	"drydock/internal/monitoring"
	"drydock/internal/revieworder"
	"drydock/internal/scope"

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

// MonitoringHandler applies the operator-authored repository control plane.
type MonitoringHandler interface {
	ApplyList(ctx context.Context, event nostr.Event) (bool, error)
	ApplyDeletion(ctx context.Context, event nostr.Event) (bool, error)
}

// ReviewOrderer owns reactive admission and the durable review queue.
type ReviewOrderer interface {
	SubmitReactive(context.Context, nostr.Event, scope.RepositoryRef) (revieworder.SubmissionResult, error)
	EnqueueClaimed(context.Context, db.ReviewTask, string) error
	Queue() <-chan db.ReviewTask
}

// ContextVMResponder publishes ContextVM JSON-RPC responses.
type ContextVMResponder interface {
	SendResponseToEvent(ctx context.Context, requestEventID, id string, result any, rpcErr *contextvm.Error, recipients ...nostr.PubKey) error
}

type Processor struct {
	store  *db.Store
	logger *slog.Logger
	// ReviewQueue is a deprecated receive-only view retained for callers while
	// revieworder.Service owns the underlying channel.
	ReviewQueue        <-chan db.ReviewTask
	reviewOrders       ReviewOrderer
	conversation       ConversationHandler
	codeChat           CodeChatHandler
	ideGateway         IDEGatewayHandler
	marketplace        MarketplaceHandler
	monitoring         MonitoringHandler
	contextVMRouter    *contextvm.Router
	contextVMResponder ContextVMResponder
	localAutofixPubKey string // if set, skip review of patches from this pubkey
	servicePubkey      string
	trustedZappers     map[string]struct{}
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

// WithReviewOrders configures shared reactive admission and queue ownership.
func WithReviewOrders(service ReviewOrderer) func(*Processor) {
	return func(p *Processor) {
		p.reviewOrders = service
	}
}

// WithZapReceipts configures NIP-57 receipt validation for the service identity.
func WithZapReceipts(servicePubkey string, trustedZappers []string) func(*Processor) {
	return func(p *Processor) {
		p.servicePubkey = scope.NormalizePubkey(servicePubkey)
		p.trustedZappers = make(map[string]struct{}, len(trustedZappers))
		for _, zapper := range trustedZappers {
			p.trustedZappers[scope.NormalizePubkey(zapper)] = struct{}{}
		}
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

// WithMonitoring sets the monitored-repository list handler.
func WithMonitoring(handler MonitoringHandler) func(*Processor) {
	return func(p *Processor) {
		p.monitoring = handler
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
		maxEventFutureSkew: maxEventFutureSkew,
		maxEventPastAge:    maxEventPastAge,
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.reviewOrders != nil {
		p.ReviewQueue = p.reviewOrders.Queue()
	}
	return p
}

const (
	maxEventFutureSkew = 10 * time.Minute
	maxEventPastAge    = 365 * 24 * time.Hour
)

func (p *Processor) ProcessEvent(ctx context.Context, event nostr.Event, relayURL string) error {
	return p.processEvent(ctx, event, relayURL, false)
}

// ProcessGiftWrappedEvent ingests a rumor authenticated by a verified NIP-59
// envelope. Rumors have IDs but intentionally do not carry direct signatures.
func (p *Processor) ProcessGiftWrappedEvent(ctx context.Context, event nostr.Event, relayURL string) error {
	return p.processEvent(ctx, event, relayURL, true)
}

func (p *Processor) processEvent(ctx context.Context, event nostr.Event, relayURL string, authenticatedEnvelope bool) error {
	if !p.validateEventForIngest(event, relayURL, authenticatedEnvelope) {
		return nil // drop silently — do not propagate invalid events
	}

	inserted, err := p.store.InsertIngestedEvent(ctx, event)
	if err != nil {
		return err
	}
	if !inserted {
		complete, err := p.store.IsIngestHandlerComplete(ctx, event.ID.Hex())
		if err != nil {
			return err
		}
		if complete {
			p.logger.Debug("skipping duplicate event with completed handler", "event_id", event.ID.Hex(), "kind", int(event.Kind))
			return nil
		}
		p.logger.Debug("retrying duplicate event with incomplete handler", "event_id", event.ID.Hex(), "kind", int(event.Kind))
	} else {
		metrics.EventsIngested.With(fmt.Sprintf("%d", int(event.Kind))).Inc()
	}

	if err := p.handleEvent(ctx, event, relayURL); err != nil {
		return err
	}
	// A synchronous handler may drain successfully after shutdown cancellation;
	// persist that success so restart redelivery does not repeat its side effects.
	return p.store.MarkIngestHandlerComplete(context.WithoutCancel(ctx), event.ID.Hex())
}

func (p *Processor) handleEvent(ctx context.Context, event nostr.Event, relayURL string) error {
	switch event.Kind {
	case eventkind.MonitoredRepositories:
		return p.handleMonitoringEvent(ctx, event, false)
	case eventkind.Deletion:
		return p.handleMonitoringEvent(ctx, event, true)
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
		repository, err := repositoryRefFromPatch(event)
		if err != nil {
			p.logger.Warn("patch event missing unique canonical repository pointer", "event_id", event.ID.Hex(), "kind", int(event.Kind), "error", err)
			return nil
		}
		repoID := repository.RepositoryID
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

		if p.reviewOrders == nil {
			p.logger.Info("reactive review disabled: review order service is not configured", "event_id", event.ID.Hex(), "repo_id", repoID)
			return nil
		}
		_, err = p.reviewOrders.SubmitReactive(ctx, event, repository)
		return err
	case eventkind.ZapReceipt:
		if p.servicePubkey == "" {
			return nil
		}
		receipt, err := p.validateZapReceipt(event)
		if err != nil {
			p.logger.Warn("rejected zap receipt", "event_id", event.ID.Hex(), "author", event.PubKey.Hex(), "relay", relayURL, "reason", err.Error())
			return nil
		}
		inserted, tasks, err := p.store.InsertZapReceiptAndClaimBlockedReviews(ctx, receipt)
		if err != nil {
			return err
		}
		if inserted {
			p.logger.Info("accepted zap receipt", "event_id", receipt.EventID, "patch_event_id", receipt.PatchEventID, "amount_msat", receipt.AmountMSat, "trusted_zapper_allowlist", len(p.trustedZappers) > 0)
		}
		for _, task := range tasks {
			if p.reviewOrders == nil {
				return errors.New("review order service is not configured")
			}
			if err := p.reviewOrders.EnqueueClaimed(ctx, task, "zap_receipt"); err != nil {
				return err
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
	case eventkind.ReviewerProfile:
		// Route reviewer profiles to the marketplace handler.
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

func (p *Processor) validateEventForIngest(event nostr.Event, relayURL string, authenticatedEnvelope bool) bool {
	reason := ""
	switch {
	case !event.CheckID():
		reason = "id_mismatch"
	case !authenticatedEnvelope && !event.VerifySignature():
		reason = "invalid_signature"
	case !eventTimestampPlausibleForKind(event.Kind, event.CreatedAt, p.maxEventFutureSkew, p.maxEventPastAge):
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
		"authenticated_envelope", authenticatedEnvelope,
		"created_at", int64(event.CreatedAt),
	)
	return false
}

func (p *Processor) handleMonitoringEvent(ctx context.Context, event nostr.Event, deletion bool) error {
	if p.monitoring == nil {
		return nil
	}
	var (
		applied bool
		err     error
	)
	if deletion {
		applied, err = p.monitoring.ApplyDeletion(ctx, event)
	} else {
		applied, err = p.monitoring.ApplyList(ctx, event)
	}
	if err != nil {
		if errors.Is(err, monitoring.ErrUnauthorizedAuthor) ||
			errors.Is(err, monitoring.ErrMalformedList) ||
			errors.Is(err, monitoring.ErrMalformedDeletion) {
			p.logger.Warn("rejected monitored repository control event",
				"event_id", event.ID.Hex(),
				"kind", int(event.Kind),
				"author", event.PubKey.Hex(),
				"reason", err.Error(),
			)
			return nil
		}
		return err
	}
	if applied {
		p.logger.Info("applied monitored repository control event",
			"event_id", event.ID.Hex(),
			"kind", int(event.Kind),
			"deleted", deletion,
		)
	}
	return nil
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
	if p.servicePubkey != "" {
		recipient, err := singleTagValue(event, "p")
		if err != nil || scope.NormalizePubkey(recipient) != p.servicePubkey {
			p.logger.Debug("ignoring ContextVM request for another recipient", "event_id", event.ID.Hex())
			return nil
		}
		method, err := singleTagValue(event, "method")
		if err != nil || method != msg.Method {
			p.logger.Debug("ignoring ContextVM request with invalid method tag", "event_id", event.ID.Hex(), "method", msg.Method)
			return nil
		}
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

func repositoryRefFromPatch(event nostr.Event) (scope.RepositoryRef, error) {
	var repository scope.RepositoryRef
	count := 0
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "a" || !strings.HasPrefix(strings.TrimSpace(tag[1]), "30617:") {
			continue
		}
		ref, err := scope.ParseRepositoryRef(tag[1])
		if err != nil {
			return scope.RepositoryRef{}, err
		}
		count++
		repository = ref
	}
	if count != 1 {
		return scope.RepositoryRef{}, errors.New("patch must contain exactly one canonical 30617 repository address")
	}
	return repository, nil
}

func eventTimestampPlausibleForKind(kind nostr.Kind, ts nostr.Timestamp, maxFutureSkew, maxPastAge time.Duration) bool {
	now := time.Now()
	createdAt := time.Unix(int64(ts), 0)
	if createdAt.After(now.Add(maxFutureSkew)) {
		return false
	}
	// Dedicated no-Since subscriptions must be able to recover an old current
	// replaceable list or tombstone. Replacement ordering, not a global age
	// cutoff, decides whether these control-plane events still matter.
	if kind == eventkind.MonitoredRepositories || kind == eventkind.Deletion {
		return true
	}
	return !createdAt.Before(now.Add(-maxPastAge))
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
