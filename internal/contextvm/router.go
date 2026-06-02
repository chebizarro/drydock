package contextvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Handler processes a ContextVM JSON-RPC request and returns a JSON-serializable result.
type Handler func(ctx context.Context, req Request) (any, *Error)

// Router dispatches JSON-RPC methods to registered handlers.
type Router struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

func NewRouter() *Router {
	return &Router{handlers: make(map[string]Handler)}
}

func (r *Router) Register(method string, handler Handler) error {
	if method == "" {
		return errors.New("contextvm method is required")
	}
	if handler == nil {
		return errors.New("contextvm handler is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[method] = handler
	return nil
}

func (r *Router) Handle(ctx context.Context, req Request) (Message, error) {
	id := req.Msg.ID
	if id == "" && req.Event.ID.Hex() != "0000000000000000000000000000000000000000000000000000000000000000" {
		id = req.Event.ID.Hex()
	}
	if req.Msg.JSONRPC != jsonRPCVersion || id == "" || req.Msg.Method == "" {
		return newError(id, ErrorInvalidRequest, "invalid JSON-RPC request"), nil
	}

	r.mu.RLock()
	handler := r.handlers[req.Msg.Method]
	r.mu.RUnlock()
	if handler == nil {
		return newError(id, ErrorMethodNotFound, fmt.Sprintf("method not found: %s", req.Msg.Method)), nil
	}

	result, rpcErr := handler(ctx, req)
	if rpcErr != nil {
		return Message{JSONRPC: jsonRPCVersion, ID: id, Error: rpcErr}, nil
	}
	resp, err := newResult(id, result)
	if err != nil {
		return newError(id, ErrorInternal, "failed to marshal handler result"), err
	}
	return resp, nil
}

func ParamsAs[T any](req Request) (T, *Error) {
	var out T
	if len(req.Msg.Params) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(req.Msg.Params, &out); err != nil {
		return out, &Error{Code: ErrorInvalidParams, Message: err.Error()}
	}
	return out, nil
}
