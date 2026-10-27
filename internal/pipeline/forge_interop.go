package pipeline

import (
	"encoding/hex"
	"strings"

	"fiatjaf.com/nostr"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
)

// PRs have a resolved git tip. Applied NIP-34 patches do not: use their exact
// declared commit, never the checkout's base commit or a guessed abbreviated SHA.
func ciCommitReference(event nostr.Event, preparedTip string) string {
	if preparedTip != "" {
		return strings.ToLower(preparedTip)
	}
	if event.Kind != eventkind.Patch {
		return ""
	}
	commit := ""
	for _, tag := range event.Tags {
		if len(tag) < 2 || (tag[0] != "commit" && tag[0] != "c") {
			continue
		}
		candidate := strings.ToLower(tag[1])
		if len(candidate) != 40 && len(candidate) != 64 {
			return ""
		}
		if _, err := hex.DecodeString(candidate); err != nil {
			return ""
		}
		if commit != "" && commit != candidate {
			return ""
		}
		commit = candidate
	}
	return commit
}
