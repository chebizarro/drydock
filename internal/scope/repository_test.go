package scope

import (
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

func TestParseRepositoryRef(t *testing.T) {
	owner := nostr.GetPublicKey(nostr.Generate())
	ref, err := ParseRepositoryRef(" 30617:" + strings.ToUpper(owner.Hex()) + ":repo-one ")
	if err != nil {
		t.Fatalf("ParseRepositoryRef: %v", err)
	}
	if ref.Address != "30617:"+owner.Hex()+":repo-one" {
		t.Fatalf("Address = %q", ref.Address)
	}
	if ref.RepositoryID != owner.Hex()+":repo-one" || ref.OwnerPubkey != owner.Hex() || ref.Identifier != "repo-one" {
		t.Fatalf("unexpected reference: %#v", ref)
	}
}

func TestParseRepositoryRefRejectsMalformedAddresses(t *testing.T) {
	owner := nostr.GetPublicKey(nostr.Generate())
	for _, address := range []string{
		"",
		"30617:" + owner.Hex(),
		"30618:" + owner.Hex() + ":repo",
		"30617:not-a-key:repo",
		"30617:" + owner.Hex() + ":",
		"30617:" + owner.Hex() + ":repo:extra",
	} {
		t.Run(address, func(t *testing.T) {
			if _, err := ParseRepositoryRef(address); err == nil {
				t.Fatalf("ParseRepositoryRef(%q) succeeded", address)
			}
		})
	}
}

func TestParsePubkeyAcceptsHexAndNpub(t *testing.T) {
	pubkey := nostr.GetPublicKey(nostr.Generate())
	for _, encoded := range []string{strings.ToUpper(pubkey.Hex()), nip19.EncodeNpub(pubkey)} {
		got, err := ParsePubkey(encoded)
		if err != nil {
			t.Fatalf("ParsePubkey(%q): %v", encoded, err)
		}
		if got != pubkey {
			t.Fatalf("ParsePubkey(%q) = %s, want %s", encoded, got.Hex(), pubkey.Hex())
		}
	}
}

func TestParsePubkeyRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "not-a-key", "npub1invalid"} {
		if _, err := ParsePubkey(value); err == nil {
			t.Fatalf("ParsePubkey(%q) succeeded", value)
		}
	}
}
