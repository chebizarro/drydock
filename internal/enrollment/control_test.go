package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/contextvm"
	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
)

func (f *fixture) operatorCall(ctrl *OperatorControl, sender nostr.SecretKey, params OperatorParams, withExpiration bool) (OperatorState, *contextvm.Error) {
	f.t.Helper()
	raw, _ := json.Marshal(params)
	tags := nostr.Tags{{"p", f.servicePK.Hex()}, {"method", MethodOperator}}
	if withExpiration {
		tags = append(tags, nostr.Tag{"expiration", strconv.FormatInt(time.Now().Add(5*time.Minute).Unix(), 10)})
	}
	event := nostr.Event{Kind: contextvm.KindContextVM, CreatedAt: nostr.Now(), Tags: tags, Content: string(raw)}
	if err := event.Sign(sender); err != nil {
		f.t.Fatal(err)
	}
	result, rpcErr := ctrl.Handle(f.ctx, contextvm.Request{Event: event, Sender: event.PubKey, Msg: contextvm.Message{JSONRPC: "2.0", ID: "op-" + event.ID.Hex()[:8], Method: MethodOperator, Params: raw}})
	if rpcErr != nil {
		return OperatorState{}, rpcErr
	}
	return result.(OperatorState), nil
}

func (f *fixture) drainChanges() {
	for {
		select {
		case <-f.registry.Changes():
		default:
			return
		}
	}
}

func (f *fixture) persistedOperator() (string, string, string) {
	cur, _, _ := f.store.GetRuntimeSetting(f.ctx, db.RuntimeSettingMonitoringOperator)
	prev, _, _ := f.store.GetRuntimeSetting(f.ctx, db.RuntimeSettingMonitoringOperatorPrevious)
	has, _, _ := f.store.GetRuntimeSetting(f.ctx, db.RuntimeSettingMonitoringOperatorHasPrevious)
	return cur, prev, has
}

func TestOperatorControlRotatesPersistsAndRollsBack(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	f.announce(10)
	admin, opA, opB, stranger := nostr.Generate(), nostr.Generate(), nostr.Generate(), nostr.Generate()
	ctrl := NewOperatorControl(f.registry, f.store, []string{nostr.GetPublicKey(admin).Hex()}, f.servicePK.Hex(), nil)

	if _, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !f.registry.Contains(f.repo.Address) {
		t.Fatal("precondition: monitored without ceiling")
	}
	if _, rpcErr := f.operatorCall(ctrl, stranger, OperatorParams{Action: "get"}, true); rpcErr == nil || rpcErr.Code != contextvm.ErrorUnauthorized {
		t.Fatalf("stranger get = %+v", rpcErr)
	}
	if _, rpcErr := f.operatorCall(ctrl, admin, OperatorParams{Action: "set", Operator: nostr.GetPublicKey(opA).Hex()}, false); rpcErr == nil || rpcErr.Code != contextvm.ErrorInvalidParams {
		t.Fatalf("missing expiration = %+v", rpcErr)
	}
	if _, rpcErr := f.operatorCall(ctrl, admin, OperatorParams{Action: "rollback"}, true); rpcErr == nil || rpcErr.Code != contextvm.ErrorConflict {
		t.Fatalf("rollback with no history = %+v", rpcErr)
	}

	f.drainChanges()
	state, rpcErr := f.operatorCall(ctrl, admin, OperatorParams{Action: "set", Operator: nostr.GetPublicKey(opA).Hex()}, true)
	if rpcErr != nil || !state.Changed || state.Operator != nostr.GetPublicKey(opA).Hex() || state.CeilingSeen || state.Monitored != 0 || !state.HasPrevious || state.PreviousOperator != "" {
		t.Fatalf("set A = %+v err=%+v", state, rpcErr)
	}
	if f.registry.Contains(f.repo.Address) {
		t.Fatal("membership must be empty until the new operator's list arrives")
	}
	if cur, prev, has := f.persistedOperator(); cur != nostr.GetPublicKey(opA).Hex() || prev != "" || has != "1" {
		t.Fatalf("persisted = %q %q %q", cur, prev, has)
	}
	select {
	case <-f.registry.Changes():
	default:
		t.Fatal("operator change did not signal routing change")
	}

	list := nostr.Event{Kind: 30001, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", "drydock:monitored-repositories:v1"}, {"a", f.repo.Address}}}
	if err := list.Sign(opA); err != nil {
		t.Fatal(err)
	}
	if applied, err := f.registry.ApplyList(f.ctx, list); err != nil || !applied {
		t.Fatalf("apply A list: %v %v", applied, err)
	}
	if !f.registry.Contains(f.repo.Address) {
		t.Fatal("ceiling list did not restore membership")
	}

	// The current operator may rotate to B; afterwards A has no control.
	state, rpcErr = f.operatorCall(ctrl, opA, OperatorParams{Action: "set", Operator: nostr.GetPublicKey(opB).Hex()}, true)
	if rpcErr != nil || state.Operator != nostr.GetPublicKey(opB).Hex() || state.PreviousOperator != nostr.GetPublicKey(opA).Hex() {
		t.Fatalf("A rotates to B = %+v err=%+v", state, rpcErr)
	}
	if _, rpcErr := f.operatorCall(ctrl, opA, OperatorParams{Action: "set", Operator: nostr.GetPublicKey(opA).Hex()}, true); rpcErr == nil || rpcErr.Code != contextvm.ErrorUnauthorized {
		t.Fatalf("former operator must lose control: %+v", rpcErr)
	}
	state, rpcErr = f.operatorCall(ctrl, admin, OperatorParams{Action: "rollback"}, true)
	if rpcErr != nil || !state.Changed || state.Operator != nostr.GetPublicKey(opA).Hex() || !state.CeilingSeen || state.Monitored != 1 {
		t.Fatalf("rollback = %+v err=%+v", state, rpcErr)
	}
	if cur, prev, _ := f.persistedOperator(); cur != nostr.GetPublicKey(opA).Hex() || prev != nostr.GetPublicKey(opB).Hex() {
		t.Fatalf("persisted after rollback = %q %q", cur, prev)
	}
	state, rpcErr = f.operatorCall(ctrl, admin, OperatorParams{Action: "set", Operator: nostr.GetPublicKey(opA).Hex()}, true)
	if rpcErr != nil || state.Changed {
		t.Fatalf("idempotent set = %+v err=%+v", state, rpcErr)
	}
}

func TestOperatorControlScopeExpansionAndLastPrincipal(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	admin, opA := nostr.Generate(), nostr.Generate()
	ctrl := NewOperatorControl(f.registry, f.store, []string{nostr.GetPublicKey(admin).Hex()}, f.servicePK.Hex(), nil)
	if _, rpcErr := f.operatorCall(ctrl, admin, OperatorParams{Action: "set", Operator: nostr.GetPublicKey(opA).Hex()}, true); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if _, rpcErr := f.operatorCall(ctrl, admin, OperatorParams{Action: "set"}, true); rpcErr == nil || rpcErr.Code != contextvm.ErrorInvalidParams {
		t.Fatalf("clearing without acknowledgement = %+v", rpcErr)
	}
	// Rollback that would remove the ceiling is gated the same way.
	if _, rpcErr := f.operatorCall(ctrl, admin, OperatorParams{Action: "rollback"}, true); rpcErr == nil || rpcErr.Code != contextvm.ErrorInvalidParams {
		t.Fatalf("rollback to no ceiling without acknowledgement = %+v", rpcErr)
	}
	state, rpcErr := f.operatorCall(ctrl, admin, OperatorParams{Action: "rollback", AllowScopeExpansion: true}, true)
	if rpcErr != nil || state.Operator != "" || state.PreviousOperator != nostr.GetPublicKey(opA).Hex() {
		t.Fatalf("acknowledged rollback = %+v err=%+v", state, rpcErr)
	}
	if _, rpcErr := f.operatorCall(ctrl, admin, OperatorParams{Action: "set", Operator: "nope"}, true); rpcErr == nil || rpcErr.Code != contextvm.ErrorInvalidParams {
		t.Fatalf("bad operator = %+v", rpcErr)
	}

	// Operator without admins cannot clear itself (last principal), may rotate.
	g := newFixture(t, Policy{}, nil, scope.Matcher{})
	g.registry.SetOperatorState(nostr.GetPublicKey(opA), nostr.ZeroPK, false)
	solo := NewOperatorControl(g.registry, g.store, nil, g.servicePK.Hex(), nil)
	if _, rpcErr := g.operatorCall(solo, opA, OperatorParams{Action: "set", AllowScopeExpansion: true}, true); rpcErr == nil || rpcErr.Code != contextvm.ErrorConflict {
		t.Fatalf("last principal clearing itself = %+v", rpcErr)
	}
	opB := nostr.Generate()
	if state, rpcErr := g.operatorCall(solo, opA, OperatorParams{Action: "set", Operator: nostr.GetPublicKey(opB).Hex()}, true); rpcErr != nil || state.Operator != nostr.GetPublicKey(opB).Hex() {
		t.Fatalf("rotation keeps a principal = %+v err=%+v", state, rpcErr)
	}
	// No admins and no operator: closed.
	h := newFixture(t, Policy{}, nil, scope.Matcher{})
	closed := NewOperatorControl(h.registry, h.store, nil, h.servicePK.Hex(), nil)
	if _, rpcErr := h.operatorCall(closed, admin, OperatorParams{Action: "set", Operator: nostr.GetPublicKey(opA).Hex()}, true); rpcErr == nil || rpcErr.Code != contextvm.ErrorUnauthorized {
		t.Fatalf("unconfigured control plane = %+v", rpcErr)
	}
}

type failingSettings struct {
	SettingsStore
	fail bool
}

func (f *failingSettings) SetRuntimeSettings(ctx context.Context, values map[string]string, by, ev string) error {
	if f.fail {
		return errors.New("disk full")
	}
	return f.SettingsStore.SetRuntimeSettings(ctx, values, by, ev)
}

func TestOperatorControlPersistsBeforePublishing(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	admin, opA := nostr.Generate(), nostr.Generate()
	settings := &failingSettings{SettingsStore: f.store, fail: true}
	ctrl := NewOperatorControl(f.registry, settings, []string{nostr.GetPublicKey(admin).Hex()}, f.servicePK.Hex(), nil)
	if _, rpcErr := f.operatorCall(ctrl, admin, OperatorParams{Action: "set", Operator: nostr.GetPublicKey(opA).Hex()}, true); rpcErr == nil || rpcErr.Code != contextvm.ErrorInternal {
		t.Fatalf("persist failure = %+v", rpcErr)
	}
	if cur, _, _ := f.registry.OperatorState(); cur != nostr.ZeroPK {
		t.Fatal("live state changed although persistence failed")
	}
	if _, found, _ := f.store.GetRuntimeSetting(f.ctx, db.RuntimeSettingMonitoringOperator); found {
		t.Fatal("partial persistence")
	}
}

func TestOperatorControlSerializesConcurrentChanges(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	admin := nostr.Generate()
	ctrl := NewOperatorControl(f.registry, f.store, []string{nostr.GetPublicKey(admin).Hex()}, f.servicePK.Hex(), nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op := nostr.GetPublicKey(nostr.Generate()).Hex()
			if _, rpcErr := f.operatorCall(ctrl, admin, OperatorParams{Action: "set", Operator: op}, true); rpcErr != nil {
				t.Errorf("concurrent set: %+v", rpcErr)
			}
		}()
	}
	wg.Wait()
	cur, prev, _ := f.registry.OperatorState()
	pCur, pPrev, _ := f.persistedOperator()
	if cur.Hex() != pCur || prev.Hex() != pPrev {
		t.Fatalf("live (%s,%s) != persisted (%s,%s)", cur.Hex(), prev.Hex(), pCur, pPrev)
	}
}

func TestBootstrapOperatorHydratesHistoryAndHonoursForce(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	boot, err := BootstrapOperator(context.Background(), f.store, "env-value", false)
	if err != nil || boot.Operator != "env-value" || boot.Source != "environment" || boot.HasPrevious {
		t.Fatalf("fresh db = %+v %v", boot, err)
	}
	admin, opA := nostr.Generate(), nostr.Generate()
	ctrl := NewOperatorControl(f.registry, f.store, []string{nostr.GetPublicKey(admin).Hex()}, f.servicePK.Hex(), nil)
	if _, rpcErr := f.operatorCall(ctrl, admin, OperatorParams{Action: "set", Operator: nostr.GetPublicKey(opA).Hex()}, true); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	boot, err = BootstrapOperator(context.Background(), f.store, "env-value", false)
	if err != nil || boot.Operator != nostr.GetPublicKey(opA).Hex() || boot.Source != "runtime_settings" || !boot.HasPrevious || boot.PreviousOperator != "" {
		t.Fatalf("persisted must win over env = %+v %v", boot, err)
	}
	// After a "restart" the history is hydrated, so rollback works.
	restarted, err := monitoringRegistryFromBoot(f, boot)
	if err != nil {
		t.Fatal(err)
	}
	ctrl2 := NewOperatorControl(restarted, f.store, []string{nostr.GetPublicKey(admin).Hex()}, f.servicePK.Hex(), nil)
	f.registry = restarted
	state, rpcErr := f.operatorCall(ctrl2, admin, OperatorParams{Action: "rollback", AllowScopeExpansion: true}, true)
	if rpcErr != nil || state.Operator != "" || state.PreviousOperator != nostr.GetPublicKey(opA).Hex() {
		t.Fatalf("rollback after restart = %+v err=%+v", state, rpcErr)
	}
	// Force: environment overrides persisted, prior becomes rollback target.
	forced, err := BootstrapOperator(context.Background(), f.store, nostr.GetPublicKey(opA).Hex(), true)
	if err != nil || forced.Operator != nostr.GetPublicKey(opA).Hex() || forced.Source != "environment (forced)" || !forced.HasPrevious || forced.PreviousOperator != "" {
		t.Fatalf("forced = %+v %v", forced, err)
	}
	if cur, _, has := f.persistedOperator(); cur != nostr.GetPublicKey(opA).Hex() || has != "1" {
		t.Fatalf("forced value not persisted: %q %q", cur, has)
	}
}
