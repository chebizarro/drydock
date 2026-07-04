package publisher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

func TestNostrRelayPublisherRetriesRetryableFailuresUntilQuorum(t *testing.T) {
	ctx := context.Background()
	event := nostr.Event{ID: nostr.MustIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}
	calls := [][]string{}
	attempt := 0
	pub := NewNostrRelayPublisher(nil, testLogger(),
		WithPublishQuorum(2),
		WithPublishRetry(2, 0),
		withPublishMany(func(_ context.Context, relays []string, _ nostr.Event) chan nostr.PublishResult {
			attempt++
			calls = append(calls, append([]string(nil), relays...))
			ch := make(chan nostr.PublishResult, len(relays))
			go func() {
				defer close(ch)
				switch attempt {
				case 1:
					ch <- nostr.PublishResult{RelayURL: "wss://a"}
					ch <- nostr.PublishResult{RelayURL: "wss://b", Error: errors.New("network: temporary failure")}
					ch <- nostr.PublishResult{RelayURL: "wss://c", Error: errors.New("invalid: bad signature")}
				case 2:
					ch <- nostr.PublishResult{RelayURL: "wss://b"}
				}
			}()
			return ch
		}),
	)

	if err := pub.Publish(ctx, []string{"wss://a", "wss://b", "wss://c"}, event); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(calls))
	}
	if strings.Join(calls[1], ",") != "wss://b" {
		t.Fatalf("expected retry only for relay b, got %v", calls[1])
	}
}

func TestNostrRelayPublisherFailsWhenQuorumUnmet(t *testing.T) {
	ctx := context.Background()
	event := nostr.Event{ID: nostr.MustIDFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")}
	pub := NewNostrRelayPublisher(nil, testLogger(),
		WithPublishQuorum(2),
		WithPublishRetry(1, 0),
		withPublishMany(func(_ context.Context, relays []string, _ nostr.Event) chan nostr.PublishResult {
			ch := make(chan nostr.PublishResult, len(relays))
			go func() {
				defer close(ch)
				ch <- nostr.PublishResult{RelayURL: "wss://a"}
				ch <- nostr.PublishResult{RelayURL: "wss://b", Error: errors.New("blocked: policy")}
			}()
			return ch
		}),
	)

	err := pub.Publish(ctx, []string{"wss://a", "wss://b"}, event)
	if err == nil || !strings.Contains(err.Error(), "publish quorum not met") {
		t.Fatalf("expected quorum error, got %v", err)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
