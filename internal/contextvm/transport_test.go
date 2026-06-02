package contextvm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"fiatjaf.com/nostr"
)

type testSigner struct{ sk [32]byte }

func newTestSigner(seed byte) testSigner {
	var sk [32]byte
	sk[0] = seed
	return testSigner{sk: sk}
}

func (s testSigner) GetPublicKey(ctx context.Context) (nostr.PubKey, error) {
	return nostr.GetPublicKey(s.sk), nil
}

func (s testSigner) SignEvent(ctx context.Context, evt *nostr.Event) error {
	return evt.Sign(s.sk)
}

type fakePool struct {
	published []nostr.Event
	filter    nostr.Filter
	events    chan nostr.RelayEvent
	closed    chan nostr.RelayClosed
	pubErr    error
}

func (p *fakePool) PublishMany(ctx context.Context, urls []string, evt nostr.Event) chan nostr.PublishResult {
	p.published = append(p.published, evt)
	ch := make(chan nostr.PublishResult, len(urls))
	for _, url := range urls {
		ch <- nostr.PublishResult{RelayURL: url, Error: p.pubErr}
	}
	close(ch)
	return ch
}

func (p *fakePool) SubscribeManyNotifyClosed(ctx context.Context, urls []string, filter nostr.Filter, opts nostr.SubscriptionOptions) (chan nostr.RelayEvent, chan nostr.RelayClosed) {
	p.filter = filter
	if p.events == nil {
		p.events = make(chan nostr.RelayEvent)
	}
	if p.closed == nil {
		p.closed = make(chan nostr.RelayClosed)
	}
	return p.events, p.closed
}

func TestTransportSendPublishesContextVMEvent(t *testing.T) {
	ctx := context.Background()
	pool := &fakePool{}
	signer := newTestSigner(1)
	recipient, _ := newTestSigner(2).GetPublicKey(ctx)
	tr := NewTransport(pool, signer, []string{"wss://read"}, []string{"wss://write"}, nil)

	id, err := tr.Send(ctx, "tools/list", map[string]string{"scope": "all"}, recipient)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(pool.published) != 1 {
		t.Fatalf("expected one event, got %d", len(pool.published))
	}
	evt := pool.published[0]
	if evt.Kind != KindContextVM {
		t.Fatalf("kind = %d", evt.Kind)
	}
	if !evt.CheckID() || !evt.VerifySignature() {
		t.Fatal("event was not signed correctly")
	}
	if id != evt.ID.Hex() {
		t.Fatalf("id %q != event id %q", id, evt.ID.Hex())
	}
	if !evt.Tags.ContainsAny("p", []string{recipient.Hex()}) {
		t.Fatalf("missing recipient tag: %+v", evt.Tags)
	}
	var msg Message
	if err := json.Unmarshal([]byte(evt.Content), &msg); err != nil {
		t.Fatalf("content json: %v", err)
	}
	if msg.JSONRPC != jsonRPCVersion || msg.ID != "" || msg.Method != "tools/list" {
		t.Fatalf("unexpected message: %+v", msg)
	}
}

func TestTransportSendReturnsPublishError(t *testing.T) {
	pool := &fakePool{pubErr: errors.New("blocked")}
	tr := NewTransport(pool, newTestSigner(1), nil, []string{"wss://write"}, nil)
	if _, err := tr.Send(context.Background(), "tools/list", nil); err == nil {
		t.Fatal("expected publish error")
	}
}

func TestTransportSubscribeDecodesAddressedEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &fakePool{events: make(chan nostr.RelayEvent, 1), closed: make(chan nostr.RelayClosed)}
	signer := newTestSigner(1)
	pubkey, _ := signer.GetPublicKey(ctx)
	tr := NewTransport(pool, signer, []string{"wss://read"}, nil, nil)

	requests, errs, err := tr.Subscribe(ctx)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(pool.filter.Kinds) != 1 || pool.filter.Kinds[0] != KindContextVM {
		t.Fatalf("unexpected filter kinds: %+v", pool.filter.Kinds)
	}
	if got := pool.filter.Tags["p"]; len(got) != 1 || got[0] != pubkey.Hex() {
		t.Fatalf("unexpected p filter: %+v", pool.filter.Tags)
	}

	msg := Message{JSONRPC: jsonRPCVersion, ID: "correlation-id", Method: "tools/call", Params: json.RawMessage(`{"name":"x"}`)}
	content, _ := json.Marshal(msg)
	evt := nostr.Event{CreatedAt: nostr.Now(), Kind: KindContextVM, Tags: nostr.Tags{nostr.Tag{"p", pubkey.Hex()}}, Content: string(content)}
	if err := signer.SignEvent(ctx, &evt); err != nil {
		t.Fatalf("sign: %v", err)
	}
	pool.events <- nostr.RelayEvent{Event: evt}

	select {
	case req := <-requests:
		if req.Msg.ID != "correlation-id" || req.Msg.Method != "tools/call" {
			t.Fatalf("unexpected request: %+v", req.Msg)
		}
	case err := <-errs:
		t.Fatalf("unexpected subscribe error: %v", err)
	case <-ctx.Done():
		t.Fatal("context ended before event")
	}
}
