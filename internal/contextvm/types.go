package contextvm

import (
	"encoding/json"

	"drydock/internal/eventkind"

	"fiatjaf.com/nostr"
)

const (
	KindContextVM nostr.Kind = eventkind.ContextVM // Ephemeral ContextVM messages
	KindGiftWrap  nostr.Kind = eventkind.GiftWrap  // NIP-59 encrypted wrapper
)

const jsonRPCVersion = "2.0"

// Message is a JSON-RPC 2.0 message carried in a ContextVM event.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error mirrors the shared contextvm error shape while retaining drydock's
// json.RawMessage Data compatibility.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

const (
	ErrorParseError     = -32700
	ErrorInvalidRequest = -32600
	ErrorMethodNotFound = -32601
	ErrorInvalidParams  = -32602
	ErrorInternal       = -32603

	// Application errors used by gated asynchronous review requests.
	ErrorUnauthorized = -32001
	ErrorNotFound     = -32002
	ErrorConflict     = -32003
)

// Request is an inbound ContextVM JSON-RPC request with its source event.
type Request struct {
	Event  nostr.Event
	Relay  string
	Sender nostr.PubKey
	Msg    Message
}

// Response is an outbound ContextVM JSON-RPC response.
type Response struct {
	ID     string          `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

func newRequest(id, method string, params any) (Message, error) {
	raw, err := marshalRaw(params)
	if err != nil {
		return Message{}, err
	}
	return Message{JSONRPC: jsonRPCVersion, ID: id, Method: method, Params: raw}, nil
}

func newResult(id string, result any) (Message, error) {
	raw, err := marshalRaw(result)
	if err != nil {
		return Message{}, err
	}
	return Message{JSONRPC: jsonRPCVersion, ID: id, Result: raw}, nil
}

func newError(id string, code int, message string) Message {
	return Message{JSONRPC: jsonRPCVersion, ID: id, Error: &Error{Code: code, Message: message}}
}

func marshalRaw(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}
