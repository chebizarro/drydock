package signing

import (
	"encoding/hex"
	"errors"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip19"
)

// NewLocalSigner creates a signer from a raw nsec or hex private key.
// Use for deployment when a Signet bunker is not available.
func NewLocalSigner(nsecOrHex string) (nostr.Signer, error) {
	nsecOrHex = strings.TrimSpace(nsecOrHex)
	if nsecOrHex == "" {
		return nil, errors.New("empty key")
	}

	var skBytes []byte

	if strings.HasPrefix(nsecOrHex, "nsec1") {
		prefix, data, err := nip19.Decode(nsecOrHex)
		if err != nil {
			return nil, err
		}
		if prefix != "nsec" {
			return nil, errors.New("expected nsec bech32")
		}
		switch v := data.(type) {
		case nostr.SecretKey:
			skBytes = v[:]
		case [32]byte:
			skBytes = v[:]
		case []byte:
			skBytes = v
		default:
			return nil, errors.New("unexpected nsec data type")
		}
	} else {
		b, err := hex.DecodeString(nsecOrHex)
		if err != nil {
			return nil, err
		}
		skBytes = b
	}

	if len(skBytes) != 32 {
		return nil, errors.New("private key must be 32 bytes")
	}

	var sk [32]byte
	copy(sk[:], skBytes)
	s := keyer.NewPlainKeySigner(sk)
	return s, nil
}
