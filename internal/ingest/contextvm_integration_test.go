package ingest_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"drydock/internal/contextvm"
	"drydock/internal/ingest"

	"fiatjaf.com/nostr"
)

func TestIntegrationPlainAndGiftWrappedContextVMRequestsAndNotifications(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	router := contextvm.NewRouter()
	requestCalls := 0
	notificationCalls := 0
	if err := router.Register("review/order", func(context.Context, contextvm.Request) (any, *contextvm.Error) {
		requestCalls++
		return map[string]string{"status": "accepted"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := router.RegisterNotification("marketplace/feedback", func(context.Context, contextvm.Request) error {
		notificationCalls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	responder := &recordingContextVMResponder{}
	processor := ingest.NewProcessor(
		store,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		ingest.WithContextVM(router, responder),
	)

	plainRequest := nostr.Event{
		Kind: nostr.Kind(25910), CreatedAt: nostr.Now(),
		Content: `{"jsonrpc":"2.0","id":"plain-order","method":"review/order"}`,
	}
	signEvent(t, nostr.Generate(), &plainRequest)
	if err := processor.ProcessEvent(ctx, plainRequest, "wss://relay.test"); err != nil {
		t.Fatalf("plain request: %v", err)
	}
	if requestCalls != 1 || responder.calls != 1 {
		t.Fatalf("plain request calls=%d responses=%d", requestCalls, responder.calls)
	}

	plainNotification := nostr.Event{
		Kind: nostr.Kind(25910), CreatedAt: nostr.Now(),
		Content: `{"jsonrpc":"2.0","method":"marketplace/feedback","params":{}}`,
	}
	signEvent(t, nostr.Generate(), &plainNotification)
	if err := processor.ProcessEvent(ctx, plainNotification, "wss://relay.test"); err != nil {
		t.Fatalf("plain notification: %v", err)
	}
	if notificationCalls != 1 || responder.calls != 1 {
		t.Fatalf("plain notification calls=%d responses=%d", notificationCalls, responder.calls)
	}

	giftSender := nostr.GetPublicKey(nostr.Generate())
	giftRequest := nostr.Event{
		PubKey: giftSender, Kind: nostr.Kind(25910), CreatedAt: nostr.Now(),
		Content: `{"jsonrpc":"2.0","id":"gift-order","method":"review/order"}`,
	}
	giftRequest.ID = giftRequest.GetID()
	if err := processor.ProcessGiftWrappedEvent(ctx, giftRequest, "wss://relay.test"); err != nil {
		t.Fatalf("gift-wrapped request rumor: %v", err)
	}
	if requestCalls != 2 || responder.calls != 2 {
		t.Fatalf("gift request calls=%d responses=%d", requestCalls, responder.calls)
	}

	giftNotification := nostr.Event{
		PubKey: giftSender, Kind: nostr.Kind(25910), CreatedAt: nostr.Now(),
		Content: `{"jsonrpc":"2.0","method":"marketplace/feedback","params":{}}`,
	}
	giftNotification.ID = giftNotification.GetID()
	if err := processor.ProcessGiftWrappedEvent(ctx, giftNotification, "wss://relay.test"); err != nil {
		t.Fatalf("gift-wrapped notification rumor: %v", err)
	}
	if notificationCalls != 2 {
		t.Fatalf("gift notification calls = %d, want 2 total", notificationCalls)
	}
	if responder.calls != 2 {
		t.Fatalf("notification produced a response; responses = %d", responder.calls)
	}
}
