package contextbuilder

import (
	"testing"
)

func TestTiktokenCounterFallsBackOnBadEncoding(t *testing.T) {
	counter := NewTiktokenCounter("nonexistent-encoding-xyz")
	// Should fall back to ApproxTokenCounter (not panic)
	count := counter.Count("Hello")
	if count < 1 {
		t.Fatalf("fallback counter returned %d", count)
	}
}

func TestDefaultTiktokenCounterNeverPanics(t *testing.T) {
	// DefaultTiktokenCounter should always return a usable counter,
	// even if tiktoken encoding data is unavailable (no network).
	counter := DefaultTiktokenCounter()
	count := counter.Count("func main() { fmt.Println(\"hello\") }")
	if count < 1 {
		t.Fatalf("counter returned %d for non-empty string", count)
	}
}

func TestTiktokenCounterEmptyString(t *testing.T) {
	counter := DefaultTiktokenCounter()
	if count := counter.Count(""); count != 0 {
		t.Fatalf("expected 0 tokens for empty string, got %d", count)
	}
}

func TestApproxTokenCounterStillWorks(t *testing.T) {
	// ApproxTokenCounter must remain usable as a fallback
	counter := ApproxTokenCounter{}
	text := "Hello, world!"
	count := counter.Count(text)
	if count < 1 {
		t.Fatalf("approx counter returned %d for %q", count, text)
	}
}
