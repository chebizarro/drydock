package publisher

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"fiatjaf.com/nostr"
)

type NostrRelayPublisher struct {
	pool *nostr.Pool
}

func NewNostrRelayPublisher() *NostrRelayPublisher {
	return &NostrRelayPublisher{
		pool: nostr.NewPool(nostr.PoolOptions{}),
	}
}

func (p *NostrRelayPublisher) Publish(ctx context.Context, relays []string, event nostr.Event) error {
	if len(relays) == 0 {
		return errors.New("no relays provided")
	}
	var errs []string
	success := 0
	for res := range p.pool.PublishMany(ctx, relays, event) {
		if res.Error != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", res.RelayURL, res.Error))
			continue
		}
		success++
	}
	if success > 0 {
		return nil
	}
	return fmt.Errorf("publish failed on all relays: %s", strings.Join(errs, "; "))
}

