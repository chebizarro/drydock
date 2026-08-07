package contextvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Handler processes a ContextVM JSON-RPC request and returns a JSON-serializable result.
type Handler func(ctx context.Context, req Request) (any, *Error)

// NotificationHandler processes a ContextVM JSON-RPC notification. Notifications
// never produce a JSON-RPC response; returned errors are reserved for transient
// failures that should leave ingest incomplete for relay redelivery.
type NotificationHandler func(ctx context.Context, req Request) error

// Router dispatches JSON-RPC requests and notifications to distinct handlers.
type Router struct {
	mu            sync.RWMutex
	handlers      map[string]Handler
	notifications map[string]NotificationHandler
}

func NewRouter() *Router {
	return &Router{
		handlers:      make(map[string]Handler),
		notifications: make(map[string]NotificationHandler),
	}
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
	if _, exists := r.handlers[method]; exists {
		return fmt.Errorf("contextvm method already registered: %s", method)
	}
	if _, exists := r.notifications[method]; exists {
		return fmt.Errorf("contextvm method already registered as notification: %s", method)
	}
	r.handlers[method] = handler
	return nil
}

// RegisterNotification registers a notification-only method.
func (r *Router) RegisterNotification(method string, handler NotificationHandler) error {
	if method == "" {
		return errors.New("contextvm method is required")
	}
	if handler == nil {
		return errors.New("contextvm notification handler is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.notifications[method]; exists {
		return fmt.Errorf("contextvm notification already registered: %s", method)
	}
	if _, exists := r.handlers[method]; exists {
		return fmt.Errorf("contextvm method already registered as request: %s", method)
	}
	r.notifications[method] = handler
	return nil
}

func (r *Router) Methods() []string {
	r.mu.RLock()
	methods := make([]string, 0, len(r.handlers)+len(r.notifications))
	for method := range r.handlers {
		methods = append(methods, method)
	}
	for method := range r.notifications {
		methods = append(methods, method)
	}
	r.mu.RUnlock()
	sort.Strings(methods)
	return methods
}

// Handle dispatches a request or notification. A zero Message means that no
// response may be published (the input was a notification).
func (r *Router) Handle(ctx context.Context, req Request) (Message, error) {
	if req.Msg.ID == "" {
		return r.handleNotification(ctx, req)
	}
	if req.Msg.JSONRPC != jsonRPCVersion || req.Msg.Method == "" {
		return newError(req.Msg.ID, ErrorInvalidRequest, "invalid JSON-RPC request"), nil
	}

	r.mu.RLock()
	handler := r.handlers[req.Msg.Method]
	_, notificationOnly := r.notifications[req.Msg.Method]
	r.mu.RUnlock()
	if handler == nil {
		if notificationOnly {
			return newError(req.Msg.ID, ErrorInvalidRequest, "method only accepts notifications"), nil
		}
		return newError(req.Msg.ID, ErrorMethodNotFound, fmt.Sprintf("method not found: %s", req.Msg.Method)), nil
	}

	result, rpcErr := handler(ctx, req)
	if rpcErr != nil {
		return Message{JSONRPC: jsonRPCVersion, ID: req.Msg.ID, Error: rpcErr}, nil
	}
	resp, err := newResult(req.Msg.ID, result)
	if err != nil {
		return newError(req.Msg.ID, ErrorInternal, "failed to marshal handler result"), err
	}
	return resp, nil
}

func (r *Router) handleNotification(ctx context.Context, req Request) (Message, error) {
	// JSON-RPC notifications never receive responses, including malformed or
	// unknown notifications.
	if req.Msg.JSONRPC != jsonRPCVersion || req.Msg.Method == "" {
		return Message{}, nil
	}
	r.mu.RLock()
	handler := r.notifications[req.Msg.Method]
	r.mu.RUnlock()
	if handler == nil {
		return Message{}, nil
	}
	return Message{}, handler(ctx, req)
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
