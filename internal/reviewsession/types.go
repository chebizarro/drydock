package reviewsession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"drydock/internal/reviewengine"
)

type Mode string

const (
	ModePatch         Mode = "patch"
	ModeInlinePatch   Mode = "inline_patch"
	ModeSecurityAudit Mode = "security_audit"
)

type State string

const (
	StateInitializing State = "initializing"
	StateActive       State = "active"
	StateBroken       State = "broken"
	StateExpired      State = "expired"
)

type TurnStatus string

const (
	TurnReserved TurnStatus = "reserved"
	TurnComplete TurnStatus = "complete"
	TurnFailed   TurnStatus = "failed"
)

var (
	ErrNotFound            = errors.New("review session: not found")
	ErrOwnerMismatch       = errors.New("review session: owner mismatch")
	ErrExpired             = errors.New("review session: expired")
	ErrBroken              = errors.New("review session: broken")
	ErrVersionConflict     = errors.New("review session: version conflict")
	ErrRequestInProgress   = errors.New("review session: request in progress")
	ErrIdempotencyConflict = errors.New("review session: request ID payload conflict")
	ErrActiveTurn          = errors.New("review session: another turn is active")
	ErrInvalidTranscript   = errors.New("review session: invalid transcript")
	ErrHistoryTooLarge     = errors.New("review session: history exceeds budget")
)

type Owner struct {
	Kind string
	ID   string
}

func (o Owner) Validate() error {
	if strings.TrimSpace(o.Kind) == "" || strings.TrimSpace(o.ID) == "" {
		return fmt.Errorf("review session: owner kind and ID are required")
	}
	return nil
}

type Snapshot struct {
	ID           string
	Kind         string
	StoragePath  string
	ManifestHash string
	DiffHash     string
	ExpiresAt    time.Time
}

type Artifact struct {
	Ordinal   int
	Kind      string
	Path      string
	StartLine int
	EndLine   int
	Hash      string
	Mandatory bool
}

type Session struct {
	ChatID         string
	Owner          Owner
	Mode           Mode
	State          State
	Snapshot       Snapshot
	TargetEnvelope json.RawMessage
	BundleHash     string
	Version        int
	ActiveRequest  string
	LeaseID        string
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Turn struct {
	TurnNo          int
	RequestID       string
	RequestHash     string
	RequestText     string
	ExpectedVersion int
	Status          TurnStatus
	Result          json.RawMessage
	Error           string
	CreatedAt       time.Time
	CompletedAt     time.Time
}

type Message struct {
	TurnNo           int
	Seq              int
	Role             reviewengine.MessageRole
	Name             string
	ToolCallID       string
	Content          string
	ToolCalls        []reviewengine.ToolCall
	PromptTokens     int
	CompletionTokens int
}

func (m Message) CompletionMessage() reviewengine.CompletionMessage {
	return reviewengine.CompletionMessage{
		Role: m.Role, Name: m.Name, ToolCallID: m.ToolCallID, Content: m.Content,
		ToolCalls: cloneToolCalls(m.ToolCalls),
	}
}

func MessageFromCompletion(turnNo int, message reviewengine.CompletionMessage) Message {
	return Message{
		TurnNo: turnNo, Role: message.Role, Name: message.Name,
		ToolCallID: message.ToolCallID, Content: message.Content,
		ToolCalls: cloneToolCalls(message.ToolCalls),
	}
}

type CreateParams struct {
	ChatID         string
	Owner          Owner
	Mode           Mode
	Snapshot       Snapshot
	TargetEnvelope json.RawMessage
	BundleHash     string
	LeaseID        string
	Artifacts      []Artifact
	RequestID      string
	RequestText    string
	ExpiresAt      time.Time
}

type Reservation struct {
	Session Session
	Turn    Turn
	Replay  bool
}

type ReserveTurnParams struct {
	ChatID          string
	Owner           Owner
	RequestID       string
	RequestText     string
	ExpectedVersion int
	LeaseID         string
	ExpiresAt       time.Time
}

type Loaded struct {
	Session   Session
	Artifacts []Artifact
	Turns     []Turn
	Messages  []Message
}

type Store interface {
	Create(context.Context, CreateParams) (Reservation, error)
	ReserveTurn(context.Context, ReserveTurnParams) (Reservation, error)
	AppendMessages(context.Context, string, string, []Message) error
	CompleteTurn(context.Context, string, string, json.RawMessage) error
	FailTurn(context.Context, string, string, error) error
	LoadForContinuation(context.Context, string) (Loaded, error)
	Expire(context.Context, string) (string, error)
	MarkBroken(context.Context, string, error) error
}

func NewChatID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("review session: generate chat ID: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func validMode(mode Mode) bool {
	return mode == ModePatch || mode == ModeInlinePatch || mode == ModeSecurityAudit
}

func cloneToolCalls(calls []reviewengine.ToolCall) []reviewengine.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	return append([]reviewengine.ToolCall(nil), calls...)
}
