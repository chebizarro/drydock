package idegateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"drydock/internal/agenticreview"
	"drydock/internal/contextbuilder"
	"drydock/internal/contextvm"
	"drydock/internal/db"
	"drydock/internal/metrics"
	"drydock/internal/reviewengine"
	"drydock/internal/revieworder"
	"drydock/internal/reviewsession"
	"drydock/internal/targetidentity"
	"drydock/internal/workspacesnapshot"

	"fiatjaf.com/nostr"
)

const (
	// maxConcurrent limits parallel review requests.
	maxConcurrent = 4

	// defaultAgenticTimeout allows iterative discovery/review turns to finish.
	defaultAgenticTimeout = 10 * time.Minute

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

// PatchOrderer admits stored patch requests through the shared review service.
type PatchOrderer interface {
	SubmitOnDemand(context.Context, revieworder.OnDemandRequest) (revieworder.AcceptedOrder, error)
}

// Config holds IDE gateway configuration.
type Config struct {
	DefaultRelays  []string
	AgenticTimeout time.Duration
	// WorkspaceBindings maps lowercase IDE pubkeys to the exact operator-approved
	// filesystem roots that identity may freeze for inline review.
	WorkspaceBindings map[string][]string
}

// Handler processes IDE integration events.
type Handler struct {
	cfg        Config
	store      *db.Store
	ctxBuilder *contextbuilder.Builder
	engine     *reviewengine.Engine
	agenticSvc *agenticreview.Service
	signer     Signer
	publish    RelayPublisher
	logger     *slog.Logger
	ourPubKey  string
	sem        chan struct{}

	patchOrderer PatchOrderer

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
	opts ...func(*Handler),
) *Handler {
	var ourPubKey string
	if signer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if pk, err := signer.GetPublicKey(ctx); err == nil {
			ourPubKey = pk.Hex()
		}
	}

	if cfg.AgenticTimeout <= 0 {
		cfg.AgenticTimeout = defaultAgenticTimeout
	}

	h := &Handler{
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
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// WithAgenticReviewService configures shared inline review preparation and sessions.
func WithAgenticReviewService(service *agenticreview.Service) func(*Handler) {
	return func(h *Handler) { h.agenticSvc = service }
}

// WithPatchOrderer configures shared stored-patch admission.
func WithPatchOrderer(orderer PatchOrderer) func(*Handler) {
	return func(h *Handler) { h.patchOrderer = orderer }
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
	if strings.TrimSpace(session.WorkspacePath) != "" {
		workspace, err := authorizedWorkspaceRoot(session.WorkspacePath, h.cfg.WorkspaceBindings[strings.ToLower(sender)])
		if err != nil {
			h.logger.Warn("IDE session workspace is not operator-authorized", "event_id", event.ID.Hex(), "session_id", session.SessionID)
			session.WorkspacePath = ""
		} else {
			session.WorkspacePath = workspace
		}
	}

	h.mu.Lock()
	if existing, ok := h.sessions[session.SessionID]; ok {
		if existing.PubKey != "" && !strings.EqualFold(existing.PubKey, sender) {
			h.mu.Unlock()
			h.logger.Warn("rejecting IDE session update from unauthorized sender", "event_id", event.ID.Hex(), "session_id", session.SessionID)
			return nil
		}
		if existing.Session.WorkspacePath != "" && session.WorkspacePath != existing.Session.WorkspacePath {
			h.mu.Unlock()
			h.logger.Warn("rejecting IDE session workspace rebinding", "event_id", event.ID.Hex(), "session_id", session.SessionID)
			return nil
		}
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
	if resp.ID == "" {
		return err
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
	if req.PatchEventID != "" {
		resp, patchErr := h.processPatchReviewRequest(ctx, rpcReq.Event, req)
		if patchErr != nil {
			return nil, patchErr
		}
		return resp, nil
	}
	if req.Force {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "force requires patch_event_id"}
	}
	resp, err := h.processReviewRequest(ctx, rpcReq.Event, rpcReq.Relay, req)
	if err != nil {
		switch {
		case errors.Is(err, reviewsession.ErrVersionConflict), errors.Is(err, reviewsession.ErrActiveTurn),
			errors.Is(err, reviewsession.ErrRequestInProgress), errors.Is(err, reviewsession.ErrIdempotencyConflict):
			return nil, &contextvm.Error{Code: contextvm.ErrorConflict, Message: err.Error()}
		case errors.Is(err, reviewsession.ErrOwnerMismatch):
			return nil, &contextvm.Error{Code: contextvm.ErrorUnauthorized, Message: err.Error()}
		case errors.Is(err, reviewsession.ErrNotFound), errors.Is(err, reviewsession.ErrExpired),
			errors.Is(err, reviewsession.ErrBroken):
			return nil, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: err.Error()}
		default:
			return nil, &contextvm.Error{Code: contextvm.ErrorInternal, Message: err.Error()}
		}
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

// processPatchReviewRequest validates IDE session ownership, then delegates all
// stored-target policy, payment, force, claim, and queue behavior to revieworder.
func (h *Handler) processPatchReviewRequest(ctx context.Context, event nostr.Event, req ReviewRequest) (ReviewResponse, *contextvm.Error) {
	metrics.IDEReviewRequestsReceived.Inc()

	h.mu.Lock()
	session, ok := h.sessions[req.SessionID]
	if !ok || session.PubKey == "" || !strings.EqualFold(session.PubKey, event.PubKey.Hex()) {
		h.mu.Unlock()
		return ReviewResponse{}, &contextvm.Error{Code: contextvm.ErrorUnauthorized, Message: "unauthorized IDE session sender"}
	}
	session.LastSeen = time.Now()
	h.mu.Unlock()

	if h.patchOrderer == nil {
		return ReviewResponse{}, &contextvm.Error{Code: contextvm.ErrorInternal, Message: "patch review gateway is not configured"}
	}
	accepted, err := h.patchOrderer.SubmitOnDemand(ctx, revieworder.OnDemandRequest{
		PatchEventID:    req.PatchEventID,
		RequesterPubkey: event.PubKey.Hex(),
		OrderID:         req.RequestID,
		RequestEventID:  event.ID.Hex(),
		Force:           req.Force,
		Invocation:      db.ReviewInvocationIDE,
	})
	if err != nil {
		switch {
		case errors.Is(err, revieworder.ErrInvalidTarget):
			return ReviewResponse{}, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: err.Error()}
		case errors.Is(err, revieworder.ErrTargetNotFound):
			return ReviewResponse{}, &contextvm.Error{Code: contextvm.ErrorNotFound, Message: err.Error()}
		case errors.Is(err, revieworder.ErrSecurityCeiling),
			errors.Is(err, revieworder.ErrForceDenied),
			errors.Is(err, revieworder.ErrPaymentDenied):
			return ReviewResponse{}, &contextvm.Error{Code: contextvm.ErrorUnauthorized, Message: err.Error()}
		case errors.Is(err, revieworder.ErrOrderConflict):
			return ReviewResponse{}, &contextvm.Error{Code: contextvm.ErrorConflict, Message: err.Error()}
		default:
			return ReviewResponse{}, &contextvm.Error{Code: contextvm.ErrorInternal, Message: err.Error()}
		}
	}

	summary := "Patch review queued for asynchronous processing."
	if accepted.RetryPending {
		summary = "Patch review accepted; queue retry is pending."
	}
	return ReviewResponse{
		RequestID: req.RequestID, SessionID: req.SessionID,
		PatchEventID: accepted.Task.PatchEventID, Queued: accepted.Queued, Forced: accepted.Task.Force,
		Summary: summary,
	}, nil
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

	isContinuation := strings.TrimSpace(req.ChatID) != ""
	if !isContinuation && strings.TrimSpace(req.Diff) == "" {
		h.logger.Debug("empty diff in review request", "event_id", event.ID.Hex())
		return ReviewResponse{}, fmt.Errorf("empty diff in initial review request")
	}
	if isContinuation && (req.ExpectedVersion == nil || *req.ExpectedVersion < 0 || strings.TrimSpace(req.Message) == "") {
		return ReviewResponse{}, fmt.Errorf("chat_id follow-ups require non-negative expected_version and message")
	}
	if h.agenticSvc == nil {
		return ReviewResponse{}, fmt.Errorf("agentic review service is not configured")
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
	ideSession := session.Session
	repoPath := ideSession.WorkspacePath
	h.mu.Unlock()

	// Process the iterative review with the configured agentic deadline.
	ctx, cancel := context.WithTimeout(ctx, h.cfg.AgenticTimeout)
	defer cancel()

	start := time.Now()
	h.cleanupExpiredFixes(ctx, start)

	owner := reviewsession.Owner{Kind: "ide", ID: strings.ToLower(event.PubKey.Hex())}
	var sessionResult agenticreview.SessionResult
	var err error
	if isContinuation {
		sessionResult, err = h.agenticSvc.Continue(ctx, agenticreview.ContinueInput{
			ChatID: req.ChatID, Owner: owner, RequestID: req.RequestID,
			ExpectedVersion: int(*req.ExpectedVersion), Message: req.Message,
		})
	} else {
		if repoPath == "" {
			return ReviewResponse{}, fmt.Errorf("IDE workspace is not operator-authorized for filesystem review")
		}
		analysis, analyzeErr := contextbuilder.NewPatchFacade().Analyze(contextbuilder.PatchAnalysisRequest{Diff: req.Diff})
		if analyzeErr != nil {
			return ReviewResponse{}, fmt.Errorf("analyze inline patch: %w", analyzeErr)
		}
		prepared, prepareErr := h.agenticSvc.Prepare(ctx, agenticreview.PrepareInput{
			Mode: reviewsession.ModeInlinePatch,
			Snapshot: agenticreview.SnapshotSpec{
				Kind: workspacesnapshot.KindMutableCopy, WorkspacePath: repoPath,
				PatchRef: req.RequestID, Allowlist: []string{"."},
			},
			Patch: analysis.FilteredDiff,
			BuildInput: contextbuilder.BuildInput{
				PatchEventContent: analysis.FilteredDiff, RepoPath: repoPath,
			},
			Target: inlineTarget(ideSession, req, repoPath, analysis.FilteredDiff),
		})
		if prepareErr != nil {
			h.logger.Warn("agentic preparation failed", "request_id", req.RequestID, "error", prepareErr)
			return ReviewResponse{}, fmt.Errorf("prepare inline review: %w", prepareErr)
		}
		defer h.agenticSvc.ReleasePrepared(prepared)
		bundle := prepared.Bundle()
		if !samePathSet(bundle.ChangedFiles, analysis.ChangedFiles) {
			return ReviewResponse{}, fmt.Errorf("finalized bundle changed files disagree with authoritative patch")
		}
		for _, status := range bundle.LayerStatuses {
			metrics.ContextLayersByStatus.With(status.Status).Inc()
			if status.Status != "used" {
				h.logger.Warn("context layer not fully available", "request_id", req.RequestID, "layer", status.Layer, "status", status.Status, "message", status.Message)
			}
		}
		sessionResult, err = h.agenticSvc.StartSession(ctx, agenticreview.StartSessionInput{
			Prepared: prepared, Owner: owner, RequestID: req.RequestID,
			Options: agenticreview.ReviewOptions{SkipWalkthrough: true},
		})
	}
	if err != nil {
		h.logger.Warn("agentic review failed", "request_id", req.RequestID, "chat_id", req.ChatID, "error", err)
		return ReviewResponse{}, fmt.Errorf("review failed: %w", err)
	}
	result := sessionResult.Output

	// Convert findings to diagnostics. Fix identifiers are scoped to the
	// persisted chat turn, not the transport request alone.
	diagnostics := make([]Diagnostic, 0, len(result.Review.Findings))
	for i, f := range result.Review.Findings {
		fixID := ""
		if f.HasSuggestion() {
			fixID = generateFixID(sessionResult.ChatID, sessionResult.Version, f.File, f.Line, i)
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

	// Build response. expected_version is the version clients must echo on
	// their next optional follow-up request.
	version := int64(sessionResult.Version)
	response := ReviewResponse{
		RequestID:       req.RequestID,
		SessionID:       req.SessionID,
		Diagnostics:     diagnostics,
		Summary:         result.Review.Summary,
		ReviewTimeMs:    time.Since(start).Milliseconds(),
		ChatID:          sessionResult.ChatID,
		ExpectedVersion: &version,
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

func authorizedWorkspaceRoot(requested string, approved []string) (string, error) {
	if strings.TrimSpace(requested) == "" || !filepath.IsAbs(requested) {
		return "", fmt.Errorf("IDE workspace path must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return "", fmt.Errorf("resolve IDE workspace: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve IDE workspace: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("IDE workspace is not a directory")
	}
	for _, root := range approved {
		if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
			continue
		}
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		canonical, err = filepath.Abs(canonical)
		if err == nil && canonical == resolved {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("IDE workspace is outside the operator-approved roots")
}

func inlineTarget(session IDESession, req ReviewRequest, repoPath, patch string) agenticreview.TargetInput {
	repoID := strings.TrimSpace(session.RepoID)
	if repoID == "" {
		repoID = "ide:" + targetidentity.SHA256(repoPath)
	}
	return agenticreview.TargetInput{
		RepoID: repoID, RootID: targetidentity.SHA256(session.SessionID),
		PatchEventID:            targetidentity.SHA256(session.SessionID + "\x00" + req.RequestID),
		CanonicalRemoteIdentity: "ide-workspace:" + targetidentity.SHA256(repoPath),
		PreparedDiffSHA256:      targetidentity.SHA256(patch),
	}
}

func samePathSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, path := range left {
		counts[path]++
	}
	for _, path := range right {
		counts[path]--
		if counts[path] < 0 {
			return false
		}
	}
	return true
}

// generateFixID creates a deterministic turn-scoped fix ID from finding details.
func generateFixID(chatID string, version int, file string, line, index int) string {
	key := fmt.Sprintf("%s:%d:%s:%d:%d", chatID, version, file, line, index)
	hash := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", hash[:8])
}
