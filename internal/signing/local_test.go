package signing

import (
	"context"
	"encoding/hex"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

func TestLocalSignerFromHex(t *testing.T) {
	sk := nostr.Generate()
	hexKey := hex.EncodeToString(sk[:])

	signer, err := NewLocalSigner(hexKey)
	if err != nil {
		t.Fatalf("NewLocalSigner from hex: %v", err)
	}

	ctx := context.Background()
	pub, err := signer.GetPublicKey(ctx)
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	expected := nostr.GetPublicKey(sk)
	if pub != expected {
		t.Fatalf("public key mismatch: got %s, want %s", pub.Hex(), expected.Hex())
	}

	// Sign and verify round-trip
	evt := &nostr.Event{
		Kind:      1,
		CreatedAt: nostr.Now(),
		Content:   "test content",
		Tags:      nostr.Tags{{"t", "test"}},
	}
	if err := signer.SignEvent(ctx, evt); err != nil {
		t.Fatalf("SignEvent: %v", err)
	}
	if !evt.VerifySignature() {
		t.Fatal("signed event failed signature verification")
	}
	if evt.PubKey != expected {
		t.Fatalf("event pubkey mismatch: got %s, want %s", evt.PubKey.Hex(), expected.Hex())
	}
}

func TestLocalSignerFromNsec(t *testing.T) {
	sk := nostr.Generate()
	nsec := nip19.EncodeNsec(sk)

	signer, err := NewLocalSigner(nsec)
	if err != nil {
		t.Fatalf("NewLocalSigner from nsec: %v", err)
	}

	ctx := context.Background()
	pub, err := signer.GetPublicKey(ctx)
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	expected := nostr.GetPublicKey(sk)
	if pub != expected {
		t.Fatalf("public key mismatch: got %s, want %s", pub.Hex(), expected.Hex())
	}
}

func TestLocalSignerRejectsInvalidInputs(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"too short hex", "abcdef"},
		{"invalid hex", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{"wrong length", "abcdef0123456789"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewLocalSigner(tc.input)
			if err == nil {
				t.Fatalf("expected error for input %q", tc.input)
			}
		})
	}
}
