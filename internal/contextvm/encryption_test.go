package contextvm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
)

func TestGiftWrapRoundTripEncryptOpenVerify(t *testing.T) {
	ctx := context.Background()
	sender := keyer.NewPlainKeySigner(nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000001"))
	recipient := keyer.NewPlainKeySigner(nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000002"))
	recipientPK, _ := recipient.GetPublicKey(ctx)

	plaintext := `{"method":"context.get","source":"secret source snippet"}`
	wrapper, inner, err := GiftWrap(ctx, sender, recipientPK, plaintext, nostr.Tags{{"t", "drydock-contextvm"}})
	if err != nil {
		t.Fatalf("GiftWrap: %v", err)
	}
	if wrapper.Kind != KindGiftWrap {
		t.Fatalf("wrapper kind = %d, want %d", wrapper.Kind, KindGiftWrap)
	}
	if inner.Kind != KindContextVM {
		t.Fatalf("inner kind = %d, want %d", inner.Kind, KindContextVM)
	}
	if wrapper.PubKey == inner.PubKey {
		t.Fatal("wrapper must be signed by an ephemeral sender key, not the inner author")
	}
	if strings.Contains(wrapper.Content, "secret source snippet") || strings.Contains(wrapper.String(), "secret source snippet") {
		t.Fatal("gift wrap leaked plaintext in wrapper")
	}

	opened, err := OpenGiftWrap(ctx, recipient, wrapper)
	if err != nil {
		t.Fatalf("OpenGiftWrap: %v", err)
	}
	if opened.ID != inner.ID || opened.Content != plaintext || opened.PubKey != inner.PubKey {
		t.Fatalf("opened inner mismatch: got id=%s pubkey=%s content=%q", opened.ID.Hex(), opened.PubKey.Hex(), opened.Content)
	}
	if !opened.CheckID() || !opened.VerifySignature() {
		t.Fatal("opened inner event failed integrity verification")
	}
}

func TestOpenGiftWrapRejectsRecipientMismatch(t *testing.T) {
	ctx := context.Background()
	sender := keyer.NewPlainKeySigner(nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000001"))
	recipient := keyer.NewPlainKeySigner(nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000002"))
	other := keyer.NewPlainKeySigner(nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000003"))
	recipientPK, _ := recipient.GetPublicKey(ctx)

	wrapper, _, err := GiftWrap(ctx, sender, recipientPK, "private", nil)
	if err != nil {
		t.Fatalf("GiftWrap: %v", err)
	}
	_, err = OpenGiftWrap(ctx, other, wrapper)
	if !errors.Is(err, ErrRecipientMismatch) {
		t.Fatalf("expected ErrRecipientMismatch, got %v", err)
	}
}

func TestOpenGiftWrapRejectsTamperedInnerEvent(t *testing.T) {
	ctx := context.Background()
	sender := keyer.NewPlainKeySigner(nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000001"))
	recipient := keyer.NewPlainKeySigner(nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000002"))
	recipientPK, _ := recipient.GetPublicKey(ctx)

	_, inner, err := GiftWrap(ctx, sender, recipientPK, "original", nil)
	if err != nil {
		t.Fatalf("GiftWrap: %v", err)
	}
	inner.Content = "tampered after signing"
	encoded, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal tampered inner: %v", err)
	}

	ephemeral := keyer.NewPlainKeySigner(nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000004"))
	ciphertext, err := ephemeral.Encrypt(ctx, string(encoded), recipientPK)
	if err != nil {
		t.Fatalf("encrypt tampered inner: %v", err)
	}
	wrapper := nostr.Event{
		Kind:      KindGiftWrap,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"p", recipientPK.Hex()}},
		Content:   ciphertext,
	}
	if err := ephemeral.SignEvent(ctx, &wrapper); err != nil {
		t.Fatalf("sign wrapper: %v", err)
	}

	_, err = OpenGiftWrap(ctx, recipient, wrapper)
	if !errors.Is(err, ErrInvalidInnerEvent) {
		t.Fatalf("expected ErrInvalidInnerEvent, got %v", err)
	}
}
