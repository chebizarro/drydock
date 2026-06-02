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

	// Track suggested fixes for later fix requests.
	fixes  sync.Map // map[string]storedFix
	fixTTL time.Duration
}

// activeSession tracks an IDE session.
type activeSession struct {
	Session     IDESession
	LastSeen    time.Time
	SourceRelay string
}

type storedFix struct {
	SessionID string
	File      string
	Diff      string
	CreatedAt time.Time
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
	switch int(event.Kind) {
	case KindIDESession:
		return h.handleSession(ctx, event, relayURL)
	case KindContextVM:
		return h.handleContextVMEvent(ctx, event, relayURL)
	default:
		return nil
	}
}

// IsIDEEvent checks if an event is an IDE integration event.
func IsIDEEvent(kind nostr.Kind) bool {
	k := int(kind)
	return k == KindIDESession || k == KindContextVM
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

	h.mu.Lock()
	h.sessions[session.SessionID] = &activeSession{
		Session:     session,
		LastSeen:    time.Now(),
		SourceRelay: relayURL,
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
		}, relayURL, "")
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
	switch msg.Method {
	case MethodReviewRequest:
		if req, rpcErr := contextvm.ParamsAs[ReviewRequest](contextvm.Request{Msg: msg}); rpcErr == nil {
			sessionID = req.SessionID
		}
	case MethodApplyFix:
		if req, rpcErr := contextvm.ParamsAs[FixRequest](contextvm.Request{Msg: msg}); rpcErr == nil {
			sessionID = req.SessionID
		}
	}

	if pubErr := h.publishContextVMResponse(ctx, event, resp, relayURL, sessionID); pubErr != nil {
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

	resp, err := h.resolveFixRequest(req)
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

	// Look up the session.
	h.mu.RLock()
	session, ok := h.sessions[req.SessionID]
	h.mu.RUnlock()

	repoPath := ""
	if ok {
		repoPath = session.Session.WorkspacePath
		session.LastSeen = time.Now()
	}

	// Process the review.
	ctx, cancel := context.WithTimeout(ctx, reviewTimeout)
	defer cancel()

	start := time.Now()
	h.cleanupExpiredFixes(start)

	// Build context from the diff.
	bundle, err := h.ctxBuilder.Build(ctx, contextbuilder.BuildInput{
		PatchEventContent: req.Diff,
		RepoPath:          repoPath,
	})
	if err != nil {
		h.logger.Warn("context build failed", "request_id", req.RequestID, "error", err)
		return ReviewResponse{}, fmt.Errorf("failed to build context: %w", err)
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
			h.storeFix(fixID, storedFix{
				SessionID: req.SessionID,
				File:      f.File,
				Diff:      f.SuggestedDiff,
				CreatedAt: start,
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

	metrics.IDEReviewResponsesSent.Inc()
	h.logger.Info("IDE review response created",
		"request_id", req.RequestID,
		"diagnostics", len(diagnostics),
		"time_ms", response.ReviewTimeMs,
	)

	return response, nil
}

func (h *Handler) resolveFixRequest(req FixRequest) (FixResponse, *contextvm.Error) {
	now := time.Now()
	h.cleanupExpiredFixes(now)

	response := FixResponse{
		RequestID: req.RequestID,
		SessionID: req.SessionID,
		FixID:     req.FixID,
	}

	fix, ok := h.lookupFix(req.FixID, now)
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
			{"e", reqEvent.ID.Hex()},     // Reference the request
			{"p", reqEvent.PubKey.Hex()}, // Tag the requester
			{"session", resp.SessionID},  // Session reference
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
func (h *Handler) publishContextVMResponse(ctx context.Context, reqEvent nostr.Event, resp contextvm.Message, relayURL, sessionID string) error {
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

func (h *Handler) storeFix(fixID string, fix storedFix) {
	h.fixes.Store(fixID, fix)
}

func (h *Handler) lookupFix(fixID string, now time.Time) (storedFix, bool) {
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

	return fix, true
}

func (h *Handler) cleanupExpiredFixes(now time.Time) {
	if h.fixTTL <= 0 {
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

// generateFixID creates a deterministic fix ID from finding details.
func generateFixID(requestID string, file string, line, index int) string {
	key := fmt.Sprintf("%s:%s:%d:%d", requestID, file, line, index)
	hash := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", hash[:8])
}
