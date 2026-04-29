// Package ratelimit provides rate limiting with database persistence.
// Supports sliding window rate limiting for per-user or per-resource limits.
package ratelimit

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// Store defines the database interface for rate limit persistence.
type Store interface {
	// GetRateLimitCount returns the count of events for a key within a time window.
	GetRateLimitCount(ctx context.Context, key string, windowStart int64) (int, error)
	// IncrementRateLimit records an event for a key at the current time.
	IncrementRateLimit(ctx context.Context, key string, timestamp int64) error
	// CheckAndIncrementRateLimit atomically checks the count and increments if under limit.
	// Returns (count_before_increment, incremented, error).
	// If count < maxRequests, it increments and returns (count, true, nil).
	// If count >= maxRequests, it does not increment and returns (count, false, nil).
	CheckAndIncrementRateLimit(ctx context.Context, key string, windowStart int64, timestamp int64, maxRequests int) (int, bool, error)
	// CleanupOldRateLimits removes entries older than the given timestamp.
	CleanupOldRateLimits(ctx context.Context, olderThan int64) (int64, error)
}

// Config configures a rate limiter.
type Config struct {
	// Window is the sliding window duration.
	Window time.Duration
	// MaxRequests is the maximum requests allowed per window.
	MaxRequests int
	// KeyPrefix is prepended to all keys (for namespacing).
	KeyPrefix string
}

// Limiter implements sliding window rate limiting.
type Limiter struct {
	cfg   Config
	store Store

	// In-memory cache for hot paths (optional optimization)
	mu    sync.RWMutex
	cache map[string]*cacheEntry
}

type cacheEntry struct {
	count     int
	expiresAt time.Time
}

// New creates a new rate limiter.
func New(cfg Config, store Store) *Limiter {
	if cfg.Window == 0 {
		cfg.Window = time.Hour
	}
	if cfg.MaxRequests == 0 {
		cfg.MaxRequests = 100
	}
	return &Limiter{
		cfg:   cfg,
		store: store,
		cache: make(map[string]*cacheEntry),
	}
}

// Result contains the outcome of a rate limit check.
type Result struct {
	Allowed   bool
	Remaining int
	ResetAt   time.Time
}

// Check checks if a request is allowed for the given key.
// Returns whether the request is allowed and remaining quota.
func (l *Limiter) Check(ctx context.Context, key string) (Result, error) {
	l.evictExpiredCache()

	fullKey := l.cfg.KeyPrefix + key
	now := time.Now()
	windowStart := now.Add(-l.cfg.Window).Unix()

	// Check cache first (for hot paths)
	if entry, ok := l.getCached(fullKey); ok && entry.expiresAt.After(now) {
		remaining := l.cfg.MaxRequests - entry.count
		if remaining <= 0 {
			return Result{
				Allowed:   false,
				Remaining: 0,
				ResetAt:   entry.expiresAt,
			}, nil
		}
	}

	// Query database for current count
	count, err := l.store.GetRateLimitCount(ctx, fullKey, windowStart)
	if err != nil {
		return Result{}, fmt.Errorf("get rate limit count: %w", err)
	}

	remaining := l.cfg.MaxRequests - count
	resetAt := now.Add(l.cfg.Window)

	// Update cache
	l.setCache(fullKey, count, resetAt)

	return Result{
		Allowed:   remaining > 0,
		Remaining: max(0, remaining),
		ResetAt:   resetAt,
	}, nil
}

// Allow checks and increments the counter if allowed.
// This is the typical use case - check and consume in one call.
func (l *Limiter) Allow(ctx context.Context, key string) (Result, error) {
	l.evictExpiredCache()

	fullKey := l.cfg.KeyPrefix + key
	now := time.Now()
	windowStart := now.Add(-l.cfg.Window).Unix()
	timestamp := now.Unix()

	// Atomically check and increment
	count, incremented, err := l.store.CheckAndIncrementRateLimit(ctx, fullKey, windowStart, timestamp, l.cfg.MaxRequests)
	if err != nil {
		return Result{}, fmt.Errorf("check and increment rate limit: %w", err)
	}

	resetAt := now.Add(l.cfg.Window)

	if incremented {
		// Request was allowed and counter was incremented
		remaining := l.cfg.MaxRequests - count - 1
		l.setCache(fullKey, count+1, resetAt)
		return Result{
			Allowed:   true,
			Remaining: max(0, remaining),
			ResetAt:   resetAt,
		}, nil
	}

	// Request was denied - limit exceeded
	l.setCache(fullKey, count, resetAt)
	return Result{
		Allowed:   false,
		Remaining: 0,
		ResetAt:   resetAt,
	}, nil
}

// Cleanup removes old rate limit entries from the database.
// Should be called periodically (e.g., every hour).
func (l *Limiter) Cleanup(ctx context.Context) (int64, error) {
	olderThan := time.Now().Add(-l.cfg.Window * 2).Unix()
	return l.store.CleanupOldRateLimits(ctx, olderThan)
}

func (l *Limiter) getCached(key string) (*cacheEntry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	entry, ok := l.cache[key]
	return entry, ok
}

func (l *Limiter) setCache(key string, count int, expiresAt time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cache[key] = &cacheEntry{count: count, expiresAt: expiresAt}
}

func (l *Limiter) incrementCache(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry, ok := l.cache[key]; ok {
		entry.count++
	}
}

func (l *Limiter) evictExpiredCache() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for key, entry := range l.cache {
		if entry.expiresAt.Before(now) {
			delete(l.cache, key)
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// DefaultCodeChatConfig returns config for codebase chat rate limiting.
func DefaultCodeChatConfig() Config {
	return Config{
		Window:      time.Hour,
		MaxRequests: 20,
		KeyPrefix:   "codechat:",
	}
}

// DefaultMarketplaceConfig returns config for marketplace rate limiting.
func DefaultMarketplaceConfig() Config {
	return Config{
		Window:      time.Hour,
		MaxRequests: 50,
		KeyPrefix:   "marketplace:",
	}
}

// DefaultFeedbackConfig returns config for feedback submission rate limiting.
func DefaultFeedbackConfig() Config {
	return Config{
		Window:      time.Hour * 24,
		MaxRequests: 100,
		KeyPrefix:   "feedback:",
	}
}

// MemoryStore is an in-memory implementation for testing.
type MemoryStore struct {
	mu      sync.Mutex
	entries map[string][]int64 // key -> timestamps
}

// NewMemoryStore creates an in-memory rate limit store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		entries: make(map[string][]int64),
	}
}

func (m *MemoryStore) GetRateLimitCount(ctx context.Context, key string, windowStart int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	timestamps, ok := m.entries[key]
	if !ok {
		return 0, nil
	}

	count := 0
	for _, ts := range timestamps {
		if ts >= windowStart {
			count++
		}
	}
	return count, nil
}

func (m *MemoryStore) IncrementRateLimit(ctx context.Context, key string, timestamp int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.entries[key] = append(m.entries[key], timestamp)
	return nil
}

func (m *MemoryStore) CheckAndIncrementRateLimit(ctx context.Context, key string, windowStart int64, timestamp int64, maxRequests int) (int, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Count current requests in window
	timestamps, ok := m.entries[key]
	count := 0
	if ok {
		for _, ts := range timestamps {
			if ts >= windowStart {
				count++
			}
		}
	}

	// Check if under limit
	if count < maxRequests {
		// Increment
		m.entries[key] = append(m.entries[key], timestamp)
		return count, true, nil
	}

	// Over limit, do not increment
	return count, false, nil
}

func (m *MemoryStore) CleanupOldRateLimits(ctx context.Context, olderThan int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var removed int64
	for key, timestamps := range m.entries {
		var kept []int64
		for _, ts := range timestamps {
			if ts >= olderThan {
				kept = append(kept, ts)
			} else {
				removed++
			}
		}
		if len(kept) == 0 {
			delete(m.entries, key)
		} else {
			m.entries[key] = kept
		}
	}
	return removed, nil
}

// SQLStore implements Store using a SQL database.
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore creates a SQL-backed rate limit store.
func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db}
}

func (s *SQLStore) GetRateLimitCount(ctx context.Context, key string, windowStart int64) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rate_limits
		WHERE key = ? AND timestamp >= ?
	`, key, windowStart).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *SQLStore) IncrementRateLimit(ctx context.Context, key string, timestamp int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO rate_limits (key, timestamp) VALUES (?, ?)
	`, key, timestamp)
	return err
}

func (s *SQLStore) CheckAndIncrementRateLimit(ctx context.Context, key string, windowStart int64, timestamp int64, maxRequests int) (int, bool, error) {
	// Begin transaction for atomic check-and-increment
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() // Rollback if we don't commit

	// Count current requests in window
	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rate_limits
		WHERE key = ? AND timestamp >= ?
	`, key, windowStart).Scan(&count)
	if err != nil {
		return 0, false, fmt.Errorf("count rate limits: %w", err)
	}

	// Check if under limit
	if count < maxRequests {
		// Insert new entry
		_, err = tx.ExecContext(ctx, `
			INSERT INTO rate_limits (key, timestamp) VALUES (?, ?)
		`, key, timestamp)
		if err != nil {
			return 0, false, fmt.Errorf("insert rate limit: %w", err)
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			return 0, false, fmt.Errorf("commit transaction: %w", err)
		}

		return count, true, nil
	}

	// Over limit - rollback (no insert)
	tx.Rollback()
	return count, false, nil
}

func (s *SQLStore) CleanupOldRateLimits(ctx context.Context, olderThan int64) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM rate_limits WHERE timestamp < ?
	`, olderThan)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
