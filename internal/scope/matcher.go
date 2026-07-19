package scope

import (
	"fmt"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

// Matcher applies operator-configured repository and repository-owner allowlists.
// Its zero value allows all repositories.
type Matcher struct {
	repositories map[string]struct{}
	owners       map[string]struct{}
}

// NewMatcher constructs a matcher from normalized or user-facing allowlist values.
func NewMatcher(repositoryIDs, ownerPubkeys []string) Matcher {
	m := Matcher{}
	if len(repositoryIDs) > 0 {
		m.repositories = make(map[string]struct{}, len(repositoryIDs))
		for _, repoID := range repositoryIDs {
			if normalized := NormalizeRepositoryID(repoID); normalized != "" {
				m.repositories[normalized] = struct{}{}
			}
		}
	}
	if len(ownerPubkeys) > 0 {
		m.owners = make(map[string]struct{}, len(ownerPubkeys))
		for _, owner := range ownerPubkeys {
			if normalized := NormalizePubkey(owner); normalized != "" {
				m.owners[normalized] = struct{}{}
			}
		}
	}
	return m
}

// Enabled reports whether repository scoping is configured.
func (m Matcher) Enabled() bool {
	return len(m.repositories) > 0 || len(m.owners) > 0
}

// Allows reports whether a repository ID or its stored announcement owner is allowed.
func (m Matcher) Allows(repositoryID, ownerPubkey string) bool {
	if !m.Enabled() {
		return true
	}
	if _, ok := m.repositories[NormalizeRepositoryID(repositoryID)]; ok {
		return true
	}
	_, ok := m.owners[NormalizePubkey(ownerPubkey)]
	return ok
}

// NormalizeRepositoryID converts an npub:identifier or hex:identifier ID to
// the canonical lowercase-hex owner form used by the database.
func NormalizeRepositoryID(repositoryID string) string {
	parts := strings.SplitN(strings.TrimSpace(repositoryID), ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
		return strings.TrimSpace(repositoryID)
	}
	return NormalizePubkey(parts[0]) + ":" + strings.TrimSpace(parts[1])
}

// NormalizePubkey converts npub public keys to lowercase hex. Other values are
// lowercased so invalid configured entries remain restrictive rather than
// accidentally turning an allowlist into allow-all.
func NormalizePubkey(pubkey string) string {
	pubkey = strings.TrimSpace(pubkey)
	if !strings.HasPrefix(strings.ToLower(pubkey), "npub1") {
		return strings.ToLower(pubkey)
	}

	prefix, value, err := nip19.Decode(pubkey)
	if err != nil || prefix != "npub" {
		return strings.ToLower(pubkey)
	}
	switch value := value.(type) {
	case nostr.PubKey:
		return value.Hex()
	case [32]byte:
		return fmt.Sprintf("%x", value[:])
	case []byte:
		if len(value) == 32 {
			return fmt.Sprintf("%x", value)
		}
	}
	return strings.ToLower(pubkey)
}
