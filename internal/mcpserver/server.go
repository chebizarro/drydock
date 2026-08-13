// Package mcpserver adapts the canonical agent tool registry to the official
// Model Context Protocol Go SDK. MCP types are intentionally contained here.
package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	"drydock/internal/agenttools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	serverName    = "drydock"
	serverVersion = "1"
)

var ErrUnscoped = errors.New("mcp server: frozen scope is required")

var resultSchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"content":{"type":"string"},
		"structured":{},
		"is_error":{"type":"boolean"},
		"truncated":{"type":"boolean"},
		"replay":{"type":"boolean"}
	},
	"required":["content"],
	"additionalProperties":false
}`)

// BoundServer is an MCP server whose role and snapshot scope were fixed before
// a transport was connected. It never accepts roots, roles, or capabilities
// from MCP tool arguments.
type BoundServer struct {
	registry *agenttools.Registry
	scope    agenttools.Scope
	server   *mcp.Server
	prefix   string
	nextID   atomic.Uint64
}

// NewBoundServer freezes an already server-resolved scope into an MCP adapter.
func NewBoundServer(registry *agenttools.Registry, scope *agenttools.Scope) (*BoundServer, error) {
	if registry == nil {
		return nil, fmt.Errorf("mcp server: registry is required")
	}
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, fmt.Errorf("mcp server: generate invocation namespace: %w", err)
	}
	bound := &BoundServer{
		registry: registry,
		scope:    *scope,
		prefix:   hex.EncodeToString(random[:]),
	}
	bound.server = mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)
	for _, definition := range registry.ListForScope(&bound.scope) {
		definition := definition
		readOnly := definition.Capability == agenttools.CapabilityRead ||
			definition.Capability == agenttools.CapabilitySelectionRead ||
			definition.Capability == agenttools.CapabilitySnapshotWide
		bound.server.AddTool(&mcp.Tool{
			Name:         definition.Name,
			Description:  definition.Description,
			InputSchema:  append(json.RawMessage(nil), definition.InputSchema...),
			OutputSchema: append(json.RawMessage(nil), resultSchema...),
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:   readOnly,
				IdempotentHint: true,
			},
		}, bound.handle(definition.Name))
	}
	return bound, nil
}

func validateScope(scope *agenttools.Scope) error {
	if scope == nil || scope.ID == "" || scope.Snapshot == nil || !scope.Role.Valid() {
		return ErrUnscoped
	}
	return nil
}

func (s *BoundServer) handle(name string) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		arguments := json.RawMessage(`{}`)
		if request != nil && request.Params != nil && len(request.Params.Arguments) != 0 {
			arguments = append(json.RawMessage(nil), request.Params.Arguments...)
		}
		callID := fmt.Sprintf("mcp-%s-%d", s.prefix, s.nextID.Add(1))
		result, err := s.registry.Dispatch(ctx, agenttools.Invocation{
			ToolCallID: callID,
			Name:       name,
			Arguments:  arguments,
			Scope:      &s.scope,
		})
		if err != nil {
			result = agenttools.Result{Content: err.Error(), IsError: true}
		}
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return nil, fmt.Errorf("mcp server: encode tool result: %w", marshalErr)
		}
		var structured any
		if err := json.Unmarshal(encoded, &structured); err != nil {
			return nil, fmt.Errorf("mcp server: prepare structured tool result: %w", err)
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
			StructuredContent: structured,
			IsError:           result.IsError,
		}, nil
	}
}

// RunStdio serves the bound tool set until the stdio connection closes.
func (s *BoundServer) RunStdio(ctx context.Context) error {
	if s == nil || s.server == nil {
		return ErrUnscoped
	}
	return s.server.Run(ctx, &mcp.StdioTransport{})
}

// SDKServer exposes the transport-ready SDK server within this adapter package.
func (s *BoundServer) SDKServer() *mcp.Server {
	if s == nil {
		return nil
	}
	return s.server
}
