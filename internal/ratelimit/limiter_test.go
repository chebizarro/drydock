package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimiter_Allow(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	limiter := New(Config{
		Window:      time.Minute,
		MaxRequests: 3,
		KeyPrefix:   "test:",
	}, store)

	key := "user123"

	// First 3 requests should be allowed
	for i := 0; i < 3; i++ {
		result, err := limiter.Allow(ctx, key)
		if err != nil {
			t.Fatalf("Allow failed: %v", err)
		}
		if !result.Allowed {
			t.Errorf("request %d should be allowed", i+1)
		}
		if result.Remaining != 2-i {
			t.Errorf("request %d: remaining = %d, want %d", i+1, result.Remaining, 2-i)
		}
	}

	// 4th request should be denied
	result, err := limiter.Allow(ctx, key)
	if err != nil {
		t.Fatalf("Allow failed: %v", err)
	}
	if result.Allowed {
		t.Error("4th request should be denied")
	}
	if result.Remaining != 0 {
		t.Errorf("remaining = %d, want 0", result.Remaining)
	}
}

func TestLimiter_Check_DoesNotConsume(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	limiter := New(Config{
		Window:      time.Minute,
		MaxRequests: 2,
		KeyPrefix:   "test:",
	}, store)

	key := "user456"

	// Check multiple times - should not consume quota
	for i := 0; i < 5; i++ {
		result, err := limiter.Check(ctx, key)
		if err != nil {
			t.Fatalf("Check failed: %v", err)
		}
		if !result.Allowed {
			t.Errorf("check %d should be allowed (check doesn't consume)", i+1)
		}
		if result.Remaining != 2 {
			t.Errorf("check %d: remaining = %d, want 2", i+1, result.Remaining)
		}
	}
}

func TestLimiter_DifferentKeys(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	limiter := New(Config{
		Window:      time.Minute,
		MaxRequests: 2,
		KeyPrefix:   "test:",
	}, store)

	// User1 uses their quota
	for i := 0; i < 2; i++ {
		limiter.Allow(ctx, "user1")
	}

	// User1 should be blocked
	result1, _ := limiter.Allow(ctx, "user1")
	if result1.Allowed {
		t.Error("user1 should be blocked")
	}

	// User2 should still have quota
	result2, _ := limiter.Allow(ctx, "user2")
	if !result2.Allowed {
		t.Error("user2 should be allowed")
	}
}

func TestLimiter_Cleanup(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	limiter := New(Config{
		Window:      time.Second, // Very short window for testing
		MaxRequests: 10,
		KeyPrefix:   "test:",
	}, store)

	// Add some entries
	limiter.Allow(ctx, "user1")
	limiter.Allow(ctx, "user2")
	limiter.Allow(ctx, "user1")

	// Wait for entries to age out
	time.Sleep(3 * time.Second)

	// Cleanup
	removed, err := limiter.Cleanup(ctx)
	if err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3", removed)
	}

	// User1 should have full quota again
	result, _ := limiter.Check(ctx, "user1")
	if result.Remaining != 10 {
		t.Errorf("remaining = %d, want 10 after cleanup", result.Remaining)
	}
}

func TestMemoryStore_GetRateLimitCount(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	now := time.Now().Unix()
	oldTime := now - 3600 // 1 hour ago

	// Add some entries
	store.IncrementRateLimit(ctx, "key1", now)
	store.IncrementRateLimit(ctx, "key1", now-10)
	store.IncrementRateLimit(ctx, "key1", oldTime) // Outside window

	// Count within last 30 minutes
	windowStart := now - 1800
	count, err := store.GetRateLimitCount(ctx, "key1", windowStart)
	if err != nil {
		t.Fatalf("GetRateLimitCount failed: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 (excluding old entry)", count)
	}
}

func TestDefaultConfigs(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{"CodeChat", DefaultCodeChatConfig()},
		{"Marketplace", DefaultMarketplaceConfig()},
		{"Feedback", DefaultFeedbackConfig()},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.config.Window == 0 {
				t.Error("Window should not be zero")
			}
			if tc.config.MaxRequests == 0 {
				t.Error("MaxRequests should not be zero")
			}
			if tc.config.KeyPrefix == "" {
				t.Error("KeyPrefix should not be empty")
			}
		})
	}
}

func TestResult_ResetAt(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	limiter := New(Config{
		Window:      time.Hour,
		MaxRequests: 1,
		KeyPrefix:   "test:",
	}, store)

	result, _ := limiter.Allow(ctx, "user1")

	// ResetAt should be approximately 1 hour from now
	expectedReset := time.Now().Add(time.Hour)
	diff := result.ResetAt.Sub(expectedReset)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("ResetAt = %v, expected around %v", result.ResetAt, expectedReset)
	}
}

func TestLimiter_EvictExpiredCache(t *testing.T) {
	limiter := New(Config{
		Window:      time.Minute,
		MaxRequests: 10,
		KeyPrefix:   "test:",
	}, NewMemoryStore())

	limiter.mu.Lock()
	limiter.cache["expired"] = &cacheEntry{count: 1, expiresAt: time.Now().Add(-time.Second)}
	limiter.cache["active"] = &cacheEntry{count: 1, expiresAt: time.Now().Add(time.Minute)}
	limiter.mu.Unlock()

	limiter.evictExpiredCache()

	limiter.mu.RLock()
	defer limiter.mu.RUnlock()
	if _, ok := limiter.cache["expired"]; ok {
		t.Fatal("expected expired cache entry to be evicted")
	}
	if _, ok := limiter.cache["active"]; !ok {
		t.Fatal("expected active cache entry to remain")
	}
}

// TestLimiter_ConcurrentAllow tests that concurrent calls to Allow()
// properly enforce the rate limit without allowing excess requests.
func TestLimiter_ConcurrentAllow(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	maxRequests := 10
	limiter := New(Config{
		Window:      time.Minute,
		MaxRequests: maxRequests,
		KeyPrefix:   "test:",
	}, store)

	key := "concurrent_user"
	concurrency := 50 // Many more than the limit

	var allowedCount atomic.Int32
	var deniedCount atomic.Int32
	var wg sync.WaitGroup

	// Spawn many concurrent requests
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := limiter.Allow(ctx, key)
			if err != nil {
				t.Errorf("Allow failed: %v", err)
				return
			}
			if result.Allowed {
				allowedCount.Add(1)
			} else {
				deniedCount.Add(1)
			}
		}()
	}

	wg.Wait()

	allowed := int(allowedCount.Load())
	denied := int(deniedCount.Load())

	// Verify exactly maxRequests were allowed
	if allowed != maxRequests {
		t.Errorf("allowed = %d, want exactly %d (denied = %d)", allowed, maxRequests, denied)
	}

	// Verify remaining were denied
	if denied != concurrency-maxRequests {
		t.Errorf("denied = %d, want %d", denied, concurrency-maxRequests)
	}

	// Double-check with store count
	count, err := store.GetRateLimitCount(ctx, "test:"+key, time.Now().Add(-time.Minute).Unix())
	if err != nil {
		t.Fatalf("GetRateLimitCount failed: %v", err)
	}
	if count != maxRequests {
		t.Errorf("store count = %d, want exactly %d", count, maxRequests)
	}
}
