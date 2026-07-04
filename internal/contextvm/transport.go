package contextvm

import (
	"context"
	"errors"
	"fmt"

	"fiatjaf.com/nostr"
)

// RelayPublisher is implemented by publisher.NostrRelayPublisher and test fakes.
type RelayPublisher interface {
	Publish(ctx context.Context, relays []string, event nostr.Event) error
}

// Transport sends private ContextVM payloads as NIP-59 gift wraps.
type Transport struct {
	signer  nostr.Keyer
	publish RelayPublisher
	relays  []string
}

func NewTransport(signer nostr.Keyer, publish RelayPublisher, relays []string) *Transport {
	return &Transport{
		signer:  signer,
		publish: publish,
		relays:  append([]string(nil), relays...),
	}
}

// SendPrivate wraps content in a signed kind-25910 inner event and publishes the
// encrypted kind-1059 wrapper. It returns wrapper and inner IDs for correlation.
func (t *Transport) SendPrivate(ctx context.Context, recipient nostr.PubKey, content string, tags nostr.Tags) (wrapperID string, innerID string, err error) {
	if t == nil {
		return "", "", errors.New("transport is nil")
	}
	if t.signer == nil {
		return "", "", errors.New("transport signer is required")
	}
	if t.publish == nil {
		return "", "", errors.New("transport publisher is required")
	}
	if len(t.relays) == 0 {
		return "", "", errors.New("transport relays are required")
	}

	wrapper, inner, err := GiftWrap(ctx, t.signer, recipient, content, tags)
	if err != nil {
		return "", "", err
	}
	if err := t.publish.Publish(ctx, t.relays, wrapper); err != nil {
		return "", "", fmt.Errorf("publish ContextVM gift wrap: %w", err)
	}
	return wrapper.ID.Hex(), inner.ID.Hex(), nil
}

// SendPrivateResponse publishes an encrypted response correlated to an incoming
// ContextVM event via an e-tag while preserving the recipient p-tag.
func (t *Transport) SendPrivateResponse(ctx context.Context, incoming nostr.Event, content string) (wrapperID string, innerID string, err error) {
	tags := nostr.Tags{
		{"p", incoming.PubKey.Hex()},
		{"e", incoming.ID.Hex()},
	}
	return t.SendPrivate(ctx, incoming.PubKey, content, tags)
}

// GiftWrapOpener adapts a recipient keyer for listener routing.
type GiftWrapOpener struct {
	Recipient nostr.Keyer
}

func (o GiftWrapOpener) OpenGiftWrap(ctx context.Context, wrapper nostr.Event) (nostr.Event, error) {
	return OpenGiftWrap(ctx, o.Recipient, wrapper)
}
