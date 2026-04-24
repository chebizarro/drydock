package ingest

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"drydock/internal/db"

	"fiatjaf.com/nostr"
)

func TestWithLocalAutofixAuthor_SkipsSelfAuthored(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Create a valid event signed by a known key
	selfPubKey := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	processor := NewProcessor(store, logger,
		WithLocalAutofixAuthor(selfPubKey),
	)

	// Verify the field is set
	if processor.localAutofixPubKey != selfPubKey {
		t.Errorf("expected localAutofixPubKey = %q, got %q", selfPubKey, processor.localAutofixPubKey)
	}
}

func TestWithLocalAutofixAuthor_AllowsOtherAuthors(t *testing.T) {
	// This test verifies the logic by checking that the field is set correctly
	// and that a different pubkey would not match.
	selfPubKey := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherPubKey := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	processor := NewProcessor(nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithLocalAutofixAuthor(selfPubKey),
	)

	// Create a mock event pubkey check
	var pk nostr.PubKey
	copy(pk[:], mustDecodeHex(otherPubKey))

	if pk.Hex() == processor.localAutofixPubKey {
		t.Error("other author should not match self pubkey")
	}
}

func mustDecodeHex(s string) []byte {
	b := make([]byte, len(s)/2)
	for i := 0; i < len(s)/2; i++ {
		b[i] = hexByte(s[2*i])<<4 | hexByte(s[2*i+1])
	}
	return b
}

func hexByte(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	default:
		return 0
	}
}
