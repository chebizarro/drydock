package contextvm

import (
	"context"
	"testing"

	"fiatjaf.com/nostr"
)

type fakeCipher struct{}

func (fakeCipher) Encrypt(ctx context.Context, plaintext string, recipient nostr.PubKey) (string, error) {
	return "sealed:" + recipient.Hex() + ":" + plaintext, nil
}

func (fakeCipher) Decrypt(ctx context.Context, ciphertext string, sender nostr.PubKey) (string, error) {
	return "opened:" + sender.Hex() + ":" + ciphertext, nil
}

func TestGiftWrapUsesKind1059(t *testing.T) {
	ctx := context.Background()
	recipient, _ := newTestSigner(2).GetPublicKey(ctx)
	evt, err := GiftWrap(ctx, fakeCipher{}, "hello", recipient)
	if err != nil {
		t.Fatalf("gift wrap: %v", err)
	}
	if evt.Kind != KindGiftWrap {
		t.Fatalf("kind = %d", evt.Kind)
	}
	if !evt.Tags.ContainsAny("p", []string{recipient.Hex()}) {
		t.Fatalf("missing p tag: %+v", evt.Tags)
	}
	if evt.Content == "hello" || evt.Content == "" {
		t.Fatalf("content was not encrypted: %q", evt.Content)
	}
}
