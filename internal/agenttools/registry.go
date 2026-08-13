package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"drydock/internal/workspacesnapshot"
)

type Capability string

const (
	CapabilityRead              Capability = "repository.read"
	CapabilitySelectionRead     Capability = "selection.read"
	CapabilitySelectionMutate   Capability = "selection.mutate"
	CapabilitySelectionFinalize Capability = "selection.finalize"
	CapabilityReviewSubmit      Capability = "review.submit"
	CapabilitySnapshotWide      Capability = "snapshot.wide"
)

type Role string

const (
	RoleContextDiscovery Role = "context_discovery"
	RoleCodeReviewer     Role = "code_reviewer"
	RoleSecurityAuditor  Role = "security_auditor"
	RoleExternalReadonly Role = "external_readonly"
)

type Definition struct {
	Name           string
	Description    string
	InputSchema    json.RawMessage
	Capability     Capability
	MaxResultBytes int
}

type Handler func(context.Context, Invocation) (Result, error)

type Invocation struct {
	ToolCallID string
	Name       string
	Arguments  json.RawMessage
	Scope      *Scope
}

type Result struct {
	Content    string          `json:"content"`
	Structured json.RawMessage `json:"structured,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	Truncated  bool            `json:"truncated,omitempty"`
	Replay     bool            `json:"replay,omitempty"`
}

type SelectionController interface {
	HandleSelectionTool(ctx context.Context, name string, arguments json.RawMessage, toolCallID string) (Result, error)
}

type ReviewSubmitter interface {
	HandleReviewSubmit(ctx context.Context, arguments json.RawMessage, toolCallID string) (Result, error)
}

type Scope struct {
	ID             string
	Snapshot       *workspacesnapshot.Snapshot
	Role           Role
	Selection      SelectionController
	Review         ReviewSubmitter
	MaxResultBytes int
}

func NewScope(id string, snapshot *workspacesnapshot.Snapshot, role Role) *Scope {
	if id == "" && snapshot != nil {
		id = snapshot.ID
	}
	return &Scope{ID: id, Snapshot: snapshot, Role: role}
}

var (
	ErrUnknownTool        = errors.New("agent tools: unknown tool")
	ErrCapabilityDenied   = errors.New("agent tools: capability denied")
	ErrInvalidInvocation  = errors.New("agent tools: invalid invocation")
	ErrReplayConflict     = errors.New("agent tools: tool call ID replay conflict")
	ErrHandlerUnavailable = errors.New("agent tools: handler unavailable")
)

type registeredTool struct {
	definition Definition
	handler    Handler
}

type replayEntry struct {
	digest string
	result Result
}

type Registry struct {
	mu     sync.RWMutex
	tools  map[string]registeredTool
	replay map[string]replayEntry
}

func NewRegistry() *Registry {
	registry := &Registry{tools: make(map[string]registeredTool), replay: make(map[string]replayEntry)}
	registerCoreTools(registry)
	return registry
}

func (r *Registry) Register(def Definition, handler Handler) error {
	if r == nil {
		return fmt.Errorf("agent tools: nil registry")
	}
	if def.Name == "" || def.Capability == "" || handler == nil {
		return fmt.Errorf("%w: definition name, capability, and handler are required", ErrInvalidInvocation)
	}
	if len(def.InputSchema) == 0 || !json.Valid(def.InputSchema) {
		return fmt.Errorf("%w: %s has invalid input schema", ErrInvalidInvocation, def.Name)
	}
	if def.MaxResultBytes <= 0 {
		def.MaxResultBytes = DefaultMaxResultBytes
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[def.Name]; exists {
		return fmt.Errorf("agent tools: duplicate definition %q", def.Name)
	}
	r.tools[def.Name] = registeredTool{definition: def, handler: handler}
	return nil
}

func (r *Registry) List(role Role) []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var definitions []Definition
	for _, tool := range r.tools {
		if roleAllows(role, tool.definition.Capability) {
			def := tool.definition
			def.InputSchema = append(json.RawMessage(nil), def.InputSchema...)
			definitions = append(definitions, def)
		}
	}
	sortDefinitions(definitions)
	return definitions
}

func (r *Registry) Dispatch(ctx context.Context, invocation Invocation) (Result, error) {
	if invocation.Scope == nil || invocation.Scope.Snapshot == nil || invocation.Scope.ID == "" ||
		invocation.Name == "" || invocation.ToolCallID == "" {
		return Result{}, ErrInvalidInvocation
	}
	if len(invocation.Arguments) == 0 {
		invocation.Arguments = json.RawMessage(`{}`)
	}
	if !json.Valid(invocation.Arguments) {
		return Result{}, fmt.Errorf("%w: arguments are not valid JSON", ErrInvalidInvocation)
	}

	r.mu.RLock()
	tool, ok := r.tools[invocation.Name]
	r.mu.RUnlock()
	if !ok {
		return Result{}, ErrUnknownTool
	}
	if !roleAllows(invocation.Scope.Role, tool.definition.Capability) {
		return Result{}, fmt.Errorf("%w: role %s cannot call %s", ErrCapabilityDenied, invocation.Scope.Role, invocation.Name)
	}

	digest := invocationDigest(invocation.Name, invocation.Arguments)
	replayKey := invocation.Scope.ID + "\x00" + invocation.ToolCallID
	r.mu.RLock()
	cached, replayed := r.replay[replayKey]
	r.mu.RUnlock()
	if replayed {
		if cached.digest != digest {
			return Result{}, ErrReplayConflict
		}
		result := cloneResult(cached.result)
		result.Replay = true
		return result, nil
	}

	result, err := tool.handler(ctx, invocation)
	if err != nil {
		return Result{}, err
	}
	limit := tool.definition.MaxResultBytes
	if invocation.Scope.MaxResultBytes > 0 && invocation.Scope.MaxResultBytes < limit {
		limit = invocation.Scope.MaxResultBytes
	}
	result = limitResult(result, limit)

	r.mu.Lock()
	if existing, exists := r.replay[replayKey]; exists {
		r.mu.Unlock()
		if existing.digest != digest {
			return Result{}, ErrReplayConflict
		}
		replayed := cloneResult(existing.result)
		replayed.Replay = true
		return replayed, nil
	}
	r.replay[replayKey] = replayEntry{digest: digest, result: cloneResult(result)}
	r.mu.Unlock()
	return result, nil
}

func (r *Registry) ClearScopeReplay(scopeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := scopeID + "\x00"
	for key := range r.replay {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			delete(r.replay, key)
		}
	}
}

func roleAllows(role Role, capability Capability) bool {
	switch role {
	case RoleContextDiscovery:
		return capability == CapabilityRead || capability == CapabilitySelectionRead ||
			capability == CapabilitySelectionMutate || capability == CapabilitySelectionFinalize
	case RoleCodeReviewer:
		return capability == CapabilityRead || capability == CapabilityReviewSubmit
	case RoleSecurityAuditor:
		return capability == CapabilityRead || capability == CapabilityReviewSubmit ||
			capability == CapabilitySnapshotWide
	case RoleExternalReadonly:
		return capability == CapabilityRead
	default:
		return false
	}
}
