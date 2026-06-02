package idegateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"drydock/internal/contextvm"

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

func newTestHandler(pub *mockPublisher) *Handler {
	return &Handler{
		cfg:      Config{},
		signer:   mockSigner{},
		publish:  pub,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		sem:      make(chan struct{}, maxConcurrent),
		sessions: make(map[string]*activeSession),
		fixTTL:   time.Minute,
	}
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

func TestHandleFixRequestReturnsStoredFix(t *testing.T) {
	pub := &mockPublisher{}
	h := newTestHandler(pub)

	h.storeFix("fix-1", storedFix{
		SessionID: "sess-1",
		File:      "main.go",
		Diff:      "@@ -1 +1 @@\n-old\n+new",
		CreatedAt: time.Now(),
	})

	params := json.RawMessage(`{"session_id":"sess-1","request_id":"req-1","fix_id":"fix-1","file":"main.go"}`)

	result, rpcErr := h.HandleApplyFixRequest(context.Background(), contextvm.Request{Msg: contextvm.Message{JSONRPC: "2.0", ID: "req-1", Method: MethodApplyFix, Params: params}})
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

	_, rpcErr := h.HandleApplyFixRequest(context.Background(), contextvm.Request{Msg: contextvm.Message{JSONRPC: "2.0", ID: "req-1", Method: MethodApplyFix, Params: params}})
	if rpcErr == nil {
		t.Fatal("expected JSON-RPC error for missing fix")
	}
	if rpcErr.Message == "" {
		t.Fatal("Error is empty, want descriptive failure")
	}
}

func TestCleanupExpiredFixes(t *testing.T) {
	pub := &mockPublisher{}
	h := newTestHandler(pub)
	h.fixTTL = time.Second

	h.storeFix("expired", storedFix{
		SessionID: "sess-1",
		File:      "main.go",
		Diff:      "diff",
		CreatedAt: time.Now().Add(-2 * time.Second),
	})

	h.cleanupExpiredFixes(time.Now())

	if _, ok := h.lookupFix("expired", time.Now()); ok {
		t.Fatal("expired fix should have been removed")
	}
}
