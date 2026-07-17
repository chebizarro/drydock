// Package conversation handles interactive review threads. When a developer
// replies to a Drydock review comment (kind 1111 / NIP-22), the Handler
// loads the original review context, builds a conversational LLM prompt,
// and publishes a response — turning Drydock from a one-way announcement
// system into an interactive review partner.
package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"drydock/internal/db"
	"drydock/internal/metrics"
	"drydock/internal/reviewengine"

	"fiatjaf.com/nostr"
)

// MaxTurnsPerReview is the maximum number of conversation turns allowed per review.
const MaxTurnsPerReview = 3

// maxConcurrent is the maximum number of concurrent conversation LLM calls.
const maxConcurrent = 4

// Signer signs Nostr events for publishing responses.
type Signer interface {
	GetPublicKey(ctx context.Context) (nostr.PubKey, error)
	SignEvent(ctx context.Context, evt *nostr.Event) error
}

// RelayPublisher publishes signed events to Nostr relays.
type RelayPublisher interface {
	Publish(ctx context.Context, relays []string, event nostr.Event) error
}

// Config holds conversation handler configuration.
type Config struct {
	// LLM endpoint for generating conversation responses (typically planner/14B).
	Endpoint reviewengine.ModelEndpoint
	// Temperature for conversation LLM calls.
	Temperature float64
	// DefaultRelays to publish responses to when no relay info is available.
	DefaultRelays []string
	// ResponseTTL is how long conversation responses live before expiring.
	ResponseTTL time.Duration
}

// Handler processes reply events and generates conversational responses.
type Handler struct {
	cfg       Config
	store     *db.Store
	client    reviewengine.LLMClient
	signer    Signer
	publish   RelayPublisher
	logger    *slog.Logger
	ourPubKey string        // cached hex pubkey, resolved once at construction
	sem       chan struct{} // bounded concurrency semaphore
}

// New creates a new conversation Handler.
func New(cfg Config, store *db.Store, client reviewengine.LLMClient, signer Signer, relayPub RelayPublisher, logger *slog.Logger) *Handler {
	if cfg.Temperature == 0 {
		cfg.Temperature = 0.3
	}
	if cfg.ResponseTTL == 0 {
		cfg.ResponseTTL = 30 * 24 * time.Hour
	}

	// Resolve our pubkey once at construction time to avoid per-event IPC/network calls.
	var ourPubKey string
	if signer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if pk, err := signer.GetPublicKey(ctx); err == nil {
			ourPubKey = pk.Hex()
		}
	}

	return &Handler{
		cfg:       cfg,
		store:     store,
		client:    client,
		signer:    signer,
		publish:   relayPub,
		logger:    logger,
		ourPubKey: ourPubKey,
		sem:       make(chan struct{}, maxConcurrent),
	}
}

// HandleReply processes a reply event to a Drydock review.
// It is safe to call from any goroutine. Concurrency is bounded by the
// internal semaphore to prevent unbounded LLM call fan-out.
func (h *Handler) HandleReply(ctx context.Context, replyEvent nostr.Event, relayURL string) error {
	metrics.ConversationRepliesReceived.Inc()

	// Acquire semaphore slot (bounded concurrency).
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	// 1. Find which review this reply targets via e-tags.
	targetEventID := replyTargetEventID(replyEvent)
	if targetEventID == "" {
		h.logger.Debug("reply event has no target event tag", "event_id", replyEvent.ID.Hex())
		return nil
	}

	// 2. Look up the review event in our database.
	reviewEventID, patchEventID, repoID, err := h.store.FindReviewForReply(ctx, targetEventID)
	if err != nil {
		return fmt.Errorf("find review for reply: %w", err)
	}
	if reviewEventID == "" {
		// Also check if the reply targets one of our prior conversation responses.
		// This allows multi-turn threads where the user replies to our response.
		reviewEventID, patchEventID, repoID, err = h.findReviewByResponseEvent(ctx, targetEventID)
		if err != nil {
			return fmt.Errorf("find review via conversation: %w", err)
		}
		if reviewEventID == "" {
			h.logger.Debug("reply does not target a known review event",
				"event_id", replyEvent.ID.Hex(), "target", targetEventID)
			return nil
		}
	}

	// 3. Atomic rate-limited insert — count + admit + insert in one transaction.
	now := time.Now().Unix()
	turnNumber, err := h.store.BeginConversationTurn(ctx, db.ConversationTurn{
		ReviewEventID: reviewEventID,
		ReplyEventID:  replyEvent.ID.Hex(),
		RepoID:        repoID,
		PatchEventID:  patchEventID,
		ReplyAuthor:   replyEvent.PubKey.Hex(),
		ReplyContent:  replyEvent.Content,
		CreatedAt:     now,
	}, MaxTurnsPerReview)
	if errors.Is(err, db.ErrConversationRateLimited) {
		metrics.ConversationRateLimited.Inc()
		h.logger.Info("conversation rate limit reached",
			"review_event_id", reviewEventID,
			"reply_event_id", replyEvent.ID.Hex())
		return nil
	}
	if err != nil {
		return fmt.Errorf("begin conversation turn: %w", err)
	}
	if turnNumber == 0 {
		// Race duplicate — another goroutine handled it.
		return nil
	}

	// 4. Load original review content for context.
	reviewContent, err := h.loadReviewContent(ctx, reviewEventID)
	if err != nil {
		h.store.MarkConversationFailed(ctx, replyEvent.ID.Hex())
		return fmt.Errorf("load review content: %w", err)
	}

	// 5. Load patch diff for additional context.
	patchDiff := h.loadPatchDiff(ctx, patchEventID)

	// 6. Load conversation history (prior turns only).
	history, err := h.store.GetConversationHistory(ctx, reviewEventID)
	if err != nil {
		h.store.MarkConversationFailed(ctx, replyEvent.ID.Hex())
		return fmt.Errorf("get conversation history: %w", err)
	}

	// 7. Build turn pairs from history (exclude the current turn we just inserted).
	var turns []turnPair
	for _, t := range history {
		if t.ReplyEventID == replyEvent.ID.Hex() {
			continue // skip current turn
		}
		turns = append(turns, turnPair{
			UserMessage:      t.ReplyContent,
			AssistantMessage: t.ResponseContent,
		})
	}

	// 8. Generate LLM response.
	system := conversationSystemPrompt()
	user := conversationUserPrompt(reviewContent, patchDiff, turns, replyEvent.Content)

	responseText, err := h.client.ChatCompletion(ctx, reviewengine.ChatRequest{
		BaseURL:     h.cfg.Endpoint.BaseURL,
		APIKey:      h.cfg.Endpoint.APIKey,
		Model:       h.cfg.Endpoint.Model,
		Temperature: h.cfg.Temperature,
		System:      system,
		User:        user,
	})
	if err != nil {
		metrics.ConversationErrors.Inc()
		h.store.MarkConversationFailed(ctx, replyEvent.ID.Hex())
		return fmt.Errorf("conversation LLM call: %w", err)
	}
	responseText = strings.TrimSpace(responseText)

	// 9. Build and publish the response event (kind 1111 / NIP-22 comment).
	relays, err := h.resolveRelays(ctx, patchEventID, repoID, relayURL)
	if err != nil {
		h.store.MarkConversationFailed(ctx, replyEvent.ID.Hex())
		return fmt.Errorf("resolve relays: %w", err)
	}

	expiresAt := strconv.FormatInt(time.Now().Add(h.cfg.ResponseTTL).Unix(), 10)

	responseEvent := nostr.Event{
		Kind:      nostr.KindComment,
		CreatedAt: nostr.Now(),
		Content:   responseText,
		Tags:      h.buildResponseTags(replyEvent, reviewEventID, repoID, expiresAt),
	}
	if err := h.signer.SignEvent(ctx, &responseEvent); err != nil {
		metrics.ConversationErrors.Inc()
		h.store.MarkConversationFailed(ctx, replyEvent.ID.Hex())
		return fmt.Errorf("sign conversation response: %w", err)
	}

	// Persist the signed response in pending state before making it observable.
	stageResult, err := h.store.DB().ExecContext(ctx,
		`UPDATE conversations SET response_event_id=?, response_content=?, status='pending' WHERE reply_event_id=?`,
		responseEvent.ID.Hex(), responseText, replyEvent.ID.Hex(),
	)
	if err != nil {
		metrics.ConversationErrors.Inc()
		if markErr := h.store.MarkConversationFailed(ctx, replyEvent.ID.Hex()); markErr != nil {
			return errors.Join(fmt.Errorf("stage conversation response: %w", err), fmt.Errorf("mark conversation retryable: %w", markErr))
		}
		return fmt.Errorf("stage conversation response: %w", err)
	}
	staged, err := stageResult.RowsAffected()
	if err != nil {
		metrics.ConversationErrors.Inc()
		h.store.MarkConversationFailed(ctx, replyEvent.ID.Hex())
		return fmt.Errorf("confirm staged conversation response: %w", err)
	}
	if staged != 1 {
		metrics.ConversationErrors.Inc()
		h.store.MarkConversationFailed(ctx, replyEvent.ID.Hex())
		return fmt.Errorf("stage conversation response: reply %s not found", replyEvent.ID.Hex())
	}

	if err := h.publish.Publish(ctx, relays, responseEvent); err != nil {
		metrics.ConversationErrors.Inc()
		h.store.MarkConversationFailed(ctx, replyEvent.ID.Hex())
		return fmt.Errorf("publish conversation response: %w", err)
	}

	// 10. Mark the durably staged response as published.
	if err := h.store.SetConversationResponse(ctx, replyEvent.ID.Hex(), responseEvent.ID.Hex(), responseText); err != nil {
		metrics.ConversationErrors.Inc()
		return fmt.Errorf("mark conversation response published: %w", err)
	}

	metrics.ConversationResponsesSent.Inc()
	h.logger.Info("conversation response published",
		"review_event_id", reviewEventID,
		"reply_event_id", replyEvent.ID.Hex(),
		"response_event_id", responseEvent.ID.Hex(),
		"turn", turnNumber,
	)

	return nil
}

// IsReplyToUs checks whether an event is a reply addressed to our pubkey.
// Uses the cached pubkey resolved at construction time — no IPC/network calls.
func (h *Handler) IsReplyToUs(_ context.Context, event nostr.Event) bool {
	if h.ourPubKey == "" {
		return false
	}
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "p" && strings.EqualFold(tag[1], h.ourPubKey) {
			return true
		}
	}
	return false
}

// WaitIdle blocks until all in-flight conversation goroutines have finished.
// Useful for testing and graceful shutdown.
func (h *Handler) WaitIdle() {
	var wg sync.WaitGroup
	for i := 0; i < maxConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.sem <- struct{}{}
			<-h.sem
		}()
	}
	wg.Wait()
}

// replyTargetEventID extracts the immediate parent event ID from a reply.
// Per NIP-22, the parent is indicated by lowercase "e" tag.
func replyTargetEventID(event nostr.Event) string {
	// Prefer e-tag with "reply" marker.
	for _, tag := range event.Tags {
		if len(tag) >= 4 && tag[0] == "e" && tag[3] == "reply" {
			return tag[1]
		}
	}
	// Fall back to last e-tag (most likely the parent in a thread).
	var lastE string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			lastE = tag[1]
		}
	}
	return lastE
}

// findReviewByResponseEvent queries conversations for a response_event_id match.
func (h *Handler) findReviewByResponseEvent(ctx context.Context, responseEventID string) (string, string, string, error) {
	var reviewEventID, patchEventID, repoID string
	err := h.store.DB().QueryRowContext(ctx,
		`SELECT review_event_id, patch_event_id, repo_id FROM conversations WHERE response_event_id=? LIMIT 1`,
		responseEventID,
	).Scan(&reviewEventID, &patchEventID, &repoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", "", nil // not found is fine
		}
		return "", "", "", fmt.Errorf("find review by response event: %w", err)
	}
	return reviewEventID, patchEventID, repoID, nil
}

func (h *Handler) loadReviewContent(ctx context.Context, reviewEventID string) (string, error) {
	rawJSON, err := h.store.GetReviewSummary(ctx, reviewEventID)
	if err != nil {
		return "", err
	}
	var evt nostr.Event
	if err := json.Unmarshal([]byte(rawJSON), &evt); err != nil {
		return "", fmt.Errorf("decode review event: %w", err)
	}
	return evt.Content, nil
}

func (h *Handler) loadPatchDiff(ctx context.Context, patchEventID string) string {
	rec, err := h.store.GetPatchEvent(ctx, patchEventID)
	if err != nil {
		h.logger.Debug("could not load patch for conversation context", "error", err)
		return ""
	}
	var evt nostr.Event
	if err := json.Unmarshal([]byte(rec.RawEvent), &evt); err != nil {
		return ""
	}
	return evt.Content
}

func (h *Handler) resolveRelays(ctx context.Context, patchEventID, repoID, replyRelayURL string) ([]string, error) {
	relays, err := h.store.GetPublishRelays(ctx, patchEventID, repoID)
	if err != nil {
		relays = nil
	}
	if replyRelayURL != "" {
		relays = append(relays, replyRelayURL)
	}
	if len(relays) == 0 {
		relays = append(relays, h.cfg.DefaultRelays...)
	}
	// Deduplicate
	seen := make(map[string]struct{}, len(relays))
	deduped := make([]string, 0, len(relays))
	for _, r := range relays {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if _, ok := seen[r]; !ok {
			seen[r] = struct{}{}
			deduped = append(deduped, r)
		}
	}
	if len(deduped) == 0 {
		return nil, fmt.Errorf("no relays available for conversation response")
	}
	return deduped, nil
}

// buildResponseTags creates NIP-22 compliant tags for a conversation response.
// The response is a comment (kind 1111) replying to the user's reply event,
// within the thread rooted at the original review event.
func (h *Handler) buildResponseTags(replyEvent nostr.Event, reviewEventID, repoID, expiration string) nostr.Tags {
	// Use the review event as the root of the conversation sub-thread.
	rootID := reviewEventID
	rootKind := nostr.KindComment

	tags := nostr.Tags{
		// NIP-22: uppercase E/K = root, lowercase e/k = parent
		{"E", rootID, "", ""},
		{"K", strconv.Itoa(int(rootKind))},
		{"e", replyEvent.ID.Hex(), "", replyEvent.PubKey.Hex()},
		{"k", strconv.Itoa(int(replyEvent.Kind))},
		{"A", "30617:" + repoID},
		{"p", replyEvent.PubKey.Hex()},
		{"expiration", expiration},
	}
	return tags
}
