package listener

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip59"
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

func TestSubscriptionFiltersScopeContextVMByRecipientAndMethod(t *testing.T) {
	since := nostr.Timestamp(1234)
	filters := subscriptionFilters(since, Config{
		GiftWrapEnabled:  true,
		ContextVMPubkey:  "service-pubkey",
		ContextVMMethods: []string{"security/audit", "review/request", "security/audit"},
	})
	if len(filters) != 2 {
		t.Fatalf("filter count = %d, want 2", len(filters))
	}
	contextFilter := filters[1]
	if len(contextFilter.Kinds) != 1 || contextFilter.Kinds[0] != 25910 {
		t.Fatalf("ContextVM kinds = %v", contextFilter.Kinds)
	}
	if got := contextFilter.Tags["p"]; len(got) != 1 || got[0] != "service-pubkey" {
		t.Fatalf("p filter = %v", got)
	}
	if got := contextFilter.Tags["method"]; len(got) != 2 || got[0] != "security/audit" || got[1] != "review/request" {
		t.Fatalf("method filter = %v", got)
	}
	if contextFilter.Since != since {
		t.Fatalf("since = %d, want %d", contextFilter.Since, since)
	}
	for _, kind := range filters[0].Kinds {
		if kind == 25910 {
			t.Fatal("general filter must exclude ContextVM when scoped filter is active")
		}
	}
}

func TestSubscriptionFiltersExcludeGiftWrapWhenDisabled(t *testing.T) {
	filters := subscriptionFilters(nostr.Timestamp(1234), Config{})
	if len(filters) != 1 {
		t.Fatalf("filter count = %d, want 1", len(filters))
	}
	for _, kind := range filters[0].Kinds {
		if kind == 1059 {
			t.Fatal("gift-wrap kind subscribed without a configured opener")
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

func (f *fakeProcessor) ProcessGiftWrappedEvent(_ context.Context, event nostr.Event, _ string) error {
	f.events = append(f.events, event)
	return f.err
}

type fakeHighWaterStore struct {
	mark        int64
	updates     []int64
	resets      []int64
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

func (f *fakeHighWaterStore) ResetListenerHighWaterMark(_ context.Context, ts int64) error {
	f.mark = ts
	f.resets = append(f.resets, ts)
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

func TestRunRejectsGiftWrapSubscriptionWithoutOpener(t *testing.T) {
	svc := New(Config{
		Relays:          []string{"wss://test.relay"},
		GiftWrapEnabled: true,
	}, &fakeProcessor{}, noopLogger())

	if err := svc.Run(context.Background()); err == nil {
		t.Fatal("expected startup error when gift-wrap subscription has no opener")
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

func TestProcessRelayEventClampsCheckpointForAcceptedFutureTimestamp(t *testing.T) {
	proc := &fakeProcessor{}
	store := &fakeHighWaterStore{}
	svc := New(Config{MaxFutureSkew: 10 * time.Minute}, proc, noopLogger())
	svc.store = store
	var lastSeen atomic.Int64
	before := time.Now().Unix()

	svc.processRelayEvent(context.Background(), nostr.RelayEvent{Event: nostr.Event{
		ID:        nostr.MustIDFromHex("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
		Kind:      1,
		CreatedAt: nostr.Timestamp(time.Now().Add(5 * time.Minute).Unix()),
	}}, &lastSeen)

	after := time.Now().Unix()
	if len(store.updates) != 1 {
		t.Fatalf("expected one high-water update, got %v", store.updates)
	}
	if got := store.updates[0]; got < before || got > after {
		t.Fatalf("checkpoint = %d, want wall-clock range [%d,%d]", got, before, after)
	}
	if got := lastSeen.Load(); got != store.updates[0] {
		t.Fatalf("lastSeen = %d, want %d", got, store.updates[0])
	}
}

func TestProcessRelayEventDoesNotAdvanceHighWaterForFutureTimestamp(t *testing.T) {
	proc := &fakeProcessor{}
	store := &fakeHighWaterStore{}
	svc := New(Config{MaxFutureSkew: 10 * time.Minute}, proc, noopLogger())
	svc.store = store
	var lastSeen atomic.Int64

	svc.processRelayEvent(context.Background(), nostr.RelayEvent{Event: nostr.Event{
		ID:        nostr.MustIDFromHex("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"),
		Kind:      1,
		CreatedAt: nostr.Timestamp(time.Now().Add(time.Hour).Unix()),
	}}, &lastSeen)

	if len(proc.events) != 1 {
		t.Fatalf("expected event to be passed to the processor, got %d calls", len(proc.events))
	}
	if len(store.updates) != 0 {
		t.Fatalf("expected no high-water update for future event, got %v", store.updates)
	}
	if got := lastSeen.Load(); got != 0 {
		t.Fatalf("lastSeen advanced for future event: %d", got)
	}
}

func TestSubscriptionSinceIgnoresFutureHighWaterMark(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	lookback := 5 * time.Minute
	overlap := 30 * time.Second
	maxFutureSkew := 10 * time.Minute

	got, used := subscriptionSince(now, now.Add(time.Hour).Unix(), lookback, overlap, 24*time.Hour, maxFutureSkew)
	want := now.Add(-lookback).Unix()
	if used {
		t.Fatal("expected poisoned future high-water mark to be ignored")
	}
	if got != want {
		t.Fatalf("expected lookback timestamp %d, got %d", want, got)
	}
}

func TestSubscriptionSinceCatchesUpFromCheckpointOlderThanLookback(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	hwm := now.Add(-time.Hour).Unix()
	overlap := 30 * time.Second

	got, used := subscriptionSince(now, hwm, 5*time.Minute, overlap, 24*time.Hour, 10*time.Minute)
	if !used {
		t.Fatal("expected plausible high-water mark to be used")
	}
	want := hwm - int64(overlap/time.Second)
	if got != want {
		t.Fatalf("expected overlapped high-water timestamp %d, got %d", want, got)
	}
}

func TestSubscriptionSinceBoundsAncientCheckpointCatchup(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	got, used := subscriptionSince(now, now.Add(-48*time.Hour).Unix(), 5*time.Minute, 30*time.Second, 24*time.Hour, 10*time.Minute)
	if !used {
		t.Fatal("expected old high-water mark to be used with a catch-up bound")
	}
	want := now.Add(-24 * time.Hour).Unix()
	if got != want {
		t.Fatalf("bounded catch-up timestamp = %d, want %d", got, want)
	}
}

func TestSubscriptionSinceClampsPlausibleFutureCheckpoint(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	overlap := 30 * time.Second
	got, used := subscriptionSince(now, now.Add(5*time.Minute).Unix(), 5*time.Minute, overlap, 24*time.Hour, 10*time.Minute)
	if !used {
		t.Fatal("expected plausible future high-water mark to be recovered")
	}
	want := now.Unix() - int64(overlap/time.Second)
	if got != want {
		t.Fatalf("future checkpoint recovery timestamp = %d, want %d", got, want)
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

func TestNIP59GiftWrapOpenerDecryptsAndVerifiesRumor(t *testing.T) {
	ctx := context.Background()
	sender := keyer.NewPlainKeySigner(nostr.Generate())
	recipient := keyer.NewPlainKeySigner(nostr.Generate())
	senderPubkey, err := sender.GetPublicKey(ctx)
	if err != nil {
		t.Fatalf("sender public key: %v", err)
	}
	recipientPubkey, err := recipient.GetPublicKey(ctx)
	if err != nil {
		t.Fatalf("recipient public key: %v", err)
	}
	rumor := nostr.Event{
		PubKey:    senderPubkey,
		Kind:      25910,
		CreatedAt: nostr.Now(),
		Content:   `{"jsonrpc":"2.0","id":"req-1","method":"review/request"}`,
	}
	rumor.ID = rumor.GetID()
	wrapper, err := nip59.GiftWrap(
		rumor,
		recipientPubkey,
		func(plaintext string) (string, error) {
			return sender.Encrypt(ctx, plaintext, recipientPubkey)
		},
		func(event *nostr.Event) error {
			return sender.SignEvent(ctx, event)
		},
		nil,
	)
	if err != nil {
		t.Fatalf("gift wrap: %v", err)
	}

	opened, err := NewNIP59GiftWrapOpener(recipient).OpenGiftWrap(ctx, wrapper)
	if err != nil {
		t.Fatalf("open gift wrap: %v", err)
	}
	if opened.ID != rumor.ID || opened.Kind != rumor.Kind || opened.Content != rumor.Content {
		t.Fatalf("opened rumor = %#v, want %#v", opened, rumor)
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
