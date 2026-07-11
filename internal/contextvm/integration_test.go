package contextvm

import (
	"context"
	"encoding/json"
	"testing"

	"fiatjaf.com/nostr"
)

func TestIntegrationRequestResponseCorrelationViaJSONRPCID(t *testing.T) {
	ctx := context.Background()
	pool := &fakePool{}
	sender := newTestSigner(11)
	recipient, _ := newTestSigner(12).GetPublicKey(ctx)
	transport := NewTransport(pool, sender, []string{"wss://read.test"}, []string{"wss://write.test"}, nil)

	rpcID := "jsonrpc-correlation-1"
	reqEventID, err := transport.SendWithID(ctx, rpcID, "review/request", map[string]string{"repo": "drydock"}, recipient)
	if err != nil {
		t.Fatalf("SendWithID: %v", err)
	}
	if len(pool.published) != 1 {
		t.Fatalf("published requests = %d, want 1", len(pool.published))
	}

	var reqMsg Message
	if err := json.Unmarshal([]byte(pool.published[0].Content), &reqMsg); err != nil {
		t.Fatalf("request content json: %v", err)
	}
	if reqMsg.ID != rpcID || reqMsg.Method != "review/request" {
		t.Fatalf("request message = %+v, want id %q method review/request", reqMsg, rpcID)
	}

	if err := transport.SendResponseToEvent(ctx, reqEventID, rpcID, map[string]string{"status": "ok"}, nil, recipient); err != nil {
		t.Fatalf("SendResponseToEvent: %v", err)
	}
	if len(pool.published) != 2 {
		t.Fatalf("published events = %d, want 2", len(pool.published))
	}

	respEvent := pool.published[1]
	if respEvent.Kind != KindContextVM {
		t.Fatalf("response kind = %d, want %d", respEvent.Kind, KindContextVM)
	}
	if !respEvent.Tags.ContainsAny("e", []string{reqEventID}) {
		t.Fatalf("response missing request e tag %q: %+v", reqEventID, respEvent.Tags)
	}
	var respMsg Message
	if err := json.Unmarshal([]byte(respEvent.Content), &respMsg); err != nil {
		t.Fatalf("response content json: %v", err)
	}
	if respMsg.ID != rpcID {
		t.Fatalf("response id = %q, want %q", respMsg.ID, rpcID)
	}
	if respMsg.Error != nil || len(respMsg.Result) == 0 {
		t.Fatalf("response should contain result only: %+v", respMsg)
	}
}

func TestIntegrationMethodRoutingToCorrectHandlers(t *testing.T) {
	router := NewRouter()
	called := make(map[string]int)

	if err := router.Register("review/request", func(ctx context.Context, req Request) (any, *Error) {
		called["review/request"]++
		return map[string]string{"handler": "review"}, nil
	}); err != nil {
		t.Fatalf("register review: %v", err)
	}
	if err := router.Register("marketplace/assign", func(ctx context.Context, req Request) (any, *Error) {
		called["marketplace/assign"]++
		return map[string]string{"handler": "marketplace"}, nil
	}); err != nil {
		t.Fatalf("register marketplace: %v", err)
	}

	resp, err := router.Handle(context.Background(), Request{Msg: Message{JSONRPC: jsonRPCVersion, ID: "route-1", Method: "marketplace/assign"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if called["marketplace/assign"] != 1 || called["review/request"] != 0 {
		t.Fatalf("handler calls = %+v, want only marketplace/assign", called)
	}
	var result map[string]string
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("result json: %v", err)
	}
	if result["handler"] != "marketplace" {
		t.Fatalf("result = %+v, want marketplace handler", result)
	}
}

func TestIntegrationErrorResponseFormatting(t *testing.T) {
	router := NewRouter()
	if err := router.Register("review/apply-fix", func(ctx context.Context, req Request) (any, *Error) {
		return nil, &Error{Code: ErrorInvalidParams, Message: "missing fix_id", Data: json.RawMessage(`{"field":"fix_id"}`)}
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	resp, err := router.Handle(context.Background(), Request{
		Event: nostr.Event{},
		Msg:   Message{JSONRPC: jsonRPCVersion, ID: "err-1", Method: "review/apply-fix"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var decoded struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Error   *Error `json:"error"`
		Result  any    `json:"result,omitempty"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode formatted response: %v", err)
	}
	if decoded.JSONRPC != "2.0" || decoded.ID != "err-1" {
		t.Fatalf("response envelope = %+v", decoded)
	}
	if decoded.Error == nil || decoded.Error.Code != ErrorInvalidParams || decoded.Error.Message != "missing fix_id" {
		t.Fatalf("error object = %+v, want invalid params/missing fix_id", decoded.Error)
	}
	if len(decoded.Error.Data) == 0 || string(decoded.Error.Data) != `{"field":"fix_id"}` {
		t.Fatalf("error data = %s", decoded.Error.Data)
	}
}
