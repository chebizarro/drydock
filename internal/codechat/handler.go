// Package codechat enables developers to ask questions about indexed codebases
// via Nostr encrypted DMs. It uses RAG (retrieval-augmented generation) to
// query the code index and provide contextual answers, transforming Drydock
// from a review tool into a codebase knowledge assistant.
package codechat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"drydock/internal/db"
	"drydock/internal/embedding"
	"drydock/internal/metrics"
	"drydock/internal/reviewengine"
	"drydock/internal/vectorstore"

	"fiatjaf.com/nostr"
)

const (
	// MaxTurnsPerConversation limits conversation length per DM thread.
	MaxTurnsPerConversation = 10

	// maxConcurrent limits parallel LLM calls for DM handling.
	maxConcurrent = 4

	// maxQueryResults is the max code chunks to retrieve for context.
	maxQueryResults = 8

	// maxContextBytes caps the total RAG context size.
	maxContextBytes = 16 * 1024
)

// Keyer provides signing and encryption for Nostr events.
// Matches the nostr.Keyer interface (Signer + Cipher).
type Keyer interface {
	GetPublicKey(ctx context.Context) (nostr.PubKey, error)
	SignEvent(ctx context.Context, evt *nostr.Event) error
	Encrypt(ctx context.Context, plaintext string, recipient nostr.PubKey) (string, error)
	Decrypt(ctx context.Context, base64ciphertext string, sender nostr.PubKey) (string, error)
}

// RelayPublisher publishes signed events to Nostr relays.
type RelayPublisher interface {
	Publish(ctx context.Context, relays []string, event nostr.Event) error
}

// Config holds codechat handler configuration.
type Config struct {
	// LLM endpoint for generating chat responses.
	Endpoint reviewengine.ModelEndpoint
	// Temperature for chat LLM calls.
	Temperature float64
	// DefaultRelays to publish responses when no relay info is available.
	DefaultRelays []string
}

// Handler processes encrypted DM events and generates codebase chat responses.
type Handler struct {
	cfg       Config
	store     *db.Store
	qdrant    *vectorstore.Client
	embedder  *embedding.Client
	client    reviewengine.LLMClient
	keyer     Keyer
	publish   RelayPublisher
	logger    *slog.Logger
	ourPubKey string        // cached hex pubkey
	sem       chan struct{} // bounded concurrency semaphore
}

// New creates a new codechat Handler.
func New(
	cfg Config,
	store *db.Store,
	qdrant *vectorstore.Client,
	embedder *embedding.Client,
	client reviewengine.LLMClient,
	keyer Keyer,
	relayPub RelayPublisher,
	logger *slog.Logger,
) *Handler {
	if cfg.Temperature == 0 {
		cfg.Temperature = 0.4
	}

	var ourPubKey string
	if keyer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if pk, err := keyer.GetPublicKey(ctx); err == nil {
			ourPubKey = pk.Hex()
		}
	}

	return &Handler{
		cfg:       cfg,
		store:     store,
		qdrant:    qdrant,
		embedder:  embedder,
		client:    client,
		keyer:     keyer,
		publish:   relayPub,
		logger:    logger,
		ourPubKey: ourPubKey,
		sem:       make(chan struct{}, maxConcurrent),
	}
}

// HandleDM processes an encrypted DM event (kind 4 NIP-04 or kind 14 sealed).
// It decrypts the message, queries the code index, generates a response, and
// publishes an encrypted reply.
func (h *Handler) HandleDM(ctx context.Context, event nostr.Event, relayURL string) error {
	metrics.CodeChatDMsReceived.Inc()

	// Acquire semaphore slot (bounded concurrency).
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	// 1. Decrypt the DM content.
	plaintext, err := h.decryptDM(ctx, event)
	if err != nil {
		h.logger.Warn("failed to decrypt DM", "event_id", event.ID.Hex(), "error", err)
		return nil // don't propagate decryption errors
	}

	h.logger.Debug("decrypted DM", "event_id", event.ID.Hex(), "length", len(plaintext))

	// 2. Parse the message to extract repo context and question.
	parsed := h.parseMessage(plaintext)
	if parsed.question == "" {
		h.logger.Debug("empty question in DM, ignoring", "event_id", event.ID.Hex())
		return nil
	}

	// 3. Rate limiting: check conversation turn count.
	senderPubKey := event.PubKey.Hex()
	turnNumber, err := h.store.BeginCodeChatTurn(ctx, db.CodeChatTurn{
		SenderPubKey: senderPubKey,
		EventID:      event.ID.Hex(),
		RepoID:       parsed.repoID,
		Question:     parsed.question,
		CreatedAt:    time.Now().Unix(),
	}, MaxTurnsPerConversation)
	if errors.Is(err, db.ErrCodeChatRateLimited) {
		metrics.CodeChatRateLimited.Inc()
		h.logger.Info("codechat rate limit reached", "sender", senderPubKey)
		return nil
	}
	if err != nil {
		return fmt.Errorf("begin codechat turn: %w", err)
	}
	if turnNumber == 0 {
		return nil // duplicate, already processed
	}

	// 4. Resolve which repo to query. Use explicit repo or infer from context.
	repoID := parsed.repoID
	if repoID == "" {
		// Try to get the most recent repo this user interacted with.
		repoID, _ = h.store.GetLastCodeChatRepo(ctx, senderPubKey)
	}
	if repoID == "" {
		// No repo context available — send helpful error.
		response := "I need to know which codebase you're asking about. " +
			"Please include the repository in your message, like:\n\n" +
			"`repo:npub1.../reponame` what does the main function do?"
		return h.sendEncryptedResponse(ctx, event, response, relayURL)
	}

	// 5. Query code index for relevant context.
	codeContext, err := h.queryCodeIndex(ctx, repoID, parsed.question)
	if err != nil {
		h.logger.Warn("code index query failed", "repo_id", repoID, "error", err)
		codeContext = "" // continue without context
	}

	// 6. Load conversation history for multi-turn context.
	history, err := h.store.GetCodeChatHistory(ctx, senderPubKey, repoID, 5)
	if err != nil {
		h.logger.Warn("failed to load chat history", "error", err)
		history = nil
	}

	// 7. Generate LLM response with RAG context.
	system := codeChatSystemPrompt(repoID)
	user := codeChatUserPrompt(codeContext, history, parsed.question)

	responseText, err := h.client.ChatCompletion(ctx, reviewengine.ChatRequest{
		BaseURL:     h.cfg.Endpoint.BaseURL,
		APIKey:      h.cfg.Endpoint.APIKey,
		Model:       h.cfg.Endpoint.Model,
		Temperature: h.cfg.Temperature,
		System:      system,
		User:        user,
	})
	if err != nil {
		metrics.CodeChatErrors.Inc()
		h.store.MarkCodeChatFailed(ctx, event.ID.Hex())
		return fmt.Errorf("codechat LLM call: %w", err)
	}
	responseText = strings.TrimSpace(responseText)

	// 8. Store the response.
	if err := h.store.SetCodeChatResponse(ctx, event.ID.Hex(), responseText); err != nil {
		h.logger.Warn("failed to record codechat response", "error", err)
	}

	// 9. Send encrypted response.
	if err := h.sendEncryptedResponse(ctx, event, responseText, relayURL); err != nil {
		metrics.CodeChatErrors.Inc()
		return err
	}

	metrics.CodeChatResponsesSent.Inc()
	h.logger.Info("codechat response sent",
		"event_id", event.ID.Hex(),
		"repo_id", repoID,
		"turn", turnNumber,
	)

	return nil
}

// IsDMToUs checks whether an encrypted DM event is addressed to our pubkey.
func (h *Handler) IsDMToUs(_ context.Context, event nostr.Event) bool {
	if h.ourPubKey == "" {
		return false
	}

	// Kind 4 (NIP-04): check p-tag for recipient
	if event.Kind == nostr.KindEncryptedDirectMessage {
		for _, tag := range event.Tags {
			if len(tag) >= 2 && tag[0] == "p" && strings.EqualFold(tag[1], h.ourPubKey) {
				return true
			}
		}
	}

	// Kind 14 (NIP-17 sealed): the event is already addressed to us if we can decrypt it
	// For now, we'll try to decrypt in HandleDM and handle failures gracefully.
	if event.Kind == 14 {
		for _, tag := range event.Tags {
			if len(tag) >= 2 && tag[0] == "p" && strings.EqualFold(tag[1], h.ourPubKey) {
				return true
			}
		}
	}

	return false
}

// WaitIdle blocks until all in-flight DM handlers have finished.
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

// decryptDM decrypts the content of an encrypted DM event using the Keyer's Cipher.
func (h *Handler) decryptDM(ctx context.Context, event nostr.Event) (string, error) {
	// Use the keyer's Decrypt method (implements NIP-44 with conversation key caching).
	plaintext, err := h.keyer.Decrypt(ctx, event.Content, event.PubKey)
	if err != nil {
		return "", fmt.Errorf("decrypt DM: %w", err)
	}
	return plaintext, nil
}

// parsedMessage contains extracted info from a DM.
type parsedMessage struct {
	repoID   string // optional explicit repo reference
	question string // the actual question
}

// parseMessage extracts repo context and question from a DM.
// Supports formats like:
//   - "repo:npub1.../myrepo what does foo do?"
//   - "@npub1.../myrepo how does the auth work?"
//   - Plain question (uses last repo context)
func (h *Handler) parseMessage(content string) parsedMessage {
	content = strings.TrimSpace(content)
	if content == "" {
		return parsedMessage{}
	}

	result := parsedMessage{question: content}

	// Look for repo: prefix
	if strings.HasPrefix(content, "repo:") {
		parts := strings.SplitN(content[5:], " ", 2)
		if len(parts) >= 1 {
			result.repoID = strings.TrimSpace(parts[0])
			if len(parts) >= 2 {
				result.question = strings.TrimSpace(parts[1])
			} else {
				result.question = ""
			}
		}
		return result
	}

	// Look for @npub/repo pattern at start
	if strings.HasPrefix(content, "@") {
		parts := strings.SplitN(content[1:], " ", 2)
		if len(parts) >= 1 && strings.Contains(parts[0], "/") {
			result.repoID = strings.TrimSpace(parts[0])
			if len(parts) >= 2 {
				result.question = strings.TrimSpace(parts[1])
			} else {
				result.question = ""
			}
		}
		return result
	}

	return result
}

// queryCodeIndex retrieves relevant code chunks for the question.
func (h *Handler) queryCodeIndex(ctx context.Context, repoID, question string) (string, error) {
	if h.qdrant == nil || h.embedder == nil {
		return "", nil
	}

	// Truncate question for embedding.
	query := question
	if len(query) > 4096 {
		query = query[:4096]
	}

	// Embed the question.
	vec, err := h.embedder.Embed(ctx, query)
	if err != nil {
		return "", fmt.Errorf("embed question: %w", err)
	}

	// Search code chunks.
	filter := map[string]any{
		"must": []map[string]any{
			{"key": "repo_id", "match": map[string]any{"value": repoID}},
		},
	}

	results, err := h.qdrant.Search(ctx, vectorstore.CollectionCodeChunks, vec, maxQueryResults*2, filter)
	if err != nil {
		return "", fmt.Errorf("search code chunks: %w", err)
	}

	if len(results) == 0 {
		return "", nil
	}

	// Build context from top results.
	var sb strings.Builder
	sb.WriteString("Relevant code from the repository:\n\n")

	totalBytes := 0
	count := 0
	for _, r := range results {
		if r.Score < 0.5 { // score threshold
			continue
		}

		filePath, _ := r.Payload["file_path"].(string)
		symbolName, _ := r.Payload["symbol_name"].(string)
		symbolKind, _ := r.Payload["symbol_kind"].(string)
		content, _ := r.Payload["content"].(string)
		startLine := payloadInt(r.Payload, "start_line")

		header := fmt.Sprintf("### %s (%s) — %s:%d\n```\n",
			symbolName, symbolKind, filePath, startLine)
		footer := "\n```\n\n"

		entryLen := len(header) + len(content) + len(footer)
		if totalBytes+entryLen > maxContextBytes {
			break
		}

		sb.WriteString(header)
		sb.WriteString(content)
		sb.WriteString(footer)

		totalBytes += entryLen
		count++

		if count >= maxQueryResults {
			break
		}
	}

	if count == 0 {
		return "", nil
	}

	return sb.String(), nil
}

// payloadInt extracts an integer from a Qdrant payload field.
func payloadInt(payload map[string]any, key string) int {
	v, ok := payload[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

// sendEncryptedResponse encrypts and publishes a DM response using the Keyer's Cipher.
func (h *Handler) sendEncryptedResponse(ctx context.Context, incomingEvent nostr.Event, response, relayURL string) error {
	// Encrypt using the keyer's Encrypt method (NIP-44).
	ciphertext, err := h.keyer.Encrypt(ctx, response, incomingEvent.PubKey)
	if err != nil {
		return fmt.Errorf("encrypt response: %w", err)
	}

	// Build response event (kind 14 for NIP-17 sealed DM).
	responseEvent := nostr.Event{
		Kind:      14, // NIP-17 sealed direct message
		CreatedAt: nostr.Now(),
		Content:   ciphertext,
		Tags: nostr.Tags{
			{"p", incomingEvent.PubKey.Hex()},
			{"e", incomingEvent.ID.Hex()}, // reference the original DM
		},
	}

	if err := h.keyer.SignEvent(ctx, &responseEvent); err != nil {
		return fmt.Errorf("sign response: %w", err)
	}

	// Determine relays.
	relays := h.cfg.DefaultRelays
	if relayURL != "" {
		relays = append([]string{relayURL}, relays...)
	}

	if err := h.publish.Publish(ctx, relays, responseEvent); err != nil {
		return fmt.Errorf("publish response: %w", err)
	}

	return nil
}
