package publisher

import (
	"strconv"

	"git.sharegap.net/cascadia/drydock/internal/scope"
)

// repositoryAnnouncementKindString is the NIP-34 repository announcement kind
// as it appears in `K`/`k` tag values.
var repositoryAnnouncementKindString = strconv.Itoa(scope.RepositoryAnnouncementKind)

// repositoryAddress builds the canonical `<kind>:<pubkey>:<identifier>` address
// for a repository from an already-qualified `<pubkey>:<identifier>` repo ID.
func repositoryAddress(repoID string) string {
	return repositoryAnnouncementKindString + ":" + repoID
}
