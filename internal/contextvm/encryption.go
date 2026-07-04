package contextvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
)

const (
	// KindContextVM is the private ContextVM payload kind carried inside NIP-59 gift wraps.
	KindContextVM nostr.Kind = 25910
	// KindGiftWrap is the NIP-59 gift-wrap event kind.
	KindGiftWrap nostr.Kind = 1059
)

const (
	maxEventFutureSkew = 10 * time.Minute
	maxEventPastAge    = 365 * 24 * time.Hour
)

var (
	ErrInvalidGiftWrap   = errors.New("invalid gift wrap")
	ErrRecipientMismatch = errors.New("gift wrap recipient mismatch")
	ErrInvalidInnerEvent = errors.New("invalid gift wrap inner event")
)

// GiftWrap creates a signed kind-25910 ContextVM event, encrypts it to recipient
// using NIP-44 from a fresh ephemeral key, and returns a signed kind-1059 wrapper.
//
// The returned inner event is provided for audit/testing only; callers should
// publish the wrapper event.
func GiftWrap(ctx context.Context, sender nostr.Keyer, recipient nostr.PubKey, content string, tags nostr.Tags) (wrapper nostr.Event, inner nostr.Event, err error) {
	if sender == nil {
		return nostr.Event{}, nostr.Event{}, errors.New("sender keyer is required")
	}
	if recipient == nostr.ZeroPK {
		return nostr.Event{}, nostr.Event{}, errors.New("recipient pubkey is required")
	}

	innerTags := append(nostr.Tags(nil), tags...)
	if !tagsContainPubKey(innerTags, recipient) {
		innerTags = append(innerTags, nostr.Tag{"p", recipient.Hex()})
	}
	inner = nostr.Event{
		Kind:      KindContextVM,
		CreatedAt: nostr.Now(),
		Tags:      innerTags,
		Content:   content,
	}
	if err := sender.SignEvent(ctx, &inner); err != nil {
		return nostr.Event{}, nostr.Event{}, fmt.Errorf("sign inner ContextVM event: %w", err)
	}

	plaintext, err := json.Marshal(inner)
	if err != nil {
		return nostr.Event{}, nostr.Event{}, fmt.Errorf("encode inner ContextVM event: %w", err)
	}

	ephemeral := keyer.NewPlainKeySigner(nostr.Generate())
	ciphertext, err := ephemeral.Encrypt(ctx, string(plaintext), recipient)
	if err != nil {
		return nostr.Event{}, nostr.Event{}, fmt.Errorf("encrypt gift wrap: %w", err)
	}

	wrapper = nostr.Event{
		Kind:      KindGiftWrap,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"p", recipient.Hex()}},
		Content:   ciphertext,
	}
	if err := ephemeral.SignEvent(ctx, &wrapper); err != nil {
		return nostr.Event{}, nostr.Event{}, fmt.Errorf("sign gift wrap: %w", err)
	}
	return wrapper, inner, nil
}

// OpenGiftWrap validates a kind-1059 wrapper is addressed to recipient, decrypts
// its NIP-44 content, decodes the inner kind-25910 event, and verifies the inner
// event ID/signature/timestamp before returning it.
func OpenGiftWrap(ctx context.Context, recipient nostr.Keyer, wrapper nostr.Event) (nostr.Event, error) {
	if recipient == nil {
		return nostr.Event{}, errors.New("recipient keyer is required")
	}
	recipientPubKey, err := recipient.GetPublicKey(ctx)
	if err != nil {
		return nostr.Event{}, fmt.Errorf("get recipient pubkey: %w", err)
	}
	if err := validateWrapper(wrapper, recipientPubKey); err != nil {
		return nostr.Event{}, err
	}

	plaintext, err := recipient.Decrypt(ctx, wrapper.Content, wrapper.PubKey)
	if err != nil {
		return nostr.Event{}, fmt.Errorf("decrypt gift wrap: %w", err)
	}

	var inner nostr.Event
	if err := json.Unmarshal([]byte(plaintext), &inner); err != nil {
		return nostr.Event{}, fmt.Errorf("decode inner ContextVM event: %w", err)
	}
	if err := validateInnerEvent(inner, recipientPubKey); err != nil {
		return nostr.Event{}, err
	}
	return inner, nil
}

func validateWrapper(wrapper nostr.Event, recipient nostr.PubKey) error {
	switch {
	case wrapper.Kind != KindGiftWrap:
		return fmt.Errorf("%w: expected kind %d, got %d", ErrInvalidGiftWrap, KindGiftWrap, wrapper.Kind)
	case !wrapper.CheckID():
		return fmt.Errorf("%w: wrapper id mismatch", ErrInvalidGiftWrap)
	case !wrapper.VerifySignature():
		return fmt.Errorf("%w: wrapper signature invalid", ErrInvalidGiftWrap)
	case !eventTimestampPlausible(wrapper.CreatedAt):
		return fmt.Errorf("%w: wrapper timestamp implausible", ErrInvalidGiftWrap)
	case !tagsContainPubKey(wrapper.Tags, recipient):
		return ErrRecipientMismatch
	}
	return nil
}

func validateInnerEvent(inner nostr.Event, recipient nostr.PubKey) error {
	switch {
	case inner.Kind != KindContextVM:
		return fmt.Errorf("%w: expected kind %d, got %d", ErrInvalidInnerEvent, KindContextVM, inner.Kind)
	case !inner.CheckID():
		return fmt.Errorf("%w: inner id mismatch", ErrInvalidInnerEvent)
	case !inner.VerifySignature():
		return fmt.Errorf("%w: inner signature invalid", ErrInvalidInnerEvent)
	case !eventTimestampPlausible(inner.CreatedAt):
		return fmt.Errorf("%w: inner timestamp implausible", ErrInvalidInnerEvent)
	case !tagsContainPubKey(inner.Tags, recipient):
		return ErrRecipientMismatch
	}
	return nil
}

func tagsContainPubKey(tags nostr.Tags, pubkey nostr.PubKey) bool {
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == "p" && tag[1] == pubkey.Hex() {
			return true
		}
	}
	return false
}

func eventTimestampPlausible(ts nostr.Timestamp) bool {
	now := time.Now()
	createdAt := time.Unix(int64(ts), 0)
	if createdAt.After(now.Add(maxEventFutureSkew)) {
		return false
	}
	if createdAt.Before(now.Add(-maxEventPastAge)) {
		return false
	}
	return true
}
