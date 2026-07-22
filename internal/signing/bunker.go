package signing

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip46"
)

// NewBunkerSigner connects to a NIP-46 bunker. When clientKeyFile is set, the
// client identity is persisted so a one-time bunker connect grant remains
// usable across supervised process and container restarts.
func NewBunkerSigner(ctx context.Context, bunkerURL, clientKeyFile string, relays ...string) (nostr.Keyer, error) {
	if strings.TrimSpace(bunkerURL) == "" {
		return nil, fmt.Errorf("signet bunker url is required")
	}
	clientKey, err := loadOrCreateClientKey(clientKeyFile)
	if err != nil {
		return nil, err
	}
	preparedURL, err := bunkerURLWithFallbackRelays(ctx, bunkerURL, relays)
	if err != nil {
		return nil, err
	}
	client, err := nip46.ConnectBunker(ctx, clientKey, preparedURL, nil, func(string) {})
	if err != nil {
		return nil, err
	}
	return keyer.NewBunkerSignerFromBunkerClient(client), nil
}

func loadOrCreateClientKey(path string) (nostr.SecretKey, error) {
	if strings.TrimSpace(path) == "" {
		return nostr.Generate(), nil
	}
	if raw, err := os.ReadFile(path); err == nil {
		key, err := nostr.SecretKeyFromHex(strings.TrimSpace(string(raw)))
		if err != nil {
			return nostr.SecretKey{}, fmt.Errorf("parse signer client key file: %w", err)
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return nostr.SecretKey{}, fmt.Errorf("read signer client key file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nostr.SecretKey{}, fmt.Errorf("create signer client key directory: %w", err)
	}
	key := nostr.Generate()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return loadOrCreateClientKey(path)
		}
		return nostr.SecretKey{}, fmt.Errorf("create signer client key file: %w", err)
	}
	if _, err = f.WriteString(key.Hex() + "\n"); err != nil {
		_ = f.Close()
		return nostr.SecretKey{}, fmt.Errorf("write signer client key file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nostr.SecretKey{}, fmt.Errorf("close signer client key file: %w", err)
	}
	return key, nil
}

func bunkerURLWithFallbackRelays(ctx context.Context, bunkerURL string, relays []string) (string, error) {
	parsed, err := nip46.ParseBunkerInput(ctx, bunkerURL)
	if err != nil {
		return "", err
	}
	if len(parsed.Relays) > 0 || len(relays) == 0 {
		return bunkerURL, nil
	}
	u, err := url.Parse(bunkerURL)
	if err != nil {
		return "", fmt.Errorf("invalid bunker url: %w", err)
	}
	if u.Scheme != "bunker" {
		return bunkerURL, nil
	}
	q := u.Query()
	for _, relay := range relays {
		if relay != "" {
			q.Add("relay", relay)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
