package config

import (
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

func TestFromEnvNormalizesTrustedZappers(t *testing.T) {
	zapper := nostr.GetPublicKey(nostr.Generate())
	t.Setenv("DRYDOCK_TRUSTED_ZAPPERS", nip19.EncodeNpub(zapper)+","+zapper.Hex())

	cfg := FromEnv()
	if len(cfg.TrustedZappers) != 2 {
		t.Fatalf("trusted zapper count = %d, want 2", len(cfg.TrustedZappers))
	}
	for _, got := range cfg.TrustedZappers {
		if got != zapper.Hex() {
			t.Fatalf("trusted zapper = %q, want %q", got, zapper.Hex())
		}
	}
}
