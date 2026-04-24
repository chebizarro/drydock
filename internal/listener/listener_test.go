package listener

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

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
		4:     true,  // NIP-04 DMs
		14:    true,  // NIP-17 sealed DMs
		31650: true,  // IDE workspace session
		1651:  true,  // IDE review request
		1653:  true,  // IDE fix request
		30620: true,  // Marketplace: reviewer profile
		1660:  true,  // Marketplace: review assignment
		1661:  true,  // Marketplace: assignment acceptance
		1662:  true,  // Marketplace: assignment rejection
		1663:  true,  // Marketplace: review feedback
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
}

func (f *fakeProcessor) ProcessEvent(_ context.Context, event nostr.Event, _ string) error {
	f.events = append(f.events, event)
	return nil
}

func TestRunExitsWhenNoRelays(t *testing.T) {
	proc := &fakeProcessor{}
	svc := New(Config{
		Relays:          nil, // no relays
		LookbackMinutes: 5,
	}, proc, nil)

	// Use a logger that doesn't panic on nil (create minimal one)
	svc.logger = noopLogger()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- svc.Run(ctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancelled with no relays")
	}
}

func TestNewAppliesOptions(t *testing.T) {
	proc := &fakeProcessor{}
	pool := nostr.NewPool(nostr.PoolOptions{})

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
