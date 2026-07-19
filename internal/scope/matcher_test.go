package scope

import (
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

func TestMatcherAllowsAllWhenUnconfigured(t *testing.T) {
	if !NewMatcher(nil, nil).Allows("any:repository", "any-owner") {
		t.Fatal("empty allowlists should allow all repositories")
	}
}

func TestMatcherAllowsConfiguredRepositoryOrOwner(t *testing.T) {
	repoOwner := nostr.GetPublicKey(nostr.Generate()).Hex()
	allowedOwner := nostr.GetPublicKey(nostr.Generate()).Hex()
	matcher := NewMatcher(
		[]string{repoOwner + ":allowed-repo"},
		[]string{allowedOwner},
	)

	tests := []struct {
		name    string
		repoID  string
		owner   string
		allowed bool
	}{
		{name: "repository allowed", repoID: repoOwner + ":allowed-repo", owner: repoOwner, allowed: true},
		{name: "owner allowed", repoID: allowedOwner + ":another-repo", owner: allowedOwner, allowed: true},
		{name: "neither allowed", repoID: repoOwner + ":denied-repo", owner: repoOwner, allowed: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := matcher.Allows(test.repoID, test.owner); got != test.allowed {
				t.Fatalf("Allows(%q, %q) = %v, want %v", test.repoID, test.owner, got, test.allowed)
			}
		})
	}
}

func TestMatcherNormalizesNpubValues(t *testing.T) {
	owner := nostr.GetPublicKey(nostr.Generate())
	npub := nip19.EncodeNpub(owner)
	matcher := NewMatcher([]string{npub + ":repo"}, []string{npub})

	if got := NormalizeRepositoryID(npub + ":repo"); got != owner.Hex()+":repo" {
		t.Fatalf("NormalizeRepositoryID() = %q, want %q", got, owner.Hex()+":repo")
	}
	if got := NormalizePubkey(npub); got != owner.Hex() {
		t.Fatalf("NormalizePubkey() = %q, want %q", got, owner.Hex())
	}
	if !matcher.Allows(owner.Hex()+":repo", "") {
		t.Fatal("npub repository entry should match canonical hex repository ID")
	}
	if !matcher.Allows("different:repo", owner.Hex()) {
		t.Fatal("npub owner entry should match canonical hex owner")
	}
}
