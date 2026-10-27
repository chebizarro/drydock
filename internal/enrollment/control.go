package enrollment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/contextvm"
	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/monitoring"
	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
)

// MethodOperator is the authenticated live control plane for the operator
// ceiling: rotate it, roll it back, or read it, without a restart.
const MethodOperator = "monitoring/operator"

var (
	// ErrScopeExpansion: the change would remove the ceiling, widening
	// effective membership to every maintainer opt-in, and the caller did not
	// acknowledge it. The flag acknowledges ceiling *removal* only; replacing
	// one operator by another is not gated, because a different list cannot
	// authorize strangers, it can only select among existing maintainer
	// opt-ins.
	ErrScopeExpansion = errors.New("removing the operator ceiling expands monitored scope; set allow_scope_expansion")
	// ErrNoPreviousOperator: nothing to roll back to.
	ErrNoPreviousOperator = errors.New("no previous operator state to roll back to")
	// ErrLastPrincipal: the change would leave nobody able to use this method.
	ErrLastPrincipal = errors.New("change would remove the last control-plane principal; configure DRYDOCK_MONITORING_ADMINS first")
)

// OperatorParams is the monitoring/operator contract.
type OperatorParams struct {
	// Action is "set", "rollback", or "get".
	Action string `json:"action"`
	// Operator is the new ceiling author (hex or npub) for "set"; empty clears
	// the ceiling, which requires AllowScopeExpansion.
	Operator string `json:"operator,omitempty"`
	// AllowScopeExpansion acknowledges that the change removes the ceiling.
	AllowScopeExpansion bool `json:"allow_scope_expansion,omitempty"`
}

// OperatorState is the response: the ceiling after the action.
type OperatorState struct {
	Action           string `json:"action"`
	Changed          bool   `json:"changed"`
	Operator         string `json:"operator,omitempty"`
	PreviousOperator string `json:"previous_operator,omitempty"`
	HasPrevious      bool   `json:"has_previous"`
	// CeilingSeen reports whether the current operator's list is known; false
	// means membership is empty until it arrives.
	CeilingSeen bool `json:"ceiling_seen"`
	// Monitored is the expiry-aware effective membership count.
	Monitored int `json:"monitored"`
}

// OperatorRegistry is the registry surface the control plane drives.
type OperatorRegistry interface {
	OperatorState() (current, previous nostr.PubKey, hasPrevious bool)
	SetOperatorState(current, previous nostr.PubKey, hasPrevious bool)
	Snapshot() monitoring.Snapshot
	MonitoredCount() int
}

// SettingsStore persists control-plane state across restarts and reads back
// the last applied command so a redelivered request is idempotent.
type SettingsStore interface {
	GetRuntimeSetting(ctx context.Context, key string) (string, bool, error)
	SetRuntimeSettings(ctx context.Context, values map[string]string, changedBy, changeEventID string) error
}

// OperatorControl handles monitoring/operator. One mutex serializes
// authorization, the durable write, and the live publication, so two callers
// cannot interleave and the registry never shows a state that was not first
// persisted.
type OperatorControl struct {
	mu            sync.Mutex
	registry      OperatorRegistry
	settings      SettingsStore
	admins        map[string]struct{}
	servicePubkey string
	logger        *slog.Logger
	now           func() time.Time
}

// NewOperatorControl builds the control handler. admins are pubkeys (hex or
// npub) allowed to use the method in addition to the current operator.
func NewOperatorControl(registry OperatorRegistry, settings SettingsStore, admins []string, servicePubkey string, logger *slog.Logger) *OperatorControl {
	if logger == nil {
		logger = slog.Default()
	}
	set := map[string]struct{}{}
	for _, raw := range admins {
		if pk, err := scope.ParsePubkey(raw); err == nil {
			set[pk.Hex()] = struct{}{}
		}
	}
	return &OperatorControl{registry: registry, settings: settings, admins: set, servicePubkey: scope.NormalizePubkey(servicePubkey), logger: logger, now: time.Now}
}

func (c *OperatorControl) RegisterContextVMMethods(router *contextvm.Router) error {
	if router == nil {
		return errors.New("contextvm router is required")
	}
	return router.Register(MethodOperator, c.Handle)
}

// authorizedLocked reports whether sender may use the method: a configured
// admin, or the current operator itself. Callers hold c.mu so the check and
// the transition see the same current operator.
func (c *OperatorControl) authorizedLocked(sender nostr.PubKey) bool {
	if _, ok := c.admins[sender.Hex()]; ok {
		return true
	}
	current, _, _ := c.registry.OperatorState()
	return current != nostr.ZeroPK && current == sender
}

func (c *OperatorControl) Handle(ctx context.Context, request contextvm.Request) (any, *contextvm.Error) {
	if c.registry == nil || c.settings == nil {
		return nil, &contextvm.Error{Code: contextvm.ErrorInternal, Message: "operator control is not configured"}
	}
	if request.Msg.ID == "" {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidRequest, Message: MethodOperator + " requires a JSON-RPC id"}
	}
	if c.servicePubkey == "" {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "p tag must address the Drydock service"}
	}
	if rpcErr := contextvm.ValidateEnvelope(request, contextvm.EnvelopePolicy{
		ServicePubkey: c.servicePubkey, Method: MethodOperator, ExpirationRequired: true, MaxLifetime: maxRequestLifetime, Now: c.now,
	}); rpcErr != nil {
		return nil, rpcErr
	}
	if request.Sender == nostr.ZeroPK || request.Sender != request.Event.PubKey {
		return nil, &contextvm.Error{Code: contextvm.ErrorUnauthorized, Message: "sender must be the event signer"}
	}
	params, rpcErr := contextvm.ParamsAs[OperatorParams](request)
	if rpcErr != nil {
		return nil, rpcErr
	}
	action := strings.ToLower(strings.TrimSpace(params.Action))
	var target nostr.PubKey
	if action == "set" {
		if raw := strings.TrimSpace(params.Operator); raw != "" {
			pk, err := scope.ParsePubkey(raw)
			if err != nil {
				return nil, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "operator must be a hex or npub public key"}
			}
			target = pk
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.authorizedLocked(request.Sender) {
		return nil, &contextvm.Error{Code: contextvm.ErrorUnauthorized, Message: "sender is neither a monitoring admin nor the current operator"}
	}
	current, previous, hasPrevious := c.registry.OperatorState()

	// Durable idempotency: the last applied command's event id is persisted in
	// the same write as the state it produced. A redelivery (relay replay, or a
	// crash after the commit but before ingest recorded handler completion)
	// answers with the current state and changes nothing — a retried rollback
	// must not swap the operators again.
	if action != "get" {
		last, _, err := c.settings.GetRuntimeSetting(ctx, db.RuntimeSettingMonitoringOperatorLastCommand)
		if err != nil {
			return nil, &contextvm.Error{Code: contextvm.ErrorInternal, Message: "operator control state unavailable"}
		}
		if last == request.Event.ID.Hex() {
			return c.stateLocked(action, false), nil
		}
	}

	var next struct {
		current, previous nostr.PubKey
		hasPrevious       bool
	}
	switch action {
	case "get":
		return c.stateLocked("get", false), nil
	case "set":
		if target == current {
			return c.stateLocked("set", false), nil
		}
		next.current, next.previous, next.hasPrevious = target, current, true
	case "rollback":
		if !hasPrevious {
			return nil, &contextvm.Error{Code: contextvm.ErrorConflict, Message: ErrNoPreviousOperator.Error()}
		}
		next.current, next.previous, next.hasPrevious = previous, current, true
	default:
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "action must be set, rollback, or get"}
	}

	// Policy, decided before any write.
	if next.current == nostr.ZeroPK && current != nostr.ZeroPK && !params.AllowScopeExpansion {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: ErrScopeExpansion.Error()}
	}
	if next.current == nostr.ZeroPK && len(c.admins) == 0 {
		return nil, &contextvm.Error{Code: contextvm.ErrorConflict, Message: ErrLastPrincipal.Error()}
	}

	// Durable first, then live. A crash in between is recovered by bootstrap
	// reading the persisted state; the live registry never runs ahead of it.
	sender, eventID := request.Sender.Hex(), request.Event.ID.Hex()
	if err := c.persist(ctx, next.current, next.previous, next.hasPrevious, sender, eventID); err != nil {
		c.logger.Error("failed to persist operator change; live state unchanged", "error", err, "action", action)
		return nil, &contextvm.Error{Code: contextvm.ErrorInternal, Message: "operator change could not be persisted"}
	}
	before := c.registry.MonitoredCount()
	c.registry.SetOperatorState(next.current, next.previous, next.hasPrevious)
	c.logger.Warn("operator ceiling changed through control plane",
		"action", action, "from", hexOf(current), "to", hexOf(next.current), "by", sender, "event_id", eventID,
		"ceiling_removed", next.current == nostr.ZeroPK, "monitored_before", before, "monitored_after", c.registry.MonitoredCount())
	return c.stateLocked(action, true), nil
}

func hexOf(pk nostr.PubKey) string {
	if pk == nostr.ZeroPK {
		return ""
	}
	return pk.Hex()
}

func (c *OperatorControl) persist(ctx context.Context, current, previous nostr.PubKey, hasPrevious bool, sender, eventID string) error {
	hasPrev := "0"
	if hasPrevious {
		hasPrev = "1"
	}
	return c.settings.SetRuntimeSettings(ctx, map[string]string{
		db.RuntimeSettingMonitoringOperator:            hexOf(current),
		db.RuntimeSettingMonitoringOperatorPrevious:    hexOf(previous),
		db.RuntimeSettingMonitoringOperatorHasPrevious: hasPrev,
		db.RuntimeSettingMonitoringOperatorLastCommand: eventID,
	}, sender, eventID)
}

func (c *OperatorControl) stateLocked(action string, changed bool) OperatorState {
	snapshot := c.registry.Snapshot()
	_, _, hasPrevious := c.registry.OperatorState()
	return OperatorState{
		Action:           action,
		Changed:          changed,
		Operator:         snapshot.Operator,
		PreviousOperator: snapshot.PreviousOperator,
		HasPrevious:      hasPrevious,
		CeilingSeen:      snapshot.CeilingSeen,
		Monitored:        c.registry.MonitoredCount(),
	}
}

// OperatorBootstrap is the operator ceiling state resolved at startup.
type OperatorBootstrap struct {
	Operator         string
	PreviousOperator string
	HasPrevious      bool
	// Source is "runtime_settings", "environment", or "environment (forced)".
	Source string
}

// SettingsReader reads persisted control-plane state. *db.Store satisfies it.
type SettingsReader interface {
	GetRuntimeSetting(ctx context.Context, key string) (string, bool, error)
	SetRuntimeSettings(ctx context.Context, values map[string]string, changedBy, changeEventID string) error
}

// BootstrapOperator resolves the operator at startup. A persisted
// control-plane value wins over the environment, which only seeds a fresh
// database — unless force is set, the audited local override for an operator
// who has lost control-plane access: the environment value is then persisted
// as the new state (with the prior one as rollback target) and logged by the
// caller.
func BootstrapOperator(ctx context.Context, settings SettingsReader, envOperator string, force bool) (OperatorBootstrap, error) {
	envOperator = strings.TrimSpace(envOperator)
	persisted, found, err := settings.GetRuntimeSetting(ctx, db.RuntimeSettingMonitoringOperator)
	if err != nil {
		return OperatorBootstrap{}, fmt.Errorf("read persisted operator: %w", err)
	}
	if !found {
		return OperatorBootstrap{Operator: envOperator, Source: "environment"}, nil
	}
	persisted = strings.TrimSpace(persisted)
	previous, _, err := settings.GetRuntimeSetting(ctx, db.RuntimeSettingMonitoringOperatorPrevious)
	if err != nil {
		return OperatorBootstrap{}, fmt.Errorf("read persisted previous operator: %w", err)
	}
	hasPrevRaw, hasPrevFound, err := settings.GetRuntimeSetting(ctx, db.RuntimeSettingMonitoringOperatorHasPrevious)
	if err != nil {
		return OperatorBootstrap{}, fmt.Errorf("read persisted operator history: %w", err)
	}
	hasPrevious := hasPrevFound && strings.TrimSpace(hasPrevRaw) == "1"
	if force && envOperator != persisted {
		if err := settings.SetRuntimeSettings(ctx, map[string]string{
			db.RuntimeSettingMonitoringOperator:            envOperator,
			db.RuntimeSettingMonitoringOperatorPrevious:    persisted,
			db.RuntimeSettingMonitoringOperatorHasPrevious: "1",
		}, "environment-force", ""); err != nil {
			return OperatorBootstrap{}, fmt.Errorf("persist forced operator: %w", err)
		}
		return OperatorBootstrap{Operator: envOperator, PreviousOperator: persisted, HasPrevious: true, Source: "environment (forced)"}, nil
	}
	return OperatorBootstrap{Operator: persisted, PreviousOperator: strings.TrimSpace(previous), HasPrevious: hasPrevious, Source: "runtime_settings"}, nil
}
