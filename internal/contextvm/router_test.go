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
