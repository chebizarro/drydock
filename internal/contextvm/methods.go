package contextvm

import (
	"encoding/json"
	"fmt"
)

const JSONRPCVersion = "2.0"

// Message is the JSON-RPC 2.0 envelope carried by ContextVM kind-25910 events.
type Message struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ParseMessage decodes a ContextVM JSON-RPC envelope from event content.
func ParseMessage(content string) (Message, error) {
	var msg Message
	if err := json.Unmarshal([]byte(content), &msg); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// MarshalRequest wraps params in a JSON-RPC request envelope.
func MarshalRequest(id, method string, params any) (string, error) {
	if id == "" {
		return "", fmt.Errorf("contextvm request id is required")
	}
	if method == "" {
		return "", fmt.Errorf("contextvm method is required")
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("marshal params: %w", err)
	}
	msg := Message{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Method:  method,
		Params:  paramsJSON,
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("marshal contextvm request: %w", err)
	}
	return string(body), nil
}

// MarshalResult wraps result in a JSON-RPC response envelope.
func MarshalResult(id string, result any) (string, error) {
	if id == "" {
		return "", fmt.Errorf("contextvm response id is required")
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	msg := Message{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Result:  resultJSON,
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("marshal contextvm response: %w", err)
	}
	return string(body), nil
}

// MarshalError wraps an error in a JSON-RPC response envelope.
func MarshalError(id string, code int, message string, data any) (string, error) {
	if id == "" {
		return "", fmt.Errorf("contextvm response id is required")
	}
	msg := Message{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Error: &Error{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("marshal contextvm error response: %w", err)
	}
	return string(body), nil
}
