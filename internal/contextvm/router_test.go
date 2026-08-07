package contextvm

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRouterDispatchesRegisteredHandler(t *testing.T) {
	r := NewRouter()
	if err := r.Register("echo", func(ctx context.Context, req Request) (any, *Error) {
		params, rpcErr := ParamsAs[map[string]string](req)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return map[string]string{"echo": params["value"]}, nil
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	params := json.RawMessage(`{"value":"hello"}`)
	resp, err := r.Handle(context.Background(), Request{Msg: Message{JSONRPC: jsonRPCVersion, ID: "evt-1", Method: "echo", Params: params}})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var result map[string]string
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("result json: %v", err)
	}
	if result["echo"] != "hello" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRouterNotificationDispatchesWithoutResponse(t *testing.T) {
	r := NewRouter()
	calls := 0
	if err := r.RegisterNotification("progress", func(context.Context, Request) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("register notification: %v", err)
	}

	resp, err := r.Handle(context.Background(), Request{Msg: Message{JSONRPC: jsonRPCVersion, Method: "progress"}})
	if err != nil {
		t.Fatalf("handle notification: %v", err)
	}
	if resp.JSONRPC != "" || resp.ID != "" || resp.Method != "" || len(resp.Result) != 0 || resp.Error != nil {
		t.Fatalf("notification produced response: %+v", resp)
	}
	if calls != 1 {
		t.Fatalf("notification calls = %d, want 1", calls)
	}

	resp, err = r.Handle(context.Background(), Request{Msg: Message{JSONRPC: jsonRPCVersion, Method: "missing"}})
	if err != nil || resp.JSONRPC != "" || resp.ID != "" || resp.Method != "" || len(resp.Result) != 0 || resp.Error != nil {
		t.Fatalf("unknown notification response=%+v err=%v", resp, err)
	}
}

func TestRouterNotificationNeverDispatchesRequestHandler(t *testing.T) {
	r := NewRouter()
	calls := 0
	if err := r.Register("request-only", func(context.Context, Request) (any, *Error) {
		calls++
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := r.Handle(context.Background(), Request{Msg: Message{JSONRPC: jsonRPCVersion, Method: "request-only"}})
	if err != nil || resp.JSONRPC != "" || resp.ID != "" || resp.Method != "" || len(resp.Result) != 0 || resp.Error != nil {
		t.Fatalf("request-only notification response=%+v err=%v", resp, err)
	}
	if calls != 0 {
		t.Fatalf("request handler called %d times for notification", calls)
	}
}

func TestRouterRejectsDuplicateRegistrationAcrossKinds(t *testing.T) {
	r := NewRouter()
	if err := r.Register("same", func(context.Context, Request) (any, *Error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("same", func(context.Context, Request) (any, *Error) { return nil, nil }); err == nil {
		t.Fatal("duplicate request registration succeeded")
	}
	if err := r.RegisterNotification("same", func(context.Context, Request) error { return nil }); err == nil {
		t.Fatal("request/notification duplicate registration succeeded")
	}
}

func TestRouterMethodNotFound(t *testing.T) {
	r := NewRouter()
	resp, err := r.Handle(context.Background(), Request{Msg: Message{JSONRPC: jsonRPCVersion, ID: "evt-1", Method: "missing"}})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != ErrorMethodNotFound {
		t.Fatalf("expected method not found, got %+v", resp.Error)
	}
}

func TestRouterInvalidRequest(t *testing.T) {
	r := NewRouter()
	resp, err := r.Handle(context.Background(), Request{Msg: Message{JSONRPC: jsonRPCVersion, ID: "evt-1"}})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != ErrorInvalidRequest {
		t.Fatalf("expected invalid request, got %+v", resp.Error)
	}
}
