package idegateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"drydock/internal/contextvm"
	"drydock/internal/db"
	"drydock/internal/payment"
	"drydock/internal/repoconfig"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
)

type mockSigner struct{}

func (m mockSigner) GetPublicKey(context.Context) (nostr.PubKey, error) {
	return nostr.PubKey{}, nil
}

func (m mockSigner) SignEvent(context.Context, *nostr.Event) error {
	return nil
}

type mockPublisher struct {
	events []nostr.Event
}

func (m *mockPublisher) Publish(_ context.Context, _ []string, event nostr.Event) error {
	m.events = append(m.events, event)
	return nil
}

type fakeConfigLoader struct {
	config []byte
	calls  int
}

func (f *fakeConfigLoader) LoadBaseRepoConfig(context.Context, string) ([]byte, error) {
	f.calls++
	return f.config, nil
}

type fakePaymentAuthorizer struct {
	result payment.AuthorizeResult
	calls  int
}

func (f *fakePaymentAuthorizer) AuthorizePatch(context.Context, nostr.Event, string, repoconfig.PaymentsConfig) (payment.AuthorizeResult, error) {
	f.calls++
	return f.result, nil
}

type collectingReviewEnqueuer struct {
	tasks []db.ReviewTask
}

func (e *collectingReviewEnqueuer) EnqueueReview(_ context.Context, task db.ReviewTask, _ string) error {
	e.tasks = append(e.tasks, task)
	return nil
}

func newTestHandler(pub *mockPublisher) *Handler {
	return &Handler{
		cfg:       Config{},
		signer:    mockSigner{},
		publish:   pub,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		ourPubKey: nostr.ZeroPK.Hex(),
		sem:       make(chan struct{}, maxConcurrent),
		sessions:  make(map[string]*activeSession),
		fixTTL:    time.Minute,
	}
}

func seedPatchReviewTarget(t *testing.T, store *db.Store, ownerSK, patchSK nostr.SecretKey) (nostr.Event, string) {
	t.Helper()
	ctx := context.Background()
	repo := nostr.Event{
		Kind: nostr.Kind(30617), CreatedAt: nostr.Now(),
		Tags: nostr.Tags{{"d", "test-repo"}, {"clone", "https://example.com/repo.git"}},
	}
	if err := repo.Sign(ownerSK); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRepositoryAnnouncement(ctx, repo); err != nil {
		t.Fatal(err)
	}
	repoID := nostr.GetPublicKey(ownerSK).Hex() + ":test-repo"
	patch := nostr.Event{
		Kind: nostr.Kind(1617), CreatedAt: nostr.Now(),
		Tags:    nostr.Tags{{"a", "30617:" + repoID}},
		Content: "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -0,0 +1 @@\n+package main\n",
	}
	if err := patch.Sign(patchSK); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertPatchEvent(ctx, patch); err != nil {
		t.Fatal(err)
	}
	return patch, repoID
}

func newPatchRequestHandler(t *testing.T, requester nostr.PubKey, loader RepositoryConfigLoader, authorizer PaymentAuthorizer, enqueuer ReviewEnqueuer) (*Handler, *db.Store) {
	t.Helper()
	store, err := db.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(&mockPublisher{})
	h.store = store
	h.configLoader = loader
	h.paymentAuth = authorizer
	h.reviewEnqueuer = enqueuer
	h.sessions["sess-1"] = &activeSession{PubKey: requester.Hex()}
	return h, store
}

func TestSessionKindIsNIP78(t *testing.T) {
	if KindIDESession != 30078 {
		t.Fatalf("KindIDESession = %d, want 30078", KindIDESession)
	}
}

func TestBuildSessionDTag(t *testing.T) {
	sessionID := "sess-1"
	want := "drydock:ide-session:sess-1"

	if got := BuildSessionDTag(sessionID); got != want {
		t.Fatalf("BuildSessionDTag(%q) = %q, want %q", sessionID, got, want)
	}
}

func TestHandleSessionUsesNIP78DTag(t *testing.T) {
	pub := &mockPublisher{}
	h := newTestHandler(pub)

	event := nostr.Event{
		Kind:    nostr.Kind(KindIDESession),
		Content: `{"workspace_path":"/tmp/repo","editor":"vscode","version":"1.0.0"}`,
		Tags: nostr.Tags{
			{"p", h.ourPubKey},
			{"d", BuildSessionDTag("sess-1")},
			{"type", "ide-session"},
			{"schema", SchemaIDESession},
			{"client", "vscode-drydock/1.0.0"},
		},
	}

	if err := h.handleSession(context.Background(), event, ""); err != nil {
		t.Fatalf("handleSession failed: %v", err)
	}

	h.mu.RLock()
	session, ok := h.sessions["sess-1"]
	h.mu.RUnlock()

	if !ok {
		t.Fatal("session not stored under raw session ID")
	}
	if session.Session.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q, want %q", session.Session.SessionID, "sess-1")
	}
}

func TestPublishReviewResponseUsesContextVMJSONRPC(t *testing.T) {
	pub := &mockPublisher{}
	h := newTestHandler(pub)

	requestID := "1111111111111111111111111111111111111111111111111111111111111111"
	requestPubKey := "2222222222222222222222222222222222222222222222222222222222222222"
	reqEvent := nostr.Event{
		ID:     nostr.MustIDFromHex(requestID),
		PubKey: nostr.MustPubKeyFromHex(requestPubKey),
	}
	resp := ReviewResponse{
		RequestID:    "req-uuid",
		SessionID:    "sess-1",
		Diagnostics:  []Diagnostic{{File: "main.go", Severity: SeverityWarning, Message: "issue", Source: "drydock"}},
		Summary:      "found 1 issue",
		ReviewTimeMs: 1234,
	}

	if err := h.publishReviewResponse(context.Background(), reqEvent, resp, ""); err != nil {
		t.Fatalf("publishReviewResponse failed: %v", err)
	}

	if len(pub.events) != 1 {
		t.Fatalf("published events = %d, want 1", len(pub.events))
	}

	event := pub.events[0]
	if event.Kind != nostr.Kind(KindContextVM) {
		t.Fatalf("event kind = %d, want %d", event.Kind, KindContextVM)
	}
	if !hasTag(event.Tags, "e", requestID) {
		t.Fatalf("missing e tag referencing request %s: %#v", requestID, event.Tags)
	}

	var rpcResp struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      string         `json:"id"`
		Result  ReviewResponse `json:"result"`
		Error   *RPCError      `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(event.Content), &rpcResp); err != nil {
		t.Fatalf("unmarshal JSON-RPC response: %v", err)
	}
	if rpcResp.JSONRPC != "2.0" {
		t.Fatalf("jsonrpc = %q, want 2.0", rpcResp.JSONRPC)
	}
	if rpcResp.ID != resp.RequestID {
		t.Fatalf("id = %q, want %q", rpcResp.ID, resp.RequestID)
	}
	if rpcResp.Error != nil {
		t.Fatalf("error = %#v, want nil", rpcResp.Error)
	}
	if rpcResp.Result.Summary != resp.Summary || len(rpcResp.Result.Diagnostics) != 1 {
		t.Fatalf("unexpected result: %#v", rpcResp.Result)
	}
}

func hasTag(tags nostr.Tags, name, value string) bool {
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == name && tag[1] == value {
			return true
		}
	}
	return false
}

func TestPatchReviewRequestForcedMaintainerReopensStatusSkipped(t *testing.T) {
	ownerSK := nostr.Generate()
	patchSK := nostr.Generate()
	requester := nostr.GetPublicKey(ownerSK)
	loader := &fakeConfigLoader{}
	queue := &collectingReviewEnqueuer{}
	h, store := newPatchRequestHandler(t, requester, loader, nil, queue)
	patch, repoID := seedPatchReviewTarget(t, store, ownerSK, patchSK)

	acquired, err := store.BeginReview(context.Background(), patch.ID.Hex(), repoID, false)
	if err != nil || !acquired {
		t.Fatalf("BeginReview = %v, %v", acquired, err)
	}
	if err := store.MarkReviewFailed(context.Background(), patch.ID.Hex(), repoID, "status_skipped:root status is draft"); err != nil {
		t.Fatal(err)
	}

	resp, rpcErr := h.processPatchReviewRequest(context.Background(), nostr.Event{PubKey: requester}, ReviewRequest{
		SessionID: "sess-1", RequestID: "req-1", PatchEventID: patch.ID.Hex(), Force: true,
	})
	if rpcErr != nil {
		t.Fatalf("forced patch request failed: %s", rpcErr.Message)
	}
	if !resp.Queued || !resp.Forced || len(queue.tasks) != 1 || !queue.tasks[0].Force {
		t.Fatalf("forced response/task mismatch: resp=%+v tasks=%+v", resp, queue.tasks)
	}
}

func TestPatchReviewRequestRejectsUnauthorizedForce(t *testing.T) {
	ownerSK := nostr.Generate()
	patchSK := nostr.Generate()
	requester := nostr.GetPublicKey(nostr.Generate())
	queue := &collectingReviewEnqueuer{}
	h, store := newPatchRequestHandler(t, requester, &fakeConfigLoader{}, nil, queue)
	patch, _ := seedPatchReviewTarget(t, store, ownerSK, patchSK)

	_, rpcErr := h.processPatchReviewRequest(context.Background(), nostr.Event{PubKey: requester}, ReviewRequest{
		SessionID: "sess-1", RequestID: "req-1", PatchEventID: patch.ID.Hex(), Force: true,
	})
	if rpcErr == nil || rpcErr.Code != contextvm.ErrorUnauthorized {
		t.Fatalf("unauthorized force error = %+v", rpcErr)
	}
	if len(queue.tasks) != 0 {
		t.Fatalf("unauthorized force enqueued tasks: %+v", queue.tasks)
	}
}

func TestPatchReviewRequestPaidAccessAuthorizesForce(t *testing.T) {
	ownerSK := nostr.Generate()
	patchSK := nostr.Generate()
	requester := nostr.GetPublicKey(nostr.Generate())
	loader := &fakeConfigLoader{config: []byte("payments:\n  enabled: true\n  price_sats: 100\n")}
	authorizer := &fakePaymentAuthorizer{result: payment.AuthorizeResult{Allowed: true, AccessKind: payment.AccessZap}}
	queue := &collectingReviewEnqueuer{}
	h, store := newPatchRequestHandler(t, requester, loader, authorizer, queue)
	patch, _ := seedPatchReviewTarget(t, store, ownerSK, patchSK)

	_, rpcErr := h.processPatchReviewRequest(context.Background(), nostr.Event{PubKey: requester}, ReviewRequest{
		SessionID: "sess-1", RequestID: "req-1", PatchEventID: patch.ID.Hex(), Force: true,
	})
	if rpcErr != nil {
		t.Fatalf("paid force failed: %s", rpcErr.Message)
	}
	if authorizer.calls != 1 || len(queue.tasks) != 1 || !queue.tasks[0].Force {
		t.Fatalf("paid force did not enqueue forced task: calls=%d tasks=%+v", authorizer.calls, queue.tasks)
	}
}

func TestPatchReviewRequestDeniesUnpaidTargetBeforeEnqueue(t *testing.T) {
	ownerSK := nostr.Generate()
	requester := nostr.GetPublicKey(ownerSK)
	loader := &fakeConfigLoader{config: []byte("payments:\n  enabled: true\n  price_sats: 100\n")}
	authorizer := &fakePaymentAuthorizer{result: payment.AuthorizeResult{Allowed: false, Reason: "no_payment"}}
	queue := &collectingReviewEnqueuer{}
	h, store := newPatchRequestHandler(t, requester, loader, authorizer, queue)
	patch, _ := seedPatchReviewTarget(t, store, ownerSK, nostr.Generate())

	_, rpcErr := h.processPatchReviewRequest(context.Background(), nostr.Event{PubKey: requester}, ReviewRequest{
		SessionID: "sess-1", RequestID: "req-1", PatchEventID: patch.ID.Hex(),
	})
	if rpcErr == nil || rpcErr.Code != contextvm.ErrorUnauthorized {
		t.Fatalf("payment denial error = %+v", rpcErr)
	}
	if authorizer.calls != 1 || len(queue.tasks) != 0 {
		t.Fatalf("payment denial did not stop enqueue: calls=%d tasks=%+v", authorizer.calls, queue.tasks)
	}
}

func TestPatchReviewRequestAppliesRepositoryScopeBeforePayment(t *testing.T) {
	ownerSK := nostr.Generate()
	requester := nostr.GetPublicKey(ownerSK)
	loader := &fakeConfigLoader{config: []byte("payments:\n  enabled: true\n  price_sats: 100\n")}
	authorizer := &fakePaymentAuthorizer{result: payment.AuthorizeResult{Allowed: true, AccessKind: payment.AccessZap}}
	queue := &collectingReviewEnqueuer{}
	h, store := newPatchRequestHandler(t, requester, loader, authorizer, queue)
	h.repositoryScope = scope.NewMatcher([]string{"another-owner:another-repo"}, nil)
	patch, _ := seedPatchReviewTarget(t, store, ownerSK, nostr.Generate())

	_, rpcErr := h.processPatchReviewRequest(context.Background(), nostr.Event{PubKey: requester}, ReviewRequest{
		SessionID: "sess-1", RequestID: "req-1", PatchEventID: patch.ID.Hex(),
	})
	if rpcErr == nil || rpcErr.Code != contextvm.ErrorUnauthorized {
		t.Fatalf("scope denial error = %+v", rpcErr)
	}
	if loader.calls != 0 || authorizer.calls != 0 || len(queue.tasks) != 0 {
		t.Fatalf("scope denial ran later gates: loader=%d payment=%d tasks=%d", loader.calls, authorizer.calls, len(queue.tasks))
	}
}

func TestHandleFixRequestReturnsStoredFix(t *testing.T) {
	pub := &mockPublisher{}
	h := newTestHandler(pub)

	if err := h.storeFix(context.Background(), "fix-1", storedFix{
		SessionID: "sess-1",
		File:      "main.go",
		Diff:      "@@ -1 +1 @@\n-old\n+new",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("storeFix failed: %v", err)
	}

	params := json.RawMessage(`{"session_id":"sess-1","request_id":"req-1","fix_id":"fix-1","file":"main.go"}`)

	reqEvent := nostr.Event{Tags: nostr.Tags{{"p", h.ourPubKey}, {"session", "sess-1"}, {"request", "req-1"}}}
	result, rpcErr := h.HandleApplyFixRequest(context.Background(), contextvm.Request{Event: reqEvent, Msg: contextvm.Message{JSONRPC: "2.0", ID: "req-1", Method: MethodApplyFix, Params: params}})
	if rpcErr != nil {
		t.Fatalf("HandleApplyFixRequest failed: %v", rpcErr.Message)
	}

	resp, ok := result.(FixResponse)
	if !ok {
		t.Fatalf("result = %T, want FixResponse", result)
	}
	if !resp.Success {
		t.Fatal("Success = false, want true")
	}
	if resp.Patch == "" {
		t.Fatal("Patch is empty, want stored patch")
	}
}

func TestHandleFixRequestMissingFix(t *testing.T) {
	pub := &mockPublisher{}
	h := newTestHandler(pub)

	params := json.RawMessage(`{"session_id":"sess-1","request_id":"req-1","fix_id":"missing","file":"main.go"}`)

	reqEvent := nostr.Event{Tags: nostr.Tags{{"p", h.ourPubKey}, {"session", "sess-1"}, {"request", "req-1"}}}
	_, rpcErr := h.HandleApplyFixRequest(context.Background(), contextvm.Request{Event: reqEvent, Msg: contextvm.Message{JSONRPC: "2.0", ID: "req-1", Method: MethodApplyFix, Params: params}})
	if rpcErr == nil {
		t.Fatal("expected JSON-RPC error for missing fix")
	}
	if rpcErr.Message == "" {
		t.Fatal("Error is empty, want descriptive failure")
	}
}

func TestStoreFixReturnsPersistenceError(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	h := newTestHandler(&mockPublisher{})
	h.store = store
	if err := h.storeFix(ctx, "fix-1", storedFix{
		SessionID: "sess-1",
		File:      "main.go",
		Diff:      "diff",
		CreatedAt: time.Now(),
	}); err == nil {
		t.Fatal("storeFix succeeded after durable store failure")
	}
}

func TestCleanupExpiredFixes(t *testing.T) {
	pub := &mockPublisher{}
	h := newTestHandler(pub)
	h.fixTTL = time.Second

	if err := h.storeFix(context.Background(), "expired", storedFix{
		SessionID: "sess-1",
		File:      "main.go",
		Diff:      "diff",
		CreatedAt: time.Now().Add(-2 * time.Second),
	}); err != nil {
		t.Fatalf("storeFix failed: %v", err)
	}

	h.cleanupExpiredFixes(context.Background(), time.Now())

	if _, ok := h.lookupFix(context.Background(), "expired", "sess-1", time.Now()); ok {
		t.Fatal("expired fix should have been removed")
	}
}
