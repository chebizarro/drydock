package ingest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
	"git.sharegap.net/cascadia/drydock/internal/ingest"
	"git.sharegap.net/cascadia/drydock/internal/loom"
	"git.sharegap.net/cascadia/drydock/internal/scope"
)

type loomMembership string

func (m loomMembership) Contains(a string) bool { return string(m) == a }
func TestProcessorIngestsSyntheticCIClaimAndRejectsForgery(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	author, publisher := nostr.Generate(), nostr.Generate()
	ref, err := scope.ParseRepositoryRef(fmt.Sprintf("%d:%s:repo", eventkind.RepositoryAnnouncement, author.Public().Hex()))
	if err != nil {
		t.Fatal(err)
	}
	claims := loom.New(store, loomMembership(ref.Address), loom.Config{MaxPerRepository: 64, MaxTotal: 1024, TTL: time.Hour}, nil)
	processor := ingest.NewProcessor(store, nil, ingest.WithLoom(claims))
	run := nostr.Event{Kind: eventkind.HiveWorkflowRun, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"a", ref.Address}, {"publisher", publisher.Public().Hex()}, {"commit", strings.Repeat("b", 40)}}}
	signEvent(t, author, &run)
	if err := processor.ProcessEvent(ctx, run, "wss://fixture.example"); err != nil {
		t.Fatal(err)
	}
	result := nostr.Event{Kind: eventkind.HiveWorkflowResult, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"e", run.ID.Hex()}, {"status", "success"}}}
	signEvent(t, publisher, &result)
	forged := result
	forged.Content = "forged"
	if err := processor.ProcessEvent(ctx, forged, "wss://fixture.example"); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.CIAttestationsForPatch(ctx, ref.RepositoryID, strings.Repeat("b", 40), "")
	if len(rows) != 0 {
		t.Fatal("invalid signature/id claim accepted")
	}
	for i := 0; i < 2; i++ {
		if err := processor.ProcessEvent(ctx, result, "wss://fixture.example"); err != nil {
			t.Fatal(err)
		}
	}
	rows, err = store.CIAttestationsForPatch(ctx, ref.RepositoryID, strings.Repeat("b", 40), "")
	if err != nil || len(rows) != 1 || rows[0].Signer != publisher.Public().Hex() {
		t.Fatalf("persisted claims: %#v %v", rows, err)
	}
	// Disabled ingest does not track runs, even if another path delivers one.
	disabled := ingest.NewProcessor(store, nil)
	run.CreatedAt++
	signEvent(t, author, &run)
	if err := disabled.ProcessEvent(ctx, run, "wss://fixture.example"); err != nil {
		t.Fatal(err)
	}
	active, _ := store.ListLoomRuns(ctx, time.Now().Unix())
	if len(active) != 0 {
		t.Fatal("disabled feature tracked a run")
	}
}
