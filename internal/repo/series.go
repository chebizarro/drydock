package repo

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"fiatjaf.com/nostr"
)

// PatchRevisionAncestry selects only the root-to-target patch lineage for a
// revision-scoped review. NIP-10 root markers may point outside the patch set;
// reply links define patch ancestry. Sibling branches and descendants of the
// requested target are deliberately excluded.
func PatchRevisionAncestry(events []nostr.Event, targetID string) ([]nostr.Event, error) {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return nil, fmt.Errorf("target patch event ID is required")
	}

	byID := make(map[string]nostr.Event, len(events))
	for _, evt := range events {
		byID[evt.ID.Hex()] = evt
	}
	current, ok := byID[targetID]
	if !ok {
		return nil, fmt.Errorf("requested target patch %s is not in its stored thread", targetID)
	}

	reversed := make([]nostr.Event, 0, len(events))
	seen := make(map[string]struct{}, len(events))
	for {
		currentID := current.ID.Hex()
		if _, duplicate := seen[currentID]; duplicate {
			return nil, fmt.Errorf("cycle in patch ancestry at %s for requested target %s", currentID, targetID)
		}
		seen[currentID] = struct{}{}
		reversed = append(reversed, current)

		previousID, hasPrevious := previousPatchID(current)
		if !hasPrevious {
			break
		}
		previous, exists := byID[previousID]
		if !exists {
			return nil, fmt.Errorf("patch %s references missing ancestor %s for requested target %s", currentID, previousID, targetID)
		}
		current = previous
	}

	slices.Reverse(reversed)
	return reversed, nil
}

func OrderPatchSeries(events []nostr.Event) []nostr.Event {
	if len(events) <= 1 {
		return events
	}

	byID := make(map[string]nostr.Event, len(events))
	indegree := make(map[string]int, len(events))
	next := make(map[string][]nostr.Event, len(events))
	for _, evt := range events {
		id := evt.ID.Hex()
		byID[id] = evt
		indegree[id] = 0
	}

	for _, evt := range events {
		if prevID, ok := previousPatchID(evt); ok {
			if _, exists := byID[prevID]; exists {
				indegree[evt.ID.Hex()]++
				next[prevID] = append(next[prevID], evt)
			}
		}
	}

	roots := make([]nostr.Event, 0, len(events))
	for _, evt := range events {
		if indegree[evt.ID.Hex()] == 0 {
			roots = append(roots, evt)
		}
	}
	sortEventsStable(roots)

	ordered := make([]nostr.Event, 0, len(events))
	queue := append([]nostr.Event(nil), roots...)
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		ordered = append(ordered, curr)

		children := append([]nostr.Event(nil), next[curr.ID.Hex()]...)
		sortEventsStable(children)
		for _, child := range children {
			indegree[child.ID.Hex()]--
			if indegree[child.ID.Hex()] == 0 {
				queue = append(queue, child)
			}
		}
	}

	if len(ordered) != len(events) {
		ordered = append([]nostr.Event(nil), events...)
		sortEventsStable(ordered)
	}
	return ordered
}

func previousPatchID(event nostr.Event) (string, bool) {
	var fallback string
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "e" {
			continue
		}
		if len(tag) >= 4 {
			switch tag[3] {
			case "reply":
				return tag[1], true
			case "root":
				continue
			}
		}
		fallback = tag[1]
	}
	if fallback != "" {
		return fallback, true
	}
	return "", false
}

func sortEventsStable(events []nostr.Event) {
	slices.SortStableFunc(events, func(a, b nostr.Event) int {
		if c := cmp.Compare(int64(a.CreatedAt), int64(b.CreatedAt)); c != 0 {
			return c
		}
		return cmp.Compare(a.ID.Hex(), b.ID.Hex())
	})
}
