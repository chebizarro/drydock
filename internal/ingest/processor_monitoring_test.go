package ingest_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"drydock/internal/ingest"
	"drydock/internal/monitoring"

	"fiatjaf.com/nostr"
)

func TestProcessorPersistsOldMonitoredListAndDeletionWithoutChangingReviewGate(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	operatorSK := nostr.Generate()
	operator := nostr.GetPublicKey(operatorSK)
	registry, err := monitoring.NewRegistry(store, operator.Hex())
	if err != nil {
		t.Fatal(err)
	}
	processor := ingest.NewProcessor(
		store,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		ingest.WithMonitoring(registry),
	)

	repo := "30617:" + nostr.GetPublicKey(nostr.Generate()).Hex() + ":repo"
	list := nostr.Event{
		Kind:      monitoring.MonitoredListKind,
		CreatedAt: nostr.Timestamp(time.Now().Add(-2 * 365 * 24 * time.Hour).Unix()),
		Tags: nostr.Tags{
			{"d", monitoring.ListIdentifier},
			{"a", repo},
		},
	}
	signEvent(t, operatorSK, &list)
	if err := processor.ProcessEvent(ctx, list, "wss://relay.test"); err != nil {
		t.Fatalf("process list: %v", err)
	}
	if !registry.Contains(repo) {
		t.Fatalf("old current list was not applied: %#v", registry.Snapshot())
	}
	complete, err := store.IsIngestHandlerComplete(ctx, list.ID.Hex())
	if err != nil || !complete {
		t.Fatalf("list completion: complete=%v err=%v", complete, err)
	}

	deletion := nostr.Event{
		Kind:      monitoring.DeletionKind,
		CreatedAt: nostr.Timestamp(time.Now().Add(-18 * 30 * 24 * time.Hour).Unix()),
		Tags:      nostr.Tags{{"a", monitoring.ListAddress(operator.Hex())}},
	}
	signEvent(t, operatorSK, &deletion)
	if err := processor.ProcessEvent(ctx, deletion, "wss://relay.test"); err != nil {
		t.Fatalf("process deletion: %v", err)
	}
	snapshot := registry.Snapshot()
	if !snapshot.Initialized || !snapshot.Deleted || registry.Contains(repo) {
		t.Fatalf("deletion snapshot: %#v", snapshot)
	}

	// Handler completion makes relay redelivery idempotent.
	if err := processor.ProcessEvent(ctx, deletion, "wss://relay.test"); err != nil {
		t.Fatalf("reprocess deletion: %v", err)
	}
	if registry.Snapshot().RevisionID != deletion.ID.Hex() {
		t.Fatalf("duplicate changed revision: %#v", registry.Snapshot())
	}
}
