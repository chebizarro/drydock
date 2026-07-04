package idegateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"drydock/internal/contextbuilder"
	"drydock/internal/contextvm"
	"drydock/internal/db"
	"drydock/internal/metrics"
	"drydock/internal/reviewengine"

	"fiatjaf.com/nostr"
)

const (
	// maxConcurrent limits parallel review requests.
	maxConcurrent = 4

	// reviewTimeout is the max time for processing a review request.
	reviewTimeout = 60 * time.Second

	// fixTTL controls how long suggested fixes are retained server-side.
	fixTTL = 15 * time.Minute
)

// Signer signs Nostr events for publishing responses.
type Signer interface {
	GetPublicKey(ctx context.Context) (nostr.PubKey, error)
	SignEvent(ctx context.Context, evt *nostr.Event) error
}

// RelayPublisher publishes signed events to Nostr relays.
type RelayPublisher interface {
	Publish(ctx context.Context, relays []string, event nostr.Event) error
}

// Config holds IDE gateway configuration.
type Config struct {
	DefaultRelays []string
}

// Handler processes IDE integration events.
type Handler struct {
	cfg        Config
	store      *db.Store
	ctxBuilder *contextbuilder.Builder
	engine     *reviewengine.Engine
	signer     Signer
	publish    RelayPublisher
	logger     *slog.Logger
	ourPubKey  string
	sem        chan struct{}

	// Track active sessions for routing responses
	mu       sync.RWMutex
	sessions map[string]*activeSession

	// Fallback suggested-fix storage for tests that construct a handler without a DB store.
	// Production handlers persist fixes through db.Store.
	fixes  sync.Map // map[string]storedFix
	fixTTL time.Duration
}

// activeSession tracks an IDE session.
type activeSession struct {
	Session     IDESession
	LastSeen    time.Time
	SourceRelay string
	PubKey      string
}

type storedFix struct {
	SessionID    string
	AuthorPubKey string
	File         string
	Diff         string
	CreatedAt    time.Time
}

// New creates a new IDE gateway handler.
func New(
	cfg Config,
	store *db.Store,
	ctxBuilder *contextbuilder.Builder,
	engine *reviewengine.Engine,
	signer Signer,
	relayPub RelayPublisher,
	logger *slog.Logger,
) *Handler {
	var ourPubKey string
	if signer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if pk, err := signer.GetPublicKey(ctx); err == nil {
			ourPubKey = pk.Hex()
		}
	}

	return &Handler{
		cfg:        cfg,
		store:      store,
		ctxBuilder: ctxBuilder,
		engine:     engine,
		signer:     signer,
		publish:    relayPub,
		logger:     logger,
		ourPubKey:  ourPubKey,
		sem:        make(chan struct{}, maxConcurrent),
		sessions:   make(map[string]*activeSession),
		fixTTL:     fixTTL,
	}
}

// HandleEvent processes an IDE-related event.
func (h *Handler) HandleEvent(ctx context.Context, event nostr.Event, relayURL string) error {
	if !event.CheckID() || !event.VerifySignature() {
		h.logger.Warn("rejecting IDE event with invalid signature", "event_id", event.ID.Hex(), "kind", int(event.Kind))
		return nil
	}
	switch int(event.Kind) {
	case KindIDESession:
		return h.handleSession(ctx, event, relayURL)
	case KindIDECommand:
		return h.handleContextVMCommand(ctx, event, relayURL)
	default:
		return nil
	}
}

// HandledKinds returns the Nostr kinds accepted by the IDE gateway.
func HandledKinds() []nostr.Kind {
	return []nostr.Kind{nostr.Kind(KindIDESession), nostr.Kind(KindIDECommand)}
}

// IsHandled checks if a Nostr kind is accepted by the IDE gateway.
func IsHandled(kind nostr.Kind) bool {
	for _, handled := range HandledKinds() {
		if kind == handled {
			return true
		}
	}
	return false
}

// IsIDEEvent checks if an event kind is handled by IDE integration.
func IsIDEEvent(kind nostr.Kind) bool {
	return IsHandled(kind)
}

func (h *Handler) handleContextVMCommand(ctx context.Context, event nostr.Event, relayURL string) error {
	msg, err := contextvm.ParseMessage(event.Content)
	if err != nil {
		h.logger.Warn("invalid ContextVM IDE command", "event_id", event.ID.Hex(), "error", err)
		return nil
	}
	if msg.ID == "" || msg.Method == "" {
		h.logger.Warn("ContextVM IDE command missing id or method", "event_id", event.ID.Hex(), "method", msg.Method)
		return nil
	}

	paramsEvent := event
	paramsEvent.Content = string(msg.Params)
	switch msg.Method {
	case MethodIDEReview:
		return h.handleReviewRequest(ctx, paramsEvent, relayURL, msg.ID)
	case MethodIDEApplyFix:
		return h.handleFixRequest(ctx, paramsEvent, relayURL, msg.ID)
	default:
		return nil
	}
}

// handleSession registers or updates an IDE workspace session.
func (h *Handler) handleSession(ctx context.Context, event nostr.Event, relayURL string) error {
	var session IDESession
	if err := json.Unmarshal([]byte(event.Content), &session); err != nil {
		h.logger.Warn("invalid IDE session event", "event_id", event.ID.Hex(), "error", err)
		return nil
	}

	// Extract session ID from "d" tag.
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "d" {
			session.SessionID = tag[1]
			break
		}
	}

	if session.SessionID == "" {
		h.logger.Warn("IDE session missing session ID", "event_id", event.ID.Hex())
		return nil
	}
	if !h.isAddressedToGateway(event) {
		h.logger.Warn("rejecting IDE session not addressed to this gateway", "event_id", event.ID.Hex(), "session_id", session.SessionID)
		return nil
	}

	sender := event.PubKey.Hex()
	h.mu.Lock()
	if existing, ok := h.sessions[session.SessionID]; ok && existing.PubKey != "" && !strings.EqualFold(existing.PubKey, sender) {
		h.mu.Unlock()
		h.logger.Warn("rejecting IDE session update from unauthorized sender", "event_id", event.ID.Hex(), "session_id", session.SessionID)
		return nil
	}
	h.sessions[session.SessionID] = &activeSession{
		Session:     session,
		LastSeen:    time.Now(),
		SourceRelay: relayURL,
		PubKey:      sender,
	}
	h.mu.Unlock()

	metrics.IDESessionsActive.Inc()
	h.logger.Info("IDE session registered",
		"session_id", session.SessionID,
		"editor", session.Editor,
		"workspace", session.WorkspacePath,
	)

	return nil
}

// handleReviewRequest processes an IDE review request.
func (h *Handler) handleReviewRequest(ctx context.Context, event nostr.Event, relayURL string, contextVMID string) error {
	metrics.IDEReviewRequestsReceived.Inc()

	// Acquire semaphore slot.
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	req, err := ParseReviewRequest(event.Content)
	if err != nil {
		h.logger.Warn("invalid review request", "event_id", event.ID.Hex(), "error", err)
		return nil
	}

	if req.RequestID != "" && req.RequestID != contextVMID {
		h.logger.Warn("review request_id does not match ContextVM id", "event_id", event.ID.Hex(), "request_id", req.RequestID, "contextvm_id", contextVMID)
		return nil
	}
	req.RequestID = contextVMID
	if req.SessionID == "" || req.RequestID == "" {
		h.logger.Warn("review request missing session_id or request_id", "event_id", event.ID.Hex())
		return nil
	}
	if !h.validateRequestEnvelope(event, req.SessionID, req.RequestID) {
		return nil
	}
	if req.Diff == "" {
		h.logger.Debug("empty diff in review request", "event_id", event.ID.Hex())
		return nil
	}

	// Look up the session and verify the request author owns it.
	h.mu.Lock()
	session, ok := h.sessions[req.SessionID]
	if !ok || session.PubKey == "" || !strings.EqualFold(session.PubKey, event.PubKey.Hex()) {
		h.mu.Unlock()
		h.logger.Warn("rejecting review request from unauthorized session sender", "event_id", event.ID.Hex(), "session_id", req.SessionID)
		return nil
	}
	session.LastSeen = time.Now()
	repoPath := session.Session.WorkspacePath
	h.mu.Unlock()

	// Process the review.
	ctx, cancel := context.WithTimeout(ctx, reviewTimeout)
	defer cancel()

	start := time.Now()
	h.cleanupExpiredFixes(ctx, start)

	// Build context from the diff.
	bundle, err := h.ctxBuilder.Build(ctx, contextbuilder.BuildInput{
		PatchEventContent: req.Diff,
		RepoPath:          repoPath,
	})
	if err != nil {
		h.logger.Warn("context build failed", "request_id", req.RequestID, "error", err)
		return h.publishErrorResponse(ctx, event, req, relayURL, "Failed to build context: "+err.Error())
	}

	// Run the review engine.
	result, err := h.engine.Run(ctx, reviewengine.RunInput{
		ContextBundle:   bundle.Content,
		ChangedFiles:    req.ChangedFiles,
		SkipWalkthrough: true, // IDEs don't need walkthrough
	})
	if err != nil {
		h.logger.Warn("review engine failed", "request_id", req.RequestID, "error", err)
		return h.publishErrorResponse(ctx, event, req, relayURL, "Review failed: "+err.Error())
	}

	// Convert findings to diagnostics.
	diagnostics := make([]Diagnostic, 0, len(result.Review.Findings))
	for i, f := range result.Review.Findings {
		fixID := ""
		if f.HasSuggestion() {
			fixID = generateFixID(req.RequestID, f.File, f.Line, i)
			h.storeFix(ctx, fixID, storedFix{
				SessionID:    req.SessionID,
				AuthorPubKey: event.PubKey.Hex(),
				File:         f.File,
				Diff:         f.SuggestedDiff,
				CreatedAt:    start,
			})
		}
		diagnostics = append(diagnostics, FindingToDiagnostic(f, fixID))
	}

	// Build response.
	response := ReviewResponse{
		RequestID:    req.RequestID,
		SessionID:    req.SessionID,
		Diagnostics:  diagnostics,
		Summary:      result.Review.Summary,
		ReviewTimeMs: time.Since(start).Milliseconds(),
	}

	// Publish response.
	if err := h.publishReviewResponse(ctx, event, response, relayURL); err != nil {
		metrics.IDEReviewErrors.Inc()
		return err
	}

	metrics.IDEReviewResponsesSent.Inc()
	h.logger.Info("IDE review response sent",
		"request_id", req.RequestID,
		"diagnostics", len(diagnostics),
		"time_ms", response.ReviewTimeMs,
	)

	return nil
}

// handleFixRequest processes an IDE fix application request.
func (h *Handler) handleFixRequest(ctx context.Context, event nostr.Event, relayURL string, contextVMID string) error {
	metrics.IDEFixRequestsReceived.Inc()

	req, err := ParseFixRequest(event.Content)
	if err != nil {
		h.logger.Warn("invalid fix request", "event_id", event.ID.Hex(), "error", err)
		return nil
	}

	if req.RequestID != "" && req.RequestID != contextVMID {
		h.logger.Warn("fix request_id does not match ContextVM id", "event_id", event.ID.Hex(), "request_id", req.RequestID, "contextvm_id", contextVMID)
		return nil
	}
	req.RequestID = contextVMID
	if req.SessionID == "" || req.RequestID == "" {
		h.logger.Warn("fix request missing session_id or request_id", "event_id", event.ID.Hex())
		return nil
	}
	if !h.validateRequestEnvelope(event, req.SessionID, req.RequestID) {
		return nil
	}

	now := time.Now()
	h.cleanupExpiredFixes(ctx, now)

	response := FixResponse{
		RequestID: req.RequestID,
		SessionID: req.SessionID,
	}

	fix, ok := h.lookupFix(ctx, req.FixID, req.SessionID, now)
	if ok && fix.AuthorPubKey != "" && !strings.EqualFold(fix.AuthorPubKey, event.PubKey.Hex()) {
		h.logger.Warn("rejecting fix request from unauthorized sender", "event_id", event.ID.Hex(), "session_id", req.SessionID)
		return nil
	}
	switch {
	case req.FixID == "":
		response.Success = false
		response.Error = "missing fix_id"
	case !ok:
		response.Success = false
		response.Error = "fix not found or expired"
	case fix.SessionID != req.SessionID:
		response.Success = false
		response.Error = "fix does not belong to this session"
	case req.File != "" && fix.File != "" && fix.File != req.File:
		response.Success = false
		response.Error = "fix does not match requested file"
	default:
		response.Success = true
		response.Diff = fix.Diff
	}

	if err := h.publishFixResponse(ctx, event, response, relayURL); err != nil {
		return err
	}

	metrics.IDEFixResponsesSent.Inc()
	h.logger.Info("IDE fix response sent", "request_id", req.RequestID, "success", response.Success)

	return nil
}

// publishReviewResponse publishes a ContextVM review response event.
func (h *Handler) publishReviewResponse(ctx context.Context, reqEvent nostr.Event, resp ReviewResponse, relayURL string) error {
	content, err := contextvm.MarshalResult(resp.RequestID, resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}

	responseEvent := nostr.Event{
		Kind:      nostr.Kind(KindIDEReviewResponse),
		CreatedAt: nostr.Now(),
		Content:   content,
		Tags: nostr.Tags{
			{"e", reqEvent.ID.Hex()},     // Reference the request event
			{"p", reqEvent.PubKey.Hex()}, // Tag the requester
			{"session", resp.SessionID},  // Session reference
			{"request", resp.RequestID},  // ContextVM request correlation
			{"method", MethodIDEReview},
			{"t", "drydock-ide"},
		},
	}

	if err := h.signer.SignEvent(ctx, &responseEvent); err != nil {
		return fmt.Errorf("sign response: %w", err)
	}

	relays := h.resolveRelays(relayURL)
	if err := h.publish.Publish(ctx, relays, responseEvent); err != nil {
		return fmt.Errorf("publish response: %w", err)
	}

	return nil
}

// publishErrorResponse publishes an error response.
func (h *Handler) publishErrorResponse(ctx context.Context, reqEvent nostr.Event, req ReviewRequest, relayURL, errMsg string) error {
	resp := ReviewResponse{
		RequestID:   req.RequestID,
		SessionID:   req.SessionID,
		Diagnostics: nil,
		Summary:     errMsg,
	}
	return h.publishReviewResponse(ctx, reqEvent, resp, relayURL)
}

// publishFixResponse publishes a ContextVM fix response event.
func (h *Handler) publishFixResponse(ctx context.Context, reqEvent nostr.Event, resp FixResponse, relayURL string) error {
	content, err := contextvm.MarshalResult(resp.RequestID, resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}

	responseEvent := nostr.Event{
		Kind:      nostr.Kind(KindIDEFixResponse),
		CreatedAt: nostr.Now(),
		Content:   content,
		Tags: nostr.Tags{
			{"e", reqEvent.ID.Hex()},
			{"p", reqEvent.PubKey.Hex()},
			{"session", resp.SessionID},
			{"request", resp.RequestID},
			{"method", MethodIDEApplyFix},
			{"t", "drydock-ide"},
		},
	}

	if err := h.signer.SignEvent(ctx, &responseEvent); err != nil {
		return fmt.Errorf("sign response: %w", err)
	}

	relays := h.resolveRelays(relayURL)
	if err := h.publish.Publish(ctx, relays, responseEvent); err != nil {
		return fmt.Errorf("publish response: %w", err)
	}

	return nil
}

func (h *Handler) resolveRelays(relayURL string) []string {
	relays := h.cfg.DefaultRelays
	if relayURL != "" {
		relays = append([]string{relayURL}, relays...)
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
	return deduped
}

// CleanupStaleSessions removes sessions that haven't been seen recently.
func (h *Handler) CleanupStaleSessions(maxAge time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	for id, session := range h.sessions {
		if now.Sub(session.LastSeen) > maxAge {
			delete(h.sessions, id)
			metrics.IDESessionsActive.Dec()
			h.logger.Debug("cleaned up stale IDE session", "session_id", id)
		}
	}
}

func (h *Handler) storeFix(ctx context.Context, fixID string, fix storedFix) {
	if h.store != nil {
		if err := h.store.UpsertIDEGatewayFix(ctx, db.IDEGatewayFix{
			FixID:        fixID,
			SessionID:    fix.SessionID,
			AuthorPubKey: fix.AuthorPubKey,
			File:         fix.File,
			Diff:         fix.Diff,
			CreatedAt:    fix.CreatedAt.Unix(),
		}); err != nil {
			h.logger.Warn("failed to persist IDE suggested fix", "fix_id", fixID, "session_id", fix.SessionID, "error", err)
		}
		return
	}

	h.fixes.Store(fixID, fix)
}

func (h *Handler) lookupFix(ctx context.Context, fixID, sessionID string, now time.Time) (storedFix, bool) {
	if h.store != nil {
		rec, ok, err := h.store.GetIDEGatewayFix(ctx, fixID, sessionID)
		if err != nil {
			h.logger.Warn("failed to load IDE suggested fix", "fix_id", fixID, "session_id", sessionID, "error", err)
			return storedFix{}, false
		}
		if !ok {
			return storedFix{}, false
		}
		createdAt := time.Unix(rec.CreatedAt, 0)
		if h.fixTTL > 0 && now.Sub(createdAt) > h.fixTTL {
			return storedFix{}, false
		}
		return storedFix{
			SessionID:    rec.SessionID,
			AuthorPubKey: rec.AuthorPubKey,
			File:         rec.File,
			Diff:         rec.Diff,
			CreatedAt:    createdAt,
		}, true
	}

	value, ok := h.fixes.Load(fixID)
	if !ok {
		return storedFix{}, false
	}

	fix, ok := value.(storedFix)
	if !ok {
		h.fixes.Delete(fixID)
		return storedFix{}, false
	}

	if h.fixTTL > 0 && now.Sub(fix.CreatedAt) > h.fixTTL {
		h.fixes.Delete(fixID)
		return storedFix{}, false
	}
	if fix.SessionID != sessionID {
		return storedFix{}, false
	}

	return fix, true
}

func (h *Handler) cleanupExpiredFixes(ctx context.Context, now time.Time) {
	if h.fixTTL <= 0 {
		return
	}

	if h.store != nil {
		if err := h.store.DeleteExpiredIDEGatewayFixes(ctx, now.Add(-h.fixTTL).Unix()); err != nil {
			h.logger.Warn("failed to delete expired IDE suggested fixes", "error", err)
		}
		return
	}

	h.fixes.Range(func(key, value any) bool {
		fix, ok := value.(storedFix)
		if !ok || now.Sub(fix.CreatedAt) > h.fixTTL {
			h.fixes.Delete(key)
		}
		return true
	})
}

func (h *Handler) validateRequestEnvelope(event nostr.Event, sessionID, requestID string) bool {
	if !h.isAddressedToGateway(event) {
		h.logger.Warn("rejecting IDE request not addressed to this gateway", "event_id", event.ID.Hex(), "session_id", sessionID, "request_id", requestID)
		return false
	}
	if !hasTagValue(event.Tags, "session", sessionID) {
		h.logger.Warn("rejecting IDE request missing matching session tag", "event_id", event.ID.Hex(), "session_id", sessionID, "request_id", requestID)
		return false
	}
	if !hasTagValue(event.Tags, "request", requestID) {
		h.logger.Warn("rejecting IDE request missing matching request tag", "event_id", event.ID.Hex(), "session_id", sessionID, "request_id", requestID)
		return false
	}
	return true
}

func (h *Handler) isAddressedToGateway(event nostr.Event) bool {
	if h.ourPubKey == "" {
		return false
	}
	return hasTagValue(event.Tags, "p", h.ourPubKey)
}

func hasTagValue(tags nostr.Tags, name, value string) bool {
	for _, tag := range tags {
		if len(tag) < 2 || tag[0] != name {
			continue
		}
		if strings.EqualFold(tag[1], value) {
			return true
		}
	}
	return false
}

// generateFixID creates a deterministic fix ID from finding details.
func generateFixID(requestID string, file string, line, index int) string {
	key := fmt.Sprintf("%s:%s:%d:%d", requestID, file, line, index)
	hash := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", hash[:8])
}
