package contextbuilder

import (
	"context"
	"log/slog"
	"sync"
	"time"

	tiktoken "github.com/pkoukk/tiktoken-go"
)

// TiktokenCounter uses OpenAI's tiktoken encoding for accurate token counting.
type TiktokenCounter struct {
	enc *tiktoken.Tiktoken
}

var (
	defaultTiktokenOnce    sync.Once
	defaultTiktokenCounter TokenCounter
)

// NewTiktokenCounter creates a counter using the specified tiktoken encoding.
// Common encodings: "cl100k_base" (GPT-4/Claude), "o200k_base" (GPT-4o).
// Returns a fallback ApproxTokenCounter if the encoding cannot be loaded.
func NewTiktokenCounter(encoding string) TokenCounter {
	enc, err := loadEncodingWithTimeout(encoding, 5*time.Second)
	if err != nil {
		slog.Warn("tiktoken encoding unavailable, using approximate counter",
			"encoding", encoding, "error", err)
		return ApproxTokenCounter{}
	}
	return &TiktokenCounter{enc: enc}
}

// DefaultTiktokenCounter returns a singleton cl100k_base counter.
// Falls back to ApproxTokenCounter if tiktoken data is unavailable (e.g. no network).
func DefaultTiktokenCounter() TokenCounter {
	defaultTiktokenOnce.Do(func() {
		defaultTiktokenCounter = NewTiktokenCounter("cl100k_base")
	})
	return defaultTiktokenCounter
}

func (t *TiktokenCounter) Count(text string) int {
	if text == "" {
		return 0
	}
	tokens := t.enc.Encode(text, nil, nil)
	return len(tokens)
}

// loadEncodingWithTimeout attempts to load a tiktoken encoding with a deadline.
// The encoding data may need to be downloaded from the internet on first use;
// this prevents blocking startup indefinitely if the network is unavailable.
func loadEncodingWithTimeout(encoding string, timeout time.Duration) (*tiktoken.Tiktoken, error) {
	type result struct {
		enc *tiktoken.Tiktoken
		err error
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ch := make(chan result, 1)
	go func() {
		enc, err := tiktoken.GetEncoding(encoding)
		ch <- result{enc, err}
	}()

	select {
	case r := <-ch:
		return r.enc, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
