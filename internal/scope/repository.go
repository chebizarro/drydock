package scope

import (
	"errors"
	"fmt"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

const RepositoryAnnouncementKind = 30617

var (
	ErrInvalidRepositoryAddress = errors.New("repository address must be a 30617:<pubkey>:<identifier> address")
	ErrInvalidRepositoryPubkey  = errors.New("repository address contains an invalid pubkey")
)

// RepositoryRef is the canonical identity derived from a NIP-34 repository
// announcement address.
type RepositoryRef struct {
	Address      string
	RepositoryID string
	OwnerPubkey  string
	Identifier   string
}

// ParseRepositoryRef parses a 30617:<hex-pubkey>:<identifier> address.
// Repository addresses use hex pubkeys on the wire; npub encodings are not
// accepted here.
func ParseRepositoryRef(address string) (RepositoryRef, error) {
	parts := strings.Split(strings.TrimSpace(address), ":")
	if len(parts) != 3 || parts[0] != fmt.Sprintf("%d", RepositoryAnnouncementKind) {
		return RepositoryRef{}, ErrInvalidRepositoryAddress
	}
	owner, err := nostr.PubKeyFromHex(parts[1])
	if err != nil {
		return RepositoryRef{}, ErrInvalidRepositoryPubkey
	}
	identifier := strings.TrimSpace(parts[2])
	if identifier == "" {
		return RepositoryRef{}, ErrInvalidRepositoryAddress
	}
	ownerHex := owner.Hex()
	return RepositoryRef{
		Address:      fmt.Sprintf("%d:%s:%s", RepositoryAnnouncementKind, ownerHex, identifier),
		RepositoryID: ownerHex + ":" + identifier,
		OwnerPubkey:  ownerHex,
		Identifier:   identifier,
	}, nil
}

// ParsePubkey accepts a hex or npub public key and returns its canonical value.
func ParsePubkey(value string) (nostr.PubKey, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "npub1") {
		prefix, decoded, err := nip19.Decode(value)
		if err != nil || prefix != "npub" {
			return nostr.ZeroPK, errors.New("invalid npub public key")
		}
		switch decoded := decoded.(type) {
		case nostr.PubKey:
			return decoded, nil
		case [32]byte:
			return nostr.PubKey(decoded), nil
		case []byte:
			if len(decoded) == 32 {
				var pubkey nostr.PubKey
				copy(pubkey[:], decoded)
				return pubkey, nil
			}
		}
		return nostr.ZeroPK, errors.New("invalid npub public key")
	}
	pubkey, err := nostr.PubKeyFromHex(value)
	if err != nil {
		return nostr.ZeroPK, errors.New("invalid hex public key")
	}
	return pubkey, nil
}

// ErrAmbiguousRepositoryRef is returned when an event does not carry exactly
// one canonical repository address tag.
var ErrAmbiguousRepositoryRef = fmt.Errorf("patch must contain exactly one canonical %d repository address", RepositoryAnnouncementKind)

// RepositoryRefFromTags resolves the single canonical repository address an
// event belongs to. It is the authoritative answer to "which repository is this
// patch for", which gates monitoring scope, so it requires exactly one match.
func RepositoryRefFromTags(tags nostr.Tags) (RepositoryRef, error) {
	addressPrefix := fmt.Sprintf("%d:", RepositoryAnnouncementKind)
	var repository RepositoryRef
	count := 0
	for _, tag := range tags {
		if len(tag) < 2 || tag[0] != "a" || !strings.HasPrefix(strings.TrimSpace(tag[1]), addressPrefix) {
			continue
		}
		ref, err := ParseRepositoryRef(tag[1])
		if err != nil {
			return RepositoryRef{}, err
		}
		count++
		repository = ref
	}
	if count != 1 {
		return RepositoryRef{}, ErrAmbiguousRepositoryRef
	}
	return repository, nil
}
