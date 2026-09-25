package db

import (
	"context"
	"testing"

	"fiatjaf.com/nostr"
)

func signedEvent(t *testing.T, content string) nostr.Event {
	t.Helper()
	e := nostr.Event{Kind: 25910, CreatedAt: nostr.Now(), Content: content}
	e.ID = e.GetID()
	return e
}

// TestReviewPublicationOutboxReusesReservedEvent pins the delivery-idempotency
// contract of the review publication outbox: once an event is reserved for a
// (patch, repo, type, index) key, every later reserve for that key returns the
// exact first event — same Nostr event ID — so a retried relay publish stays
// idempotent, and marking it delivered does not change the stored event.
func TestReviewPublicationOutboxReusesReservedEvent(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	first := signedEvent(t, `{"n":1}`)
	second := signedEvent(t, `{"n":2}`)
	if first.ID == second.ID {
		t.Fatal("test fixtures must have distinct event IDs")
	}

	reserved, delivered, err := store.ReserveReviewPublication(ctx, "patch1", "repo1", "summary", 0, first)
	if err != nil {
		t.Fatalf("reserve first: %v", err)
	}
	if delivered {
		t.Fatal("freshly reserved publication must not be delivered")
	}
	if reserved.ID != first.ID {
		t.Fatalf("reserve returned %s, want %s", reserved.ID.Hex(), first.ID.Hex())
	}

	// A second reserve of the same logical event must reuse the first one.
	reReserved, _, err := store.ReserveReviewPublication(ctx, "patch1", "repo1", "summary", 0, second)
	if err != nil {
		t.Fatalf("reserve second: %v", err)
	}
	if reReserved.ID != first.ID {
		t.Fatalf("reserve reused %s, want original %s (idempotency broken)", reReserved.ID.Hex(), first.ID.Hex())
	}

	got, delivered, found, err := store.GetReviewPublication(ctx, "patch1", "repo1", "summary", 0)
	if err != nil || !found {
		t.Fatalf("get publication: found=%v err=%v", found, err)
	}
	if got.ID != first.ID || delivered {
		t.Fatalf("get returned id=%s delivered=%v, want id=%s delivered=false", got.ID.Hex(), delivered, first.ID.Hex())
	}

	if err := store.MarkReviewPublicationDelivered(ctx, "patch1", "repo1", "summary", 0); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	got, delivered, found, err = store.GetReviewPublication(ctx, "patch1", "repo1", "summary", 0)
	if err != nil || !found {
		t.Fatalf("get after deliver: found=%v err=%v", found, err)
	}
	if !delivered || got.ID != first.ID {
		t.Fatalf("after deliver id=%s delivered=%v, want id=%s delivered=true", got.ID.Hex(), delivered, first.ID.Hex())
	}

	// A distinct key is an independent reservation.
	other, _, err := store.ReserveReviewPublication(ctx, "patch1", "repo1", "detail", 1, second)
	if err != nil {
		t.Fatalf("reserve distinct key: %v", err)
	}
	if other.ID != second.ID {
		t.Fatalf("distinct key reserved %s, want %s", other.ID.Hex(), second.ID.Hex())
	}
}

// TestReviewFailureNoticeOutboxReusesReservedEvent exercises the same contract
// through the two-column key shape, proving the shared outbox helper handles
// both key widths.
func TestReviewFailureNoticeOutboxReusesReservedEvent(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	first := signedEvent(t, `{"notice":1}`)
	second := signedEvent(t, `{"notice":2}`)
	if first.ID == second.ID {
		t.Fatal("test fixtures must have distinct event IDs")
	}

	reserved, delivered, err := store.ReserveReviewFailureNotice(ctx, "patch1", "repo1", first)
	if err != nil {
		t.Fatalf("reserve first: %v", err)
	}
	if delivered || reserved.ID != first.ID {
		t.Fatalf("reserve returned id=%s delivered=%v, want id=%s delivered=false", reserved.ID.Hex(), delivered, first.ID.Hex())
	}

	reReserved, _, err := store.ReserveReviewFailureNotice(ctx, "patch1", "repo1", second)
	if err != nil {
		t.Fatalf("reserve second: %v", err)
	}
	if reReserved.ID != first.ID {
		t.Fatalf("reserve reused %s, want original %s (idempotency broken)", reReserved.ID.Hex(), first.ID.Hex())
	}

	if err := store.MarkReviewFailureNoticeDelivered(ctx, "patch1", "repo1"); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	got, delivered, found, err := store.GetReviewFailureNotice(ctx, "patch1", "repo1")
	if err != nil || !found {
		t.Fatalf("get after deliver: found=%v err=%v", found, err)
	}
	if !delivered || got.ID != first.ID {
		t.Fatalf("after deliver id=%s delivered=%v, want id=%s delivered=true", got.ID.Hex(), delivered, first.ID.Hex())
	}
}

// TestReviewFailureNoticeMarkDeliveredMissing pins that marking an unreserved
// key reports the reservation as missing rather than silently succeeding.
func TestReviewFailureNoticeMarkDeliveredMissing(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	if err := store.MarkReviewFailureNoticeDelivered(ctx, "absent", "repo"); err == nil {
		t.Fatal("expected error marking a nonexistent reservation delivered")
	}
}
