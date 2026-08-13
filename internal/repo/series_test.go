package repo

import (
	"strings"
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

func TestPatchRevisionAncestryOrdersOnlyRequestedLineage(t *testing.T) {
	root := nostr.Event{
		ID:        nostr.MustIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		CreatedAt: nostr.Timestamp(100),
	}
	parent := nostr.Event{
		ID:        nostr.MustIDFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		CreatedAt: nostr.Timestamp(103),
		Tags:      nostr.Tags{{"e", root.ID.Hex(), "", "reply"}},
	}
	target := nostr.Event{
		ID:        nostr.MustIDFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		CreatedAt: nostr.Timestamp(102),
		Tags: nostr.Tags{
			{"e", root.ID.Hex(), "", "root"},
			{"e", parent.ID.Hex(), "", "reply"},
		},
	}
	sibling := nostr.Event{
		ID:        nostr.MustIDFromHex("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"),
		CreatedAt: nostr.Timestamp(101),
		Tags:      nostr.Tags{{"e", root.ID.Hex(), "", "reply"}},
	}
	descendant := nostr.Event{
		ID:        nostr.MustIDFromHex("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"),
		CreatedAt: nostr.Timestamp(104),
		Tags:      nostr.Tags{{"e", target.ID.Hex(), "", "reply"}},
	}

	ancestry, err := PatchRevisionAncestry(
		[]nostr.Event{descendant, target, sibling, root, parent},
		target.ID.Hex(),
	)
	if err != nil {
		t.Fatalf("select target ancestry: %v", err)
	}
	want := []nostr.ID{root.ID, parent.ID, target.ID}
	if len(ancestry) != len(want) {
		t.Fatalf("ancestry length = %d, want %d: %v", len(ancestry), len(want), ancestry)
	}
	for i := range want {
		if ancestry[i].ID != want[i] {
			t.Fatalf("ancestry[%d] = %s, want %s", i, ancestry[i].ID.Hex(), want[i].Hex())
		}
	}
}

func TestPatchRevisionAncestryRejectsMissingAncestor(t *testing.T) {
	missing := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	target := nostr.Event{
		ID:   nostr.MustIDFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		Tags: nostr.Tags{{"e", missing, "", "reply"}},
	}
	_, err := PatchRevisionAncestry([]nostr.Event{target}, target.ID.Hex())
	if err == nil || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), target.ID.Hex()) {
		t.Fatalf("expected missing ancestor and requested target attribution, got %v", err)
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
