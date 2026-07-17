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
	case KindContextVM:
		return h.handleContextVMEvent(ctx, event, relayURL)
	default:
		return nil
	}
}

// HandledKinds returns the Nostr kinds accepted by the IDE gateway.
func HandledKinds() []nostr.Kind {
	return []nostr.Kind{nostr.Kind(KindIDESession), nostr.Kind(KindContextVM)}
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

// IsIDEEvent checks if an event is an IDE integration event.
func IsIDEEvent(kind nostr.Kind) bool {
	return IsHandled(kind)
}

// handleSession registers or updates an IDE workspace session.
func (h *Handler) handleSession(ctx context.Context, event nostr.Event, relayURL string) error {
	var session IDESession
	if err := json.Unmarshal([]byte(event.Content), &session); err != nil {
		h.logger.Warn("invalid IDE session event", "event_id", event.ID.Hex(), "error", err)
		return nil
	}

	// Extract session ID from NIP-78 "d" tag.
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "d" {
			if strings.HasPrefix(tag[1], BuildSessionDTag("")) {
				session.SessionID = strings.TrimPrefix(tag[1], BuildSessionDTag(""))
			} else if session.SessionID == "" {
				session.SessionID = tag[1]
			}
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

// RegisterContextVMHandlers registers IDE gateway ContextVM methods.
func (h *Handler) RegisterContextVMHandlers(router *contextvm.Router) error {
	if err := router.Register(MethodReviewRequest, h.HandleReviewRequest); err != nil {
		return err
	}
	return router.Register(MethodApplyFix, h.HandleApplyFixRequest)
}

// handleContextVMEvent routes IDE ContextVM requests and publishes JSON-RPC responses.
func (h *Handler) handleContextVMEvent(ctx context.Context, event nostr.Event, relayURL string) error {
	var msg contextvm.Message
	if err := json.Unmarshal([]byte(event.Content), &msg); err != nil {
		h.logger.Warn("invalid ContextVM message", "event_id", event.ID.Hex(), "error", err)
		return h.publishContextVMResponse(ctx, event, contextvm.Message{
			JSONRPC: "2.0",
			ID:      event.ID.Hex(),
			Error:   &contextvm.Error{Code: contextvm.ErrorParseError, Message: "parse error"},
		}, relayURL, "", "")
	}

	// Ignore responses and methods owned by other ContextVM handlers.
	if msg.Method == "" || (msg.Method != MethodReviewRequest && msg.Method != MethodApplyFix) {
		return nil
	}

	router := contextvm.NewRouter()
	if err := h.RegisterContextVMHandlers(router); err != nil {
		return err
	}
	resp, err := router.Handle(ctx, contextvm.Request{
		Event:  event,
		Relay:  relayURL,
		Sender: event.PubKey,
		Msg:    msg,
	})
	if err != nil {
		h.logger.Warn("ContextVM handler failed", "event_id", event.ID.Hex(), "method", msg.Method, "error", err)
	}

	sessionID := ""
	fixID := ""
	switch msg.Method {
	case MethodReviewRequest:
		if req, rpcErr := contextvm.ParamsAs[ReviewRequest](contextvm.Request{Msg: msg}); rpcErr == nil {
			sessionID = req.SessionID
		}
	case MethodApplyFix:
		if req, rpcErr := contextvm.ParamsAs[FixRequest](contextvm.Request{Msg: msg}); rpcErr == nil {
			sessionID = req.SessionID
			fixID = req.FixID
		}
	}

	if pubErr := h.publishContextVMResponse(ctx, event, resp, relayURL, sessionID, fixID); pubErr != nil {
		return pubErr
	}
	if msg.Method == MethodApplyFix {
		metrics.IDEFixResponsesSent.Inc()
	}
	return err
}

// HandleReviewRequest processes a ContextVM IDE review request.
func (h *Handler) HandleReviewRequest(ctx context.Context, rpcReq contextvm.Request) (any, *contextvm.Error) {
	req, rpcErr := contextvm.ParamsAs[ReviewRequest](rpcReq)
	if rpcErr != nil {
		h.logger.Warn("invalid review request params", "event_id", rpcReq.Event.ID.Hex(), "error", rpcErr.Message)
		return nil, rpcErr
	}
	if req.RequestID == "" {
		req.RequestID = rpcReq.Msg.ID
	}
	if req.SessionID == "" || req.RequestID == "" {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "session_id and request_id are required"}
	}
	if !h.validateRequestEnvelope(rpcReq.Event, req.SessionID, req.RequestID) {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidRequest, Message: "request is not addressed to this IDE session/gateway"}
	}
	resp, err := h.processReviewRequest(ctx, rpcReq.Event, rpcReq.Relay, req)
	if err != nil {
		return nil, &contextvm.Error{Code: contextvm.ErrorInternal, Message: err.Error()}
	}
	return resp, nil
}

// HandleApplyFixRequest processes a ContextVM IDE fix application request.
func (h *Handler) HandleApplyFixRequest(ctx context.Context, rpcReq contextvm.Request) (any, *contextvm.Error) {
	metrics.IDEFixRequestsReceived.Inc()

	req, rpcErr := contextvm.ParamsAs[FixRequest](rpcReq)
	if rpcErr != nil {
		h.logger.Warn("invalid fix request params", "event_id", rpcReq.Event.ID.Hex(), "error", rpcErr.Message)
		return nil, rpcErr
	}
	if req.RequestID == "" {
		req.RequestID = rpcReq.Msg.ID
	}
	if req.SessionID == "" || req.RequestID == "" {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "session_id and request_id are required"}
	}
	if !h.validateRequestEnvelope(rpcReq.Event, req.SessionID, req.RequestID) {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidRequest, Message: "request is not addressed to this IDE session/gateway"}
	}

	resp, err := h.resolveFixRequest(ctx, rpcReq.Event, req)
	if err != nil {
		h.logger.Warn("fix request failed", "request_id", req.RequestID, "fix_id", req.FixID, "error", err.Message)
		return nil, err
	}
	h.logger.Info("IDE fix response created", "request_id", req.RequestID, "fix_id", req.FixID, "success", resp.Success)
	return resp, nil
}

// processReviewRequest processes an IDE review request.
func (h *Handler) processReviewRequest(ctx context.Context, event nostr.Event, _ string, req ReviewRequest) (ReviewResponse, error) {
	metrics.IDEReviewRequestsReceived.Inc()

	// Acquire semaphore slot.
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-ctx.Done():
		return ReviewResponse{}, ctx.Err()
	}

	if req.Diff == "" {
		h.logger.Debug("empty diff in review request", "event_id", event.ID.Hex())
		return ReviewResponse{}, fmt.Errorf("empty diff in review request")
	}

	// Look up the session and verify the request author owns it.
	h.mu.Lock()
	session, ok := h.sessions[req.SessionID]
	if !ok || session.PubKey == "" || !strings.EqualFold(session.PubKey, event.PubKey.Hex()) {
		h.mu.Unlock()
		h.logger.Warn("rejecting review request from unauthorized session sender", "event_id", event.ID.Hex(), "session_id", req.SessionID)
		return ReviewResponse{}, fmt.Errorf("unauthorized IDE session sender")
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
		return ReviewResponse{}, fmt.Errorf("failed to build context: %w", err)
	}
	for _, status := range bundle.LayerStatuses {
		metrics.ContextLayersByStatus.With(status.Status).Inc()
		if status.Status != "used" {
			h.logger.Warn("context layer not fully available", "request_id", req.RequestID, "layer", status.Layer, "status", status.Status, "message", status.Message)
		}
	}

	// Run the review engine.
	result, err := h.engine.Run(ctx, reviewengine.RunInput{
		ContextBundle:   bundle.Content,
		ChangedFiles:    req.ChangedFiles,
		SkipWalkthrough: true, // IDEs don't need walkthrough
	})
	if err != nil {
		h.logger.Warn("review engine failed", "request_id", req.RequestID, "error", err)
		return ReviewResponse{}, fmt.Errorf("review failed: %w", err)
	}

	// Convert findings to diagnostics.
	diagnostics := make([]Diagnostic, 0, len(result.Review.Findings))
	for i, f := range result.Review.Findings {
		fixID := ""
		if f.HasSuggestion() {
			fixID = generateFixID(req.RequestID, f.File, f.Line, i)
			if err := h.storeFix(ctx, fixID, storedFix{
				SessionID:    req.SessionID,
				AuthorPubKey: event.PubKey.Hex(),
				File:         f.File,
				Diff:         f.SuggestedDiff,
				CreatedAt:    start,
			}); err != nil {
				return ReviewResponse{}, fmt.Errorf("persist suggested fix %s: %w", fixID, err)
			}
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

	metrics.IDEReviewResponsesSent.Inc()
	h.logger.Info("IDE review response created",
		"request_id", req.RequestID,
		"diagnostics", len(diagnostics),
		"time_ms", response.ReviewTimeMs,
	)

	return response, nil
}

func (h *Handler) resolveFixRequest(ctx context.Context, event nostr.Event, req FixRequest) (FixResponse, *contextvm.Error) {
	now := time.Now()
	h.cleanupExpiredFixes(ctx, now)

	response := FixResponse{
		RequestID: req.RequestID,
		SessionID: req.SessionID,
		FixID:     req.FixID,
	}

	fix, ok := h.lookupFix(ctx, req.FixID, req.SessionID, now)
	if ok && fix.AuthorPubKey != "" && !strings.EqualFold(fix.AuthorPubKey, event.PubKey.Hex()) {
		return response, &contextvm.Error{Code: contextvm.ErrorInvalidRequest, Message: "fix does not belong to this requester"}
	}
	switch {
	case req.FixID == "":
		return response, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "missing fix_id"}
	case !ok:
		return response, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "fix not found or expired"}
	case fix.SessionID != req.SessionID:
		return response, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "fix does not belong to this session"}
	case req.File != "" && fix.File != "" && fix.File != req.File:
		return response, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "fix does not match requested file"}
	default:
		response.Success = true
		response.Patch = fix.Diff
		response.Diff = fix.Diff
	}

	return response, nil
}

// publishReviewResponse publishes a ContextVM JSON-RPC review response event.
func (h *Handler) publishReviewResponse(ctx context.Context, reqEvent nostr.Event, resp ReviewResponse, relayURL string) error {
	rpcResp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      resp.RequestID,
		Result:  resp,
	}

	content, err := json.Marshal(rpcResp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}

	responseEvent := nostr.Event{
		Kind:      nostr.Kind(KindContextVM),
		CreatedAt: nostr.Now(),
		Content:   string(content),
		Tags: nostr.Tags{
			{"e", reqEvent.ID.Hex()},     // Reference the request event
			{"p", reqEvent.PubKey.Hex()}, // Tag the requester
			{"session", resp.SessionID},  // Session reference
			{"request", resp.RequestID},  // Request correlation
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

// publishContextVMResponse publishes a ContextVM JSON-RPC response event.
func (h *Handler) publishContextVMResponse(ctx context.Context, reqEvent nostr.Event, resp contextvm.Message, relayURL, sessionID, fixID string) error {
	content, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}

	tags := nostr.Tags{
		{"e", reqEvent.ID.Hex()},
		{"p", reqEvent.PubKey.Hex()},
	}
	if sessionID != "" {
		tags = append(tags, nostr.Tag{"session", sessionID})
	}
	if resp.ID != "" {
		tags = append(tags, nostr.Tag{"request", resp.ID})
	}
	if fixID != "" {
		tags = append(tags, nostr.Tag{"fix", fixID})
	}

	responseEvent := nostr.Event{
		Kind:      nostr.Kind(KindContextVM),
		CreatedAt: nostr.Now(),
		Content:   string(content),
		Tags:      tags,
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

func (h *Handler) storeFix(ctx context.Context, fixID string, fix storedFix) error {
	if h.store != nil {
		if err := h.store.UpsertIDEGatewayFix(ctx, db.IDEGatewayFix{
			FixID:        fixID,
			SessionID:    fix.SessionID,
			AuthorPubKey: fix.AuthorPubKey,
			File:         fix.File,
			Diff:         fix.Diff,
			CreatedAt:    fix.CreatedAt.Unix(),
		}); err != nil {
			return fmt.Errorf("upsert IDE suggested fix: %w", err)
		}
		return nil
	}

	h.fixes.Store(fixID, fix)
	return nil
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
