package idegateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drydock/internal/agenticreview"
	"drydock/internal/agenttools"
	"drydock/internal/contextbuilder"
	"drydock/internal/contextvm"
	"drydock/internal/db"
	"drydock/internal/reviewengine"
	"drydock/internal/reviewsession"
	"drydock/internal/testutil"
	"drydock/internal/workspacesnapshot"

	"fiatjaf.com/nostr"
)

type integSigner struct {
	sk nostr.SecretKey
}

func (s integSigner) GetPublicKey(_ context.Context) (nostr.PubKey, error) {
	return nostr.GetPublicKey(s.sk), nil
}

func (s integSigner) SignEvent(_ context.Context, evt *nostr.Event) error {
	return evt.Sign(s.sk)
}

type ideTokenCounter struct{}

func (ideTokenCounter) Count(text string) int { return len(text) }

type ideAgenticClient struct {
	results  []reviewengine.CompletionResult
	requests []reviewengine.CompletionRequest
	next     int
}

func (c *ideAgenticClient) ChatCompletion(context.Context, reviewengine.ChatRequest) (reviewengine.ChatResult, error) {
	return reviewengine.ChatResult{Content: `{"change_type":"feature","risk_areas":["correctness"],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`}, nil
}

func (c *ideAgenticClient) Complete(_ context.Context, request reviewengine.CompletionRequest) (reviewengine.CompletionResult, error) {
	c.requests = append(c.requests, request)
	result := c.results[c.next]
	c.next++
	return result, nil
}

func ideToolCall(t *testing.T, id, name string, arguments any) reviewengine.ToolCall {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	return reviewengine.ToolCall{ID: id, Type: "function", Function: reviewengine.ToolCallFunction{Name: name, Arguments: string(encoded)}}
}

func ideCompletion(calls ...reviewengine.ToolCall) reviewengine.CompletionResult {
	return reviewengine.CompletionResult{
		Message: reviewengine.CompletionMessage{Role: reviewengine.MessageRoleAssistant, ToolCalls: calls},
		Usage:   reviewengine.CompletionUsage{TotalTokens: 1},
	}
}

type integRelayPublisher struct {
	events []nostr.Event
}

func (p *integRelayPublisher) Publish(_ context.Context, _ []string, event nostr.Event) error {
	p.events = append(p.events, event)
	return nil
}

func TestIntegrationIDEGatewayNostrSessionReviewAndFixCycle(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte(testutil.AgenticFixtureBaseFiles()["main.go"]), 0o600); err != nil {
		t.Fatal(err)
	}
	llm := &ideAgenticClient{results: []reviewengine.CompletionResult{
		ideCompletion(ideToolCall(t, "finalize", agenttools.ToolSelectionFinalize, map[string]any{})),
		ideCompletion(ideToolCall(t, "evidence-1", agenttools.ToolCodeRead, map[string]any{"path": "main.go", "start_line": 1, "end_line": 2})),
		ideCompletion(ideToolCall(t, "submit-1", agenttools.ToolReviewSubmit, map[string]any{
			"summary": "Found one issue.",
			"findings": []any{map[string]any{
				"priority": "P1", "category": "correctness", "file": "main.go", "line": 2,
				"explanation": "Prefer explicit return handling.", "suggestion": "apply patch",
				"suggested_diff": "@@ -2,1 +2,1 @@\n-return 0\n+return 1", "confidence": 0.95,
				"evidence_tool_call_ids": []string{"evidence-1"},
			}},
			"coverage": map[string]any{"examined_files": []string{"main.go"}, "outcome": "findings", "summary": "reviewed changed file"},
		})),
		ideCompletion(ideToolCall(t, "evidence-2", agenttools.ToolCodeRead, map[string]any{"path": "main.go", "start_line": 1, "end_line": 2})),
		ideCompletion(ideToolCall(t, "submit-2", agenttools.ToolReviewSubmit, map[string]any{
			"summary": "The issue remains.",
			"findings": []any{map[string]any{
				"priority": "P1", "category": "correctness", "file": "main.go", "line": 2,
				"explanation": "Prefer explicit return handling.", "suggestion": "apply patch",
				"suggested_diff": "@@ -2,1 +2,1 @@\n-return 0\n+return 1", "confidence": 0.95,
				"evidence_tool_call_ids": []string{"evidence-2"},
			}},
			"coverage": map[string]any{"examined_files": []string{"main.go"}, "outcome": "findings", "summary": "reviewed frozen file"},
		})),
	}}

	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "planner"},
		Coder32B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder32b"},
		LLM70B:   reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "llm70b"},
		Coder14B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder14b"},
	}, llm, logger)

	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "ide.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sessionStore, err := reviewsession.NewSQLiteStore(database.DB(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := workspacesnapshot.NewManager(workspacesnapshot.Config{StorageRoot: t.TempDir(), LeaseTTL: time.Hour, SessionLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	registry := agenttools.NewRegistry()
	discovery, err := agenticreview.NewDiscovery(agenticreview.DiscoveryConfig{Client: llm, Registry: registry, Counter: ideTokenCounter{}, TokenBudget: 100_000})
	if err != nil {
		t.Fatal(err)
	}
	agenticSvc, err := agenticreview.NewService(agenticreview.ServiceConfig{
		Snapshots: manager, Sessions: sessionStore, Discovery: discovery, Engine: engine,
		Client: llm, Registry: registry, Counter: ideTokenCounter{}, HistoryTokens: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}

	pub := &integRelayPublisher{}
	gatewaySigner := integSigner{sk: nostr.Generate()}
	clientSK := nostr.Generate()
	handler := New(
		Config{DefaultRelays: []string{"wss://relay.test"}},
		database,
		contextbuilder.NewDefault(),
		engine,
		gatewaySigner,
		pub,
		logger,
		WithAgenticReviewService(agenticSvc),
	)

	sessionEvent := nostr.Event{
		Kind:    nostr.Kind(KindIDESession),
		Content: fmt.Sprintf(`{"workspace_path":%q,"editor":"vscode","version":"1.0.0"}`, workspace),
		Tags: nostr.Tags{
			{"p", handler.ourPubKey},
			{"d", BuildSessionDTag("sess-1")},
			{"type", "ide-session"},
			{"schema", SchemaIDESession},
			{"client", "vscode-drydock/1.0.0"},
		},
	}
	if err := sessionEvent.Sign(clientSK); err != nil {
		t.Fatalf("sign session event: %v", err)
	}
	if err := handler.HandleEvent(ctx, sessionEvent, "wss://relay.test"); err != nil {
		t.Fatalf("handle session: %v", err)
	}

	reviewParams, err := json.Marshal(ReviewRequest{
		SessionID: "sess-1", RequestID: "req-1", Diff: testutil.AgenticFixturePatch(),
		ChangedFiles: []string{"client-supplied-wrong.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	clientPub := nostr.GetPublicKey(clientSK)
	reviewEvent := nostr.Event{PubKey: clientPub, Tags: nostr.Tags{{"p", handler.ourPubKey}, {"session", "sess-1"}, {"request", "req-1"}}}
	reviewResult, rpcErr := handler.HandleReviewRequest(ctx, contextvm.Request{
		Event: reviewEvent,
		Relay: "wss://relay.test",
		Msg: contextvm.Message{
			JSONRPC: "2.0",
			ID:      "req-1",
			Method:  MethodReviewRequest,
			Params:  reviewParams,
		},
	})
	if rpcErr != nil {
		t.Fatalf("handle review request: %v", rpcErr.Message)
	}

	reviewResp, ok := reviewResult.(ReviewResponse)
	if !ok {
		t.Fatalf("review result = %T, want ReviewResponse", reviewResult)
	}
	if reviewResp.ChatID == "" || reviewResp.ExpectedVersion == nil || *reviewResp.ExpectedVersion != 0 {
		t.Fatalf("session metadata missing: %+v", reviewResp)
	}
	if len(reviewResp.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %d, want 1", len(reviewResp.Diagnostics))
	}

	diag := reviewResp.Diagnostics[0]
	if !diag.HasFix || diag.FixID == "" {
		t.Fatalf("diagnostic fix metadata missing: %+v", diag)
	}
	if diag.SuggestedFix == "" {
		t.Fatal("expected suggested_fix in review response")
	}

	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("LIVE WORKSPACE MUTATION\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	followVersion := *reviewResp.ExpectedVersion
	followParams, err := json.Marshal(ReviewRequest{
		SessionID: "sess-1", RequestID: "req-2", ChatID: reviewResp.ChatID,
		ExpectedVersion: &followVersion, Message: "Check the frozen workspace again.",
	})
	if err != nil {
		t.Fatal(err)
	}
	followEvent := nostr.Event{PubKey: clientPub, Tags: nostr.Tags{{"p", handler.ourPubKey}, {"session", "sess-1"}, {"request", "req-2"}}}
	followResult, followErr := handler.HandleReviewRequest(ctx, contextvm.Request{
		Event: followEvent, Msg: contextvm.Message{JSONRPC: "2.0", ID: "req-2", Method: MethodReviewRequest, Params: followParams},
	})
	if followErr != nil {
		t.Fatalf("follow-up failed: %v", followErr.Message)
	}
	followResp := followResult.(ReviewResponse)
	if followResp.ChatID != reviewResp.ChatID || followResp.ExpectedVersion == nil || *followResp.ExpectedVersion != 1 {
		t.Fatalf("follow-up session metadata = %+v", followResp)
	}
	if len(followResp.Diagnostics) != 1 || followResp.Diagnostics[0].FixID == diag.FixID {
		t.Fatalf("turn-scoped fix ID missing: initial=%q follow=%+v", diag.FixID, followResp.Diagnostics)
	}
	var frozenRead agenttools.Result
	for _, message := range llm.requests[len(llm.requests)-1].Messages {
		if message.Role == reviewengine.MessageRoleTool && message.ToolCallID == "evidence-2" {
			if err := json.Unmarshal([]byte(message.Content), &frozenRead); err != nil {
				t.Fatal(err)
			}
		}
	}
	if strings.Contains(frozenRead.Content, "LIVE WORKSPACE MUTATION") ||
		frozenRead.Content != testutil.AgenticFixtureBaseFiles()["main.go"] {
		t.Fatalf("continuation frozen code.read = %q", frozenRead.Content)
	}

	completionsBeforeReplay := llm.next
	replayed, replayErr := handler.HandleReviewRequest(ctx, contextvm.Request{
		Event: followEvent, Msg: contextvm.Message{JSONRPC: "2.0", ID: "req-2", Method: MethodReviewRequest, Params: followParams},
	})
	if replayErr != nil {
		t.Fatalf("duplicate continuation failed: %v", replayErr.Message)
	}
	replayResp := replayed.(ReviewResponse)
	if llm.next != completionsBeforeReplay || replayResp.ExpectedVersion == nil ||
		*replayResp.ExpectedVersion != 1 || replayResp.Diagnostics[0].FixID != followResp.Diagnostics[0].FixID {
		t.Fatalf("duplicate continuation was not replayed: response=%+v calls=%d", replayResp, llm.next)
	}

	staleVersion := int64(0)
	staleParams, err := json.Marshal(ReviewRequest{
		SessionID: "sess-1", RequestID: "req-stale", ChatID: reviewResp.ChatID,
		ExpectedVersion: &staleVersion, Message: "stale follow-up",
	})
	if err != nil {
		t.Fatal(err)
	}
	staleEvent := nostr.Event{PubKey: clientPub, Tags: nostr.Tags{{"p", handler.ourPubKey}, {"session", "sess-1"}, {"request", "req-stale"}}}
	_, staleErr := handler.HandleReviewRequest(ctx, contextvm.Request{
		Event: staleEvent, Msg: contextvm.Message{JSONRPC: "2.0", ID: "req-stale", Method: MethodReviewRequest, Params: staleParams},
	})
	if staleErr == nil || staleErr.Code != contextvm.ErrorConflict || llm.next != completionsBeforeReplay {
		t.Fatalf("stale continuation result: error=%+v calls=%d", staleErr, llm.next)
	}

	fixParams := json.RawMessage(`{
		"session_id":"sess-1",
		"request_id":"fix-req-1",
		"fix_id":"` + diag.FixID + `",
		"file":"main.go"
	}`)
	fixEvent := nostr.Event{PubKey: clientPub, Tags: nostr.Tags{{"p", handler.ourPubKey}, {"session", "sess-1"}, {"request", "fix-req-1"}}}
	fixResult, rpcErr := handler.HandleApplyFixRequest(ctx, contextvm.Request{
		Event: fixEvent,
		Relay: "wss://relay.test",
		Msg: contextvm.Message{
			JSONRPC: "2.0",
			ID:      "fix-req-1",
			Method:  MethodApplyFix,
			Params:  fixParams,
		},
	})
	if rpcErr != nil {
		t.Fatalf("handle fix request: %v", rpcErr.Message)
	}

	fixResp, ok := fixResult.(FixResponse)
	if !ok {
		t.Fatalf("fix result = %T, want FixResponse", fixResult)
	}
	if !fixResp.Success {
		t.Fatal("fix response not successful")
	}
	if fixResp.Patch == "" {
		t.Fatal("expected patch in fix response")
	}
	if fixResp.Patch != diag.SuggestedFix {
		t.Fatalf("fix patch mismatch: got %q want %q", fixResp.Patch, diag.SuggestedFix)
	}
}
