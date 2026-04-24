// Package circuitbreaker implements the circuit breaker pattern for graceful degradation
// when external services are unavailable or experiencing high error rates.
//
// States:
//   - Closed: Normal operation, requests pass through
//   - Open: Service is down, requests fail fast without calling the service
//   - HalfOpen: Testing if service recovered, limited requests pass through
package circuitbreaker

import (
	"context"
	"errors"
	"sync"
	"time"
)

// State represents the current state of a circuit breaker.
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Config configures circuit breaker behavior.
type Config struct {
	// FailureThreshold is the number of failures before opening the circuit.
	// Default: 5
	FailureThreshold int

	// SuccessThreshold is the number of successes in half-open state before closing.
	// Default: 2
	SuccessThreshold int

	// Timeout is how long to wait before transitioning from open to half-open.
	// Default: 30s
	Timeout time.Duration

	// MaxHalfOpenRequests limits concurrent requests in half-open state.
	// Default: 1
	MaxHalfOpenRequests int
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		FailureThreshold:    5,
		SuccessThreshold:    2,
		Timeout:             30 * time.Second,
		MaxHalfOpenRequests: 1,
	}
}

// ErrCircuitOpen is returned when the circuit is open and requests fail fast.
var ErrCircuitOpen = errors.New("circuit breaker is open")

// Breaker implements the circuit breaker pattern.
type Breaker struct {
	cfg Config

	mu              sync.RWMutex
	state           State
	failures        int
	successes       int
	lastFailureTime time.Time
	halfOpenCount   int
}

// New creates a new circuit breaker with the given configuration.
func New(cfg Config) *Breaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.SuccessThreshold <= 0 {
		cfg.SuccessThreshold = 2
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxHalfOpenRequests <= 0 {
		cfg.MaxHalfOpenRequests = 1
	}
	return &Breaker{
		cfg:   cfg,
		state: StateClosed,
	}
}

// State returns the current state of the circuit breaker.
func (b *Breaker) State() State {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.currentState()
}

// currentState returns the effective state, checking for timeout transitions.
// Must be called with at least a read lock held.
func (b *Breaker) currentState() State {
	if b.state == StateOpen && time.Since(b.lastFailureTime) >= b.cfg.Timeout {
		return StateHalfOpen
	}
	return b.state
}

// Allow checks if a request should be allowed to proceed.
// Returns true if the request can proceed, false if the circuit is open.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	state := b.currentState()
	switch state {
	case StateClosed:
		return true
	case StateOpen:
		return false
	case StateHalfOpen:
		if b.halfOpenCount < b.cfg.MaxHalfOpenRequests {
			b.halfOpenCount++
			// Transition state if this is first allowed request
			if b.state == StateOpen {
				b.state = StateHalfOpen
			}
			return true
		}
		return false
	default:
		return false
	}
}

// RecordSuccess records a successful request.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		b.failures = 0 // reset failure count on success
	case StateHalfOpen:
		b.successes++
		b.halfOpenCount--
		if b.successes >= b.cfg.SuccessThreshold {
			b.state = StateClosed
			b.failures = 0
			b.successes = 0
		}
	case StateOpen:
		// Should not happen, but handle gracefully
		if b.currentState() == StateHalfOpen {
			b.state = StateHalfOpen
			b.successes = 1
		}
	}
}

// RecordFailure records a failed request.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures++
	b.lastFailureTime = time.Now()

	switch b.state {
	case StateClosed:
		if b.failures >= b.cfg.FailureThreshold {
			b.state = StateOpen
		}
	case StateHalfOpen:
		// Single failure in half-open state reopens the circuit
		b.state = StateOpen
		b.successes = 0
		b.halfOpenCount = 0
	case StateOpen:
		// Already open, just update the last failure time
	}
}

// Execute runs the provided function with circuit breaker protection.
// If the circuit is open, it returns ErrCircuitOpen without calling fn.
// Otherwise, it calls fn and records success/failure based on the result.
func (b *Breaker) Execute(ctx context.Context, fn func(ctx context.Context) error) error {
	if !b.Allow() {
		return ErrCircuitOpen
	}

	err := fn(ctx)
	if err != nil {
		b.RecordFailure()
		return err
	}

	b.RecordSuccess()
	return nil
}

// ExecuteWithFallback runs fn with circuit breaker protection.
// If the circuit is open or fn fails, it calls fallback instead.
func (b *Breaker) ExecuteWithFallback(ctx context.Context, fn func(ctx context.Context) error, fallback func(ctx context.Context) error) error {
	if !b.Allow() {
		return fallback(ctx)
	}

	err := fn(ctx)
	if err != nil {
		b.RecordFailure()
		return fallback(ctx)
	}

	b.RecordSuccess()
	return nil
}

// Reset resets the circuit breaker to closed state.
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = StateClosed
	b.failures = 0
	b.successes = 0
	b.halfOpenCount = 0
}

// Stats returns current statistics about the circuit breaker.
type Stats struct {
	State           State
	Failures        int
	Successes       int
	LastFailureTime time.Time
}

// Stats returns the current statistics.
func (b *Breaker) Stats() Stats {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return Stats{
		State:           b.currentState(),
		Failures:        b.failures,
		Successes:       b.successes,
		LastFailureTime: b.lastFailureTime,
	}
}
