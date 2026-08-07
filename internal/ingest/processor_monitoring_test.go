package ingest_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"drydock/internal/ingest"
	"drydock/internal/monitoring"
	"drydock/internal/revieworder"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
)

func TestProcessorAppliesLiveMonitoredListToReactiveAdmission(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	operatorSK := nostr.Generate()
	operator := nostr.GetPublicKey(operatorSK)
	registry, err := monitoring.NewRegistry(store, operator.Hex())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	orders := revieworder.New(revieworder.Config{}, store, scope.Matcher{}, registry, nil, nil, logger)
	processor := ingest.NewProcessor(
		store,
		logger,
		ingest.WithMonitoring(registry),
		ingest.WithReviewOrders(orders),
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
	monitoredPatch := nostr.Event{
		Kind: 1617, CreatedAt: nostr.Now(),
		Tags: nostr.Tags{{"a", repo}}, Content: "monitored diff",
	}
	signEvent(t, nostr.Generate(), &monitoredPatch)
	if err := processor.ProcessEvent(ctx, monitoredPatch, "wss://relay.test"); err != nil {
		t.Fatalf("process monitored patch: %v", err)
	}
	select {
	case task := <-orders.Queue():
		if task.PatchEventID != monitoredPatch.ID.Hex() {
			t.Fatalf("queued task = %+v", task)
		}
	default:
		t.Fatal("live list addition did not admit reactive patch")
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
	removedPatch := nostr.Event{
		Kind: 1617, CreatedAt: nostr.Now(),
		Tags: nostr.Tags{{"a", repo}}, Content: "removed diff",
	}
	signEvent(t, nostr.Generate(), &removedPatch)
	if err := processor.ProcessEvent(ctx, removedPatch, "wss://relay.test"); err != nil {
		t.Fatalf("process removed patch: %v", err)
	}
	select {
	case task := <-orders.Queue():
		t.Fatalf("removed repository patch was queued: %+v", task)
	default:
	}
	ref, err := scope.ParseRepositoryRef(repo)
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok, err := store.GetReviewSkip(ctx, removedPatch.ID.Hex(), ref.RepositoryID); err != nil || !ok || reason != "not_monitored" {
		t.Fatalf("removed repository durable skip = %q ok=%v err=%v", reason, ok, err)
	}

	// Handler completion makes relay redelivery idempotent.
	if err := processor.ProcessEvent(ctx, deletion, "wss://relay.test"); err != nil {
		t.Fatalf("reprocess deletion: %v", err)
	}
	if registry.Snapshot().RevisionID != deletion.ID.Hex() {
		t.Fatalf("duplicate changed revision: %#v", registry.Snapshot())
	}
}
