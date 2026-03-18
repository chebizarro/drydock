package repo

import (
	"testing"

	"fiatjaf.com/nostr"
)

func TestOrderPatchSeriesPrefersReplyChain(t *testing.T) {
	root := nostr.Event{
		ID:        nostr.MustIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		CreatedAt: nostr.Timestamp(100),
	}
	second := nostr.Event{
		ID:        nostr.MustIDFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		CreatedAt: nostr.Timestamp(102),
		Tags:      nostr.Tags{{"e", root.ID.Hex(), "", "reply"}},
	}
	third := nostr.Event{
		ID:        nostr.MustIDFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		CreatedAt: nostr.Timestamp(101),
		Tags:      nostr.Tags{{"e", second.ID.Hex(), "", "reply"}},
	}

	ordered := OrderPatchSeries([]nostr.Event{third, second, root})
	if len(ordered) != 3 {
		t.Fatalf("expected 3 events, got %d", len(ordered))
	}
	if ordered[0].ID != root.ID || ordered[1].ID != second.ID || ordered[2].ID != third.ID {
		t.Fatalf("unexpected order: %s, %s, %s", ordered[0].ID.Hex(), ordered[1].ID.Hex(), ordered[2].ID.Hex())
	}
}

func TestOrderPatchSeriesFallsBackToCreatedAt(t *testing.T) {
	a := nostr.Event{
		ID:        nostr.MustIDFromHex("1111111111111111111111111111111111111111111111111111111111111111"),
		CreatedAt: nostr.Timestamp(5),
	}
	b := nostr.Event{
		ID:        nostr.MustIDFromHex("2222222222222222222222222222222222222222222222222222222222222222"),
		CreatedAt: nostr.Timestamp(1),
	}
	c := nostr.Event{
		ID:        nostr.MustIDFromHex("3333333333333333333333333333333333333333333333333333333333333333"),
		CreatedAt: nostr.Timestamp(3),
	}

	ordered := OrderPatchSeries([]nostr.Event{a, b, c})
	if ordered[0].ID != b.ID || ordered[1].ID != c.ID || ordered[2].ID != a.ID {
		t.Fatalf("unexpected created_at fallback order: %s, %s, %s", ordered[0].ID.Hex(), ordered[1].ID.Hex(), ordered[2].ID.Hex())
	}
}

