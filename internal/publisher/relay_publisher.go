package publisher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"fiatjaf.com/nostr"
)

type NostrRelayPublisher struct {
	pool   *nostr.Pool
	logger *slog.Logger
}

// NewNostrRelayPublisher creates a publisher. If pool is nil, a new pool is created.
func NewNostrRelayPublisher(pool *nostr.Pool, logger *slog.Logger) *NostrRelayPublisher {
	if pool == nil {
		pool = nostr.NewPool(nostr.PoolOptions{})
	}
	return &NostrRelayPublisher{
		pool:   pool,
		logger: logger,
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
			reason := res.Error.Error()
			switch {
			case strings.Contains(reason, "duplicate:"):
				// Already stored on this relay — treat as success.
				p.logger.Debug("relay reported duplicate",
					"relay", res.RelayURL,
					"event_id", event.ID.Hex(),
				)
				success++
			case strings.Contains(reason, "rate-limited:"):
				p.logger.Warn("relay rate-limited publish",
					"relay", res.RelayURL,
					"event_id", event.ID.Hex(),
					"reason", reason,
				)
				errs = append(errs, fmt.Sprintf("%s: %s", res.RelayURL, reason))
			case strings.Contains(reason, "blocked:"):
				p.logger.Warn("relay blocked publish",
					"relay", res.RelayURL,
					"event_id", event.ID.Hex(),
					"reason", reason,
				)
				errs = append(errs, fmt.Sprintf("%s: %s", res.RelayURL, reason))
			case strings.Contains(reason, "invalid:"):
				p.logger.Error("relay rejected event as invalid",
					"relay", res.RelayURL,
					"event_id", event.ID.Hex(),
					"reason", reason,
				)
				errs = append(errs, fmt.Sprintf("%s: %s", res.RelayURL, reason))
			default:
				errs = append(errs, fmt.Sprintf("%s: %v", res.RelayURL, res.Error))
			}
			continue
		}
		success++
	}
	if success > 0 {
		return nil
	}
	return fmt.Errorf("publish failed on all relays: %s", strings.Join(errs, "; "))
}
