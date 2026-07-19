package listener

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"fiatjaf.com/nostr"
)

func noopLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestSubscribedKindsSet(t *testing.T) {
	kinds := SubscribedKinds()

	expected := map[int]bool{
		30617: true,
		30618: true,
		1617:  true,
		1618:  true,
		1619:  true,
		1621:  true,
		1111:  true,
		1630:  true,
		1631:  true,
		1632:  true,
		1633:  true,
		1985:  true,
		4:     true, // NIP-04 DMs
		14:    true, // NIP-17 sealed DMs
		1059:  true, // NIP-59 gift wraps
		30078: true, // IDE workspace session (NIP-78 app data)
		25910: true, // ContextVM IDE/marketplace intents/responses
		31990: true, // Marketplace: reviewer profile (NIP-89 app handler)
		7000:  true, // Marketplace: NIP-90 review feedback
		9735:  true, // NIP-57 zap receipt
	}

	if len(kinds) != len(expected) {
		t.Fatalf("expected %d kinds, got %d", len(expected), len(kinds))
	}

	seen := make(map[int]bool, len(kinds))
	for _, kind := range kinds {
		seen[int(kind)] = true
	}

	for kind := range expected {
		if !seen[kind] {
			t.Fatalf("missing kind %d", kind)
		}
	}
}

func TestSubscribedKindsReturnsCopy(t *testing.T) {
	k1 := SubscribedKinds()
	k2 := SubscribedKinds()
	// Mutating the returned slice should not affect the original.
	k1[0] = 9999
	if k2[0] == 9999 {
		t.Fatal("SubscribedKinds returned a shared slice, not a copy")
	}
}

// fakeProcessor records ProcessEvent calls.
type fakeProcessor struct {
	events []nostr.Event
	err    error
}

func (f *fakeProcessor) ProcessEvent(_ context.Context, event nostr.Event, _ string) error {
	f.events = append(f.events, event)
	return f.err
}

type fakeHighWaterStore struct {
	mark        int64
	updates     []int64
	updateErr   error
	updateCalls int
}

func (f *fakeHighWaterStore) GetListenerHighWaterMark(context.Context) (int64, error) {
	return f.mark, nil
}

func (f *fakeHighWaterStore) UpdateListenerHighWaterMark(_ context.Context, ts int64) error {
	f.updateCalls++
	if f.updateErr != nil {
		return f.updateErr
	}
	f.mark = ts
	f.updates = append(f.updates, ts)
	return nil
}

type fakeOpener struct {
	opened nostr.Event
	err    error
}

func (f fakeOpener) OpenGiftWrap(context.Context, nostr.Event) (nostr.Event, error) {
	return f.opened, f.err
}

func TestRunReturnsErrorWhenNoRelays(t *testing.T) {
	proc := &fakeProcessor{}
	svc := New(Config{
		Relays:          nil,
		LookbackMinutes: 5,
	}, proc, noopLogger())

	if err := svc.Run(context.Background()); err == nil {
		t.Fatal("expected startup error with no relays")
	}
}

func TestNewAppliesOptions(t *testing.T) {
	proc := &fakeProcessor{}
	pool := nostr.NewPool()

	svc := New(Config{
		Relays:          []string{"wss://test.relay"},
		LookbackMinutes: 10,
	}, proc, noopLogger(), WithPool(pool))

	if svc.pool != pool {
		t.Fatal("WithPool option not applied")
	}
	if svc.cfg.LookbackMinutes != 10 {
		t.Fatalf("expected lookback 10, got %d", svc.cfg.LookbackMinutes)
	}
}

func TestNewCreatesDefaultPool(t *testing.T) {
	proc := &fakeProcessor{}
	svc := New(Config{}, proc, noopLogger())
	if svc.pool == nil {
		t.Fatal("expected default pool to be created")
	}
}

func TestProcessRelayEventDoesNotAdvanceHighWaterOnProcessingFailure(t *testing.T) {
	proc := &fakeProcessor{err: errors.New("boom")}
	store := &fakeHighWaterStore{}
	svc := New(Config{}, proc, noopLogger())
	svc.store = store
	var lastSeen atomic.Int64

	svc.processRelayEvent(context.Background(), nostr.RelayEvent{Event: nostr.Event{
		ID:        nostr.MustIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Kind:      1,
		CreatedAt: nostr.Timestamp(1234),
	}}, &lastSeen)

	if len(store.updates) != 0 {
		t.Fatalf("expected no high-water update on processing failure, got %v", store.updates)
	}
}

func TestProcessRelayEventDoesNotAdvanceHighWaterWhenPersistenceFails(t *testing.T) {
	proc := &fakeProcessor{}
	store := &fakeHighWaterStore{updateErr: errors.New("database unavailable")}
	svc := New(Config{}, proc, noopLogger())
	svc.store = store
	var lastSeen atomic.Int64
	failuresBefore := ListenerCheckpointPersistFailures.Value()

	svc.processRelayEvent(context.Background(), nostr.RelayEvent{Event: nostr.Event{
		ID:        nostr.MustIDFromHex("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"),
		Kind:      1,
		CreatedAt: nostr.Timestamp(4321),
	}}, &lastSeen)

	if got := lastSeen.Load(); got != 0 {
		t.Fatalf("lastSeen advanced despite persistence failure: %d", got)
	}
	if store.updateCalls != checkpointPersistAttempts {
		t.Fatalf("expected %d persistence attempts, got %d", checkpointPersistAttempts, store.updateCalls)
	}
	if got := ListenerCheckpointPersistFailures.Value(); got != failuresBefore+1 {
		t.Fatalf("expected checkpoint failure metric to increment, before=%d after=%d", failuresBefore, got)
	}
}

func TestProcessRelayEventOpensGiftWrapBeforeRouting(t *testing.T) {
	inner := nostr.Event{
		ID:        nostr.MustIDFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		Kind:      25910,
		CreatedAt: nostr.Timestamp(2222),
		Content:   "opened",
	}
	proc := &fakeProcessor{}
	store := &fakeHighWaterStore{}
	svc := New(Config{}, proc, noopLogger(), WithGiftWrapOpener(fakeOpener{opened: inner}))
	svc.store = store
	var lastSeen atomic.Int64

	svc.processRelayEvent(context.Background(), nostr.RelayEvent{Event: nostr.Event{
		ID:        nostr.MustIDFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		Kind:      1059,
		CreatedAt: nostr.Timestamp(3333),
	}}, &lastSeen)

	if len(proc.events) != 1 || proc.events[0].Kind != 25910 || proc.events[0].Content != "opened" {
		t.Fatalf("expected opened inner event to be routed, got %#v", proc.events)
	}
	if len(store.updates) != 1 || store.updates[0] != 3333 {
		t.Fatalf("expected wrapper timestamp high-water update after success, got %v", store.updates)
	}
}
