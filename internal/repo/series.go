package repo

import (
	"cmp"
	"slices"

	"fiatjaf.com/nostr"
)

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
		fallback = tag[1]
		if len(tag) >= 4 && tag[3] == "reply" {
			return tag[1], true
		}
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

