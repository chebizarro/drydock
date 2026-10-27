package signing

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fiatjaf.com/nostr"
)

// LoadOrCreateClientKey loads a persisted NIP-46 transport client key from
// path. When the file exists, it reads the hex-encoded key (32 bytes). When
// the file does not exist, it generates a new key and writes it to path with
// mode 0600. An empty path generates a fresh key every call, which is suitable
// for ephemeral or short-lived clients.
func LoadOrCreateClientKey(path string) (nostr.SecretKey, error) {
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
			return LoadOrCreateClientKey(path)
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