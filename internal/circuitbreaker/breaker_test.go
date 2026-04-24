package circuitbreaker

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBreaker_InitialState(t *testing.T) {
	b := New(DefaultConfig())
	if b.State() != StateClosed {
		t.Errorf("expected initial state to be closed, got %v", b.State())
	}
}

func TestBreaker_AllowWhenClosed(t *testing.T) {
	b := New(DefaultConfig())
	if !b.Allow() {
		t.Error("expected Allow to return true when closed")
	}
}

func TestBreaker_OpensAfterFailures(t *testing.T) {
	cfg := Config{
		FailureThreshold: 3,
		Timeout:          10 * time.Second,
	}
	b := New(cfg)

	// Record failures up to threshold
	for i := 0; i < 3; i++ {
		b.Allow()
		b.RecordFailure()
	}

	if b.State() != StateOpen {
		t.Errorf("expected state to be open after %d failures, got %v", cfg.FailureThreshold, b.State())
	}

	if b.Allow() {
		t.Error("expected Allow to return false when open")
	}
}

func TestBreaker_SuccessResetsFailures(t *testing.T) {
	cfg := Config{
		FailureThreshold: 3,
		Timeout:          10 * time.Second,
	}
	b := New(cfg)

	// Record 2 failures (below threshold)
	b.Allow()
	b.RecordFailure()
	b.Allow()
	b.RecordFailure()

	// Record a success - should reset failure count
	b.Allow()
	b.RecordSuccess()

	// Now 2 more failures should not open the circuit
	b.Allow()
	b.RecordFailure()
	b.Allow()
	b.RecordFailure()

	if b.State() != StateClosed {
		t.Errorf("expected state to be closed after success reset, got %v", b.State())
	}
}

func TestBreaker_TransitionsToHalfOpenAfterTimeout(t *testing.T) {
	cfg := Config{
		FailureThreshold: 2,
		Timeout:          50 * time.Millisecond,
	}
	b := New(cfg)

	// Open the circuit
	b.Allow()
	b.RecordFailure()
	b.Allow()
	b.RecordFailure()

	if b.State() != StateOpen {
		t.Fatalf("expected state to be open, got %v", b.State())
	}

	// Wait for timeout
	time.Sleep(60 * time.Millisecond)

	// Should now be half-open
	if b.State() != StateHalfOpen {
		t.Errorf("expected state to be half-open after timeout, got %v", b.State())
	}
}

func TestBreaker_HalfOpenClosesAfterSuccesses(t *testing.T) {
	cfg := Config{
		FailureThreshold:    2,
		SuccessThreshold:    2,
		Timeout:             50 * time.Millisecond,
		MaxHalfOpenRequests: 3,
	}
	b := New(cfg)

	// Open the circuit
	b.Allow()
	b.RecordFailure()
	b.Allow()
	b.RecordFailure()

	// Wait for half-open
	time.Sleep(60 * time.Millisecond)

	// Record successes to close
	b.Allow()
	b.RecordSuccess()
	b.Allow()
	b.RecordSuccess()

	if b.State() != StateClosed {
		t.Errorf("expected state to be closed after successes in half-open, got %v", b.State())
	}
}

func TestBreaker_HalfOpenReopensOnFailure(t *testing.T) {
	cfg := Config{
		FailureThreshold: 2,
		Timeout:          50 * time.Millisecond,
	}
	b := New(cfg)

	// Open the circuit
	b.Allow()
	b.RecordFailure()
	b.Allow()
	b.RecordFailure()

	// Wait for half-open
	time.Sleep(60 * time.Millisecond)

	// Allow one request
	if !b.Allow() {
		t.Error("expected Allow to return true in half-open")
	}

	// Fail - should reopen
	b.RecordFailure()

	if b.State() != StateOpen {
		t.Errorf("expected state to be open after failure in half-open, got %v", b.State())
	}
}

func TestBreaker_LimitsHalfOpenRequests(t *testing.T) {
	cfg := Config{
		FailureThreshold:    2,
		Timeout:             50 * time.Millisecond,
		MaxHalfOpenRequests: 1,
	}
	b := New(cfg)

	// Open the circuit
	b.Allow()
	b.RecordFailure()
	b.Allow()
	b.RecordFailure()

	// Wait for half-open
	time.Sleep(60 * time.Millisecond)

	// First request allowed
	if !b.Allow() {
		t.Error("expected first request to be allowed in half-open")
	}

	// Second request should be blocked
	if b.Allow() {
		t.Error("expected second request to be blocked in half-open")
	}
}

func TestBreaker_Execute(t *testing.T) {
	b := New(DefaultConfig())

	// Successful execution
	err := b.Execute(context.Background(), func(ctx context.Context) error {
		return nil
	})
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	// Failed execution
	testErr := errors.New("test error")
	err = b.Execute(context.Background(), func(ctx context.Context) error {
		return testErr
	})
	if err != testErr {
		t.Errorf("expected test error, got %v", err)
	}
}

func TestBreaker_ExecuteReturnsCircuitOpen(t *testing.T) {
	cfg := Config{
		FailureThreshold: 2,
		Timeout:          10 * time.Second,
	}
	b := New(cfg)

	// Open the circuit
	b.Allow()
	b.RecordFailure()
	b.Allow()
	b.RecordFailure()

	// Execute should return ErrCircuitOpen
	err := b.Execute(context.Background(), func(ctx context.Context) error {
		t.Error("function should not be called when circuit is open")
		return nil
	})

	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("expected ErrCircuitOpen, got %v", err)
	}
}

func TestBreaker_ExecuteWithFallback(t *testing.T) {
	cfg := Config{
		FailureThreshold: 2,
		Timeout:          10 * time.Second,
	}
	b := New(cfg)

	fallbackCalled := false
	fallback := func(ctx context.Context) error {
		fallbackCalled = true
		return nil
	}

	// Open the circuit
	b.Allow()
	b.RecordFailure()
	b.Allow()
	b.RecordFailure()

	// ExecuteWithFallback should call fallback
	err := b.ExecuteWithFallback(context.Background(), func(ctx context.Context) error {
		t.Error("primary function should not be called when circuit is open")
		return nil
	}, fallback)

	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if !fallbackCalled {
		t.Error("expected fallback to be called")
	}
}

func TestBreaker_ExecuteWithFallbackOnError(t *testing.T) {
	b := New(DefaultConfig())

	primaryCalled := false
	fallbackCalled := false

	testErr := errors.New("test error")

	err := b.ExecuteWithFallback(context.Background(), func(ctx context.Context) error {
		primaryCalled = true
		return testErr
	}, func(ctx context.Context) error {
		fallbackCalled = true
		return nil
	})

	if err != nil {
		t.Errorf("expected no error from fallback, got %v", err)
	}
	if !primaryCalled {
		t.Error("expected primary to be called")
	}
	if !fallbackCalled {
		t.Error("expected fallback to be called after primary error")
	}
}

func TestBreaker_Reset(t *testing.T) {
	cfg := Config{
		FailureThreshold: 2,
		Timeout:          10 * time.Second,
	}
	b := New(cfg)

	// Open the circuit
	b.Allow()
	b.RecordFailure()
	b.Allow()
	b.RecordFailure()

	if b.State() != StateOpen {
		t.Fatalf("expected state to be open, got %v", b.State())
	}

	// Reset
	b.Reset()

	if b.State() != StateClosed {
		t.Errorf("expected state to be closed after reset, got %v", b.State())
	}

	// Should allow requests again
	if !b.Allow() {
		t.Error("expected Allow to return true after reset")
	}
}

func TestBreaker_Stats(t *testing.T) {
	b := New(DefaultConfig())

	b.Allow()
	b.RecordFailure()
	b.Allow()
	b.RecordSuccess()
	b.Allow()
	b.RecordFailure()

	stats := b.Stats()

	if stats.State != StateClosed {
		t.Errorf("expected state closed, got %v", stats.State)
	}
	if stats.Failures != 1 { // success reset one failure, then one more
		t.Errorf("expected 1 failure, got %d", stats.Failures)
	}
}

func TestStateString(t *testing.T) {
	tests := []struct {
		state    State
		expected string
	}{
		{StateClosed, "closed"},
		{StateOpen, "open"},
		{StateHalfOpen, "half-open"},
		{State(99), "unknown"},
	}

	for _, tc := range tests {
		if tc.state.String() != tc.expected {
			t.Errorf("expected %q, got %q", tc.expected, tc.state.String())
		}
	}
}
