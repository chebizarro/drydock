package idegateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"drydock/internal/contextbuilder"
	"drydock/internal/contextvm"
	"drydock/internal/reviewengine"

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

	llm := &reviewengine.FakeLLMForTest{
		Responses: []string{
			`{"change_type":"feature","risk_areas":["correctness"],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`,
			`{"summary":"Found one issue.","findings":[{"severity":"high","category":"correctness","file":"main.go","line":2,"evidence":"unused branch","explanation":"Prefer explicit return handling.","suggestion":"apply patch","suggested_diff":"@@ -2,1 +2,1 @@\n-return 0\n+return 1","confidence":0.95}],"needs_more_context":[]}`,
		},
	}

	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "planner"},
		Coder32B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder32b"},
		LLM70B:   reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "llm70b"},
		Coder14B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder14b"},
	}, llm, logger)

	pub := &integRelayPublisher{}
	handler := New(
		Config{DefaultRelays: []string{"wss://relay.test"}},
		nil,
		contextbuilder.NewDefault(),
		engine,
		integSigner{sk: nostr.Generate()},
		pub,
		logger,
	)

	sessionEvent := nostr.Event{
		Kind:    nostr.Kind(KindIDESession),
		Content: `{"workspace_path":"/tmp/repo","editor":"vscode","version":"1.0.0"}`,
		Tags: nostr.Tags{
			{"d", BuildSessionDTag("sess-1")},
			{"type", "ide-session"},
			{"schema", SchemaIDESession},
			{"client", "vscode-drydock/1.0.0"},
		},
	}
	if err := handler.HandleEvent(ctx, sessionEvent, "wss://relay.test"); err != nil {
		t.Fatalf("handle session: %v", err)
	}

	reviewParams := json.RawMessage(`{
		"session_id":"sess-1",
		"request_id":"req-1",
		"diff":"diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1,2 @@\n package main\n+func x() int { return 0 }\n",
		"changed_files":["main.go"]
	}`)
	reviewResult, rpcErr := handler.HandleReviewRequest(ctx, contextvm.Request{
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

	fixParams := json.RawMessage(`{
		"session_id":"sess-1",
		"request_id":"fix-req-1",
		"fix_id":"` + diag.FixID + `",
		"file":"main.go"
	}`)
	fixResult, rpcErr := handler.HandleApplyFixRequest(ctx, contextvm.Request{
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
