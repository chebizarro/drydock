package monitoring

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip34"
)

// InventoryStore is the read surface the migration inventory needs. *db.Store
// satisfies it.
type InventoryStore interface {
	LoadMonitoredRepositoryLists(context.Context) ([]db.MonitoredRepositoryListState, error)
	LoadRepositoryAnnouncement(ctx context.Context, repoID string) (nostr.Event, bool, error)
	LoadMonitoringEnrollments(context.Context) ([]db.MonitoringEnrollment, error)
}

// InventoryEntry is the migration verdict for one repository the operator
// list names.
type InventoryEntry struct {
	Repository          string   `json:"repository"`
	AnnouncementKnown   bool     `json:"announcement_known"`
	AnnouncementEventID string   `json:"announcement_event_id,omitempty"`
	Owner               string   `json:"owner,omitempty"`
	Maintainers         []string `json:"maintainers,omitempty"`
	// AuthorizedLists are authors with an active list naming this service
	// that declares the repository and hold owner/maintainer authority.
	AuthorizedLists []string `json:"authorized_lists,omitempty"`
	// UnaddressedLists are authors with owner/maintainer authority whose
	// list declares the repository but does not name this service (withdrawn
	// or addressed elsewhere); they are one p tag away from coverage.
	UnaddressedLists []string `json:"unaddressed_lists,omitempty"`
	// ActiveEnrollments are authors with an unexpired, authorized enrollment.
	ActiveEnrollments []string `json:"active_enrollments,omitempty"`
	// Verdict is "monitored" or "dropped".
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
}

// Inventory is the deterministic report of what the switch from operator
// grant to maintainer authority does to the operator's current list.
type Inventory struct {
	Operator            string           `json:"operator"`
	Drydock             string           `json:"drydock"`
	OperatorListSeen    bool             `json:"operator_list_seen"`
	OperatorListDeleted bool             `json:"operator_list_deleted"`
	GeneratedAt         int64            `json:"generated_at"`
	Entries             []InventoryEntry `json:"entries"`
	Monitored           int              `json:"monitored"`
	Dropped             int              `json:"dropped"`
}

// BuildInventory inspects persisted state only; it writes nothing. operator is
// the legacy single author (hex or npub); drydock is the service pubkey lists
// must name. The report is sorted by repository address so two runs over the
// same database are byte-identical.
func BuildInventory(ctx context.Context, store InventoryStore, operator, drydock string, now time.Time) (Inventory, error) {
	operatorPK, err := scope.ParsePubkey(operator)
	if err != nil {
		return Inventory{}, fmt.Errorf("invalid operator pubkey: %w", err)
	}
	drydockPK, err := scope.ParsePubkey(drydock)
	if err != nil {
		return Inventory{}, fmt.Errorf("invalid drydock pubkey: %w", err)
	}
	inv := Inventory{Operator: operatorPK.Hex(), Drydock: drydockPK.Hex(), GeneratedAt: now.Unix()}

	states, err := store.LoadMonitoredRepositoryLists(ctx)
	if err != nil {
		return Inventory{}, err
	}
	type listInfo struct {
		author nostr.PubKey
		names  bool
		repos  map[string]scope.RepositoryRef
	}
	var operatorRepos map[string]scope.RepositoryRef
	var lists []listInfo
	for _, state := range states {
		list, err := listFromPersisted(state, drydockPK)
		if err != nil {
			continue // unreadable persisted state cannot grant; Load would refuse it too
		}
		if list.author == operatorPK {
			inv.OperatorListSeen = true
			switch list.status {
			case StatusDeleted:
				inv.OperatorListDeleted = true
			case StatusActive, StatusWithdrawn:
				operatorRepos = list.repos
			}
		}
		// Every active list, the operator's included, is a candidate opt-in;
		// whether it grants is decided per repository by authority below.
		// Withdrawn lists are reported as "unaddressed" (one p tag away).
		switch list.status {
		case StatusActive:
			lists = append(lists, listInfo{author: list.author, names: list.namesDrydock, repos: list.repos})
		case StatusWithdrawn:
			lists = append(lists, listInfo{author: list.author, names: false, repos: list.repos})
		}
	}

	enrollments, err := store.LoadMonitoringEnrollments(ctx)
	if err != nil {
		return Inventory{}, err
	}
	activeEnrollments := map[string][]nostr.PubKey{}
	for _, e := range enrollments {
		if !e.Active(now.Unix()) {
			continue
		}
		if pk, err := nostr.PubKeyFromHex(e.AuthorPubkey); err == nil {
			activeEnrollments[e.RepositoryAddress] = append(activeEnrollments[e.RepositoryAddress], pk)
		}
	}

	addresses := make([]string, 0, len(operatorRepos))
	for address := range operatorRepos {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	for _, address := range addresses {
		ref := operatorRepos[address]
		entry := InventoryEntry{Repository: address}
		announcement, ok, err := store.LoadRepositoryAnnouncement(ctx, ref.RepositoryID)
		if err != nil {
			return Inventory{}, err
		}
		authorized := func(pk nostr.PubKey) bool { return false }
		if ok && db.ValidRepositoryAnnouncement(announcement) && announcement.PubKey.Hex() == ref.OwnerPubkey {
			entry.AnnouncementKnown = true
			entry.AnnouncementEventID = announcement.ID.Hex()
			entry.Owner = announcement.PubKey.Hex()
			maintainers := map[nostr.PubKey]struct{}{}
			for _, m := range nip34.ParseRepository(announcement).Maintainers {
				maintainers[m] = struct{}{}
				entry.Maintainers = append(entry.Maintainers, m.Hex())
			}
			sort.Strings(entry.Maintainers)
			authorized = func(pk nostr.PubKey) bool {
				if pk == announcement.PubKey {
					return true
				}
				_, ok := maintainers[pk]
				return ok
			}
		}
		for _, list := range lists {
			if _, declares := list.repos[address]; !declares || !authorized(list.author) {
				continue
			}
			if list.names {
				entry.AuthorizedLists = append(entry.AuthorizedLists, list.author.Hex())
			} else {
				entry.UnaddressedLists = append(entry.UnaddressedLists, list.author.Hex())
			}
		}
		for _, pk := range activeEnrollments[address] {
			if authorized(pk) {
				entry.ActiveEnrollments = append(entry.ActiveEnrollments, pk.Hex())
			}
		}
		sort.Strings(entry.AuthorizedLists)
		sort.Strings(entry.UnaddressedLists)
		sort.Strings(entry.ActiveEnrollments)
		switch {
		case len(entry.AuthorizedLists) > 0 || len(entry.ActiveEnrollments) > 0:
			entry.Verdict = "monitored"
			inv.Monitored++
		case !entry.AnnouncementKnown:
			entry.Verdict, entry.Reason = "dropped", "no_announcement"
			inv.Dropped++
		case len(entry.UnaddressedLists) > 0:
			entry.Verdict, entry.Reason = "dropped", "maintainer_list_does_not_name_drydock"
			inv.Dropped++
		default:
			entry.Verdict, entry.Reason = "dropped", "no_maintainer_optin"
			inv.Dropped++
		}
		inv.Entries = append(inv.Entries, entry)
	}
	return inv, nil
}

// Text renders the inventory as a stable human-readable table.
func (inv Inventory) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "operator=%s drydock=%s operator_list_seen=%v deleted=%v monitored=%d dropped=%d\n",
		inv.Operator, inv.Drydock, inv.OperatorListSeen, inv.OperatorListDeleted, inv.Monitored, inv.Dropped)
	for _, e := range inv.Entries {
		fmt.Fprintf(&b, "%-9s %s", e.Verdict, e.Repository)
		if e.Reason != "" {
			fmt.Fprintf(&b, " reason=%s", e.Reason)
		}
		if len(e.AuthorizedLists) > 0 {
			fmt.Fprintf(&b, " lists=%s", strings.Join(e.AuthorizedLists, ","))
		}
		if len(e.ActiveEnrollments) > 0 {
			fmt.Fprintf(&b, " enrollments=%s", strings.Join(e.ActiveEnrollments, ","))
		}
		if len(e.UnaddressedLists) > 0 {
			fmt.Fprintf(&b, " unaddressed=%s", strings.Join(e.UnaddressedLists, ","))
		}
		b.WriteString("\n")
	}
	return b.String()
}
