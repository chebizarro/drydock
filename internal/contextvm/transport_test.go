package contextvm

import (
	"context"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
)

type fakeRelayPublisher struct {
	event  nostr.Event
	relays []string
	calls  int
}

func (f *fakeRelayPublisher) Publish(_ context.Context, relays []string, event nostr.Event) error {
	f.calls++
	f.relays = append([]string(nil), relays...)
	f.event = event
	return nil
}

func TestTransportSendPrivatePublishesGiftWrap(t *testing.T) {
	ctx := context.Background()
	sender := keyer.NewPlainKeySigner(nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000001"))
	recipient := keyer.NewPlainKeySigner(nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000002"))
	recipientPK, _ := recipient.GetPublicKey(ctx)
	pub := &fakeRelayPublisher{}
	transport := NewTransport(sender, pub, []string{"wss://relay.example"})

	wrapperID, innerID, err := transport.SendPrivate(ctx, recipientPK, "private ContextVM payload", nil)
	if err != nil {
		t.Fatalf("SendPrivate: %v", err)
	}
	if pub.calls != 1 {
		t.Fatalf("expected one publish call, got %d", pub.calls)
	}
	if pub.event.Kind != KindGiftWrap {
		t.Fatalf("published kind = %d, want %d", pub.event.Kind, KindGiftWrap)
	}
	if pub.event.ID.Hex() != wrapperID {
		t.Fatalf("wrapper id mismatch")
	}
	opened, err := OpenGiftWrap(ctx, recipient, pub.event)
	if err != nil {
		t.Fatalf("OpenGiftWrap published event: %v", err)
	}
	if opened.ID.Hex() != innerID || opened.Content != "private ContextVM payload" {
		t.Fatalf("opened payload mismatch")
	}
}
