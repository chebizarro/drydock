package signing

import (
	"context"
	"errors"
	"fmt"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip05"
	"fiatjaf.com/nostr/nip46"
)

type BunkerSignerConfig struct {
	BunkerURL string
	OnAuthURL func(string)
}

func NewBunkerSigner(ctx context.Context, cfg BunkerSignerConfig) (nostr.Signer, error) {
	if !nip46.IsValidBunkerURL(cfg.BunkerURL) && !nip05.IsValidIdentifier(cfg.BunkerURL) {
		return nil, errors.New("bunker signer requires bunker:// URL or NIP-05 bunker identifier")
	}

	pool := nostr.NewPool(nostr.PoolOptions{})
	k, err := keyer.New(ctx, pool, cfg.BunkerURL, &keyer.SignerOptions{
		BunkerAuthHandler: cfg.OnAuthURL,
	})
	if err != nil {
		return nil, fmt.Errorf("create bunker signer: %w", err)
	}
	signer, ok := k.(nostr.Signer)
	if !ok {
		return nil, errors.New("resolved keyer does not implement signer")
	}
	
	if _, err := signer.GetPublicKey(ctx); err != nil {
		return nil, fmt.Errorf("bunker signer validation failed: %w", err)
	}
	
	return signer, nil
}

