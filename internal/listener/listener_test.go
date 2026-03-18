package listener

import "testing"

func TestSubscribedKindsSet(t *testing.T) {
	kinds := SubscribedKinds()

	expected := map[int]bool{
		30617: true,
		30618: true,
		1617:  true,
		1618:  true,
		1619:  true,
		1621:  true,
		1622:  true,
		1630:  true,
		1631:  true,
		1632:  true,
		1633:  true,
		1985:  true,
	}

	if len(kinds) != len(expected) {
		t.Fatalf("expected %d kinds, got %d", len(expected), len(kinds))
	}

	seen := make(map[int]bool, len(kinds))
	for _, kind := range kinds {
		seen[int(kind)] = true
	}

	for kind := range expected {
		if !seen[kind] {
			t.Fatalf("missing kind %d", kind)
		}
	}
}

