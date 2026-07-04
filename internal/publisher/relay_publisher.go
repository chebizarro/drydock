package publisher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"fiatjaf.com/nostr"
)

type publishManyFunc func(context.Context, []string, nostr.Event) chan nostr.PublishResult

type NostrRelayPublisher struct {
	pool        *nostr.Pool
	logger      *slog.Logger
	quorum      int
	maxAttempts int
	backoff     time.Duration
	publishMany publishManyFunc
}

// RelayPublisherOption configures relay publishing reliability behavior.
type RelayPublisherOption func(*NostrRelayPublisher)

// WithPublishQuorum requires at least quorum relay OKs before Publish succeeds.
func WithPublishQuorum(quorum int) RelayPublisherOption {
	return func(p *NostrRelayPublisher) {
		p.quorum = quorum
	}
}

// WithPublishRetry configures retry attempts and exponential backoff for retryable failures.
func WithPublishRetry(maxAttempts int, backoff time.Duration) RelayPublisherOption {
	return func(p *NostrRelayPublisher) {
		p.maxAttempts = maxAttempts
		p.backoff = backoff
	}
}

// withPublishMany injects PublishMany for deterministic unit tests.
func withPublishMany(fn publishManyFunc) RelayPublisherOption {
	return func(p *NostrRelayPublisher) {
		p.publishMany = fn
	}
}

// NewNostrRelayPublisher creates a publisher. If pool is nil, a new pool is created.
func NewNostrRelayPublisher(pool *nostr.Pool, logger *slog.Logger, opts ...RelayPublisherOption) *NostrRelayPublisher {
	if pool == nil {
		pool = nostr.NewPool(nostr.PoolOptions{})
	}
	if logger == nil {
		logger = slog.Default()
	}
	p := &NostrRelayPublisher{
		pool:        pool,
		logger:      logger,
		quorum:      1,
		maxAttempts: 3,
		backoff:     100 * time.Millisecond,
	}
	p.publishMany = p.pool.PublishMany
	for _, opt := range opts {
		opt(p)
	}
	if p.quorum <= 0 {
		p.quorum = 1
	}
	if p.maxAttempts <= 0 {
		p.maxAttempts = 1
	}
	if p.backoff < 0 {
		p.backoff = 0
	}
	return p
}

func (p *NostrRelayPublisher) Publish(ctx context.Context, relays []string, event nostr.Event) error {
	relays = dedupeRelayURLs(relays)
	if len(relays) == 0 {
		return errors.New("no relays provided")
	}
	quorum := p.quorum
	if quorum > len(relays) {
		quorum = len(relays)
	}

	pending := append([]string(nil), relays...)
	success := 0
	var errs []string

	for attempt := 1; attempt <= p.maxAttempts && len(pending) > 0; attempt++ {
		nextPending := make([]string, 0, len(pending))
		for res := range p.publishMany(ctx, pending, event) {
			if res.Error == nil || isDuplicateOK(res.Error) {
				if res.Error != nil {
					p.logger.Debug("relay reported duplicate",
						"relay", res.RelayURL,
						"event_id", event.ID.Hex(),
					)
				}
				success++
				continue
			}

			reason := res.Error.Error()
			errs = append(errs, fmt.Sprintf("%s: %s", res.RelayURL, reason))
			logPublishFailure(p.logger, res.RelayURL, event.ID.Hex(), reason)
			if isRetryablePublishError(res.Error) && attempt < p.maxAttempts {
				nextPending = append(nextPending, res.RelayURL)
			}
		}

		if success >= quorum {
			return nil
		}
		pending = dedupeRelayURLs(nextPending)
		if len(pending) == 0 {
			break
		}
		if err := sleepBackoff(ctx, p.backoff, attempt); err != nil {
			return err
		}
	}

	return fmt.Errorf("publish quorum not met: successes=%d quorum=%d relays=%d errors=%s", success, quorum, len(relays), strings.Join(errs, "; "))
}

func isDuplicateOK(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate:")
}

func isRetryablePublishError(err error) bool {
	if err == nil {
		return false
	}
	reason := err.Error()
	if strings.Contains(reason, "invalid:") || strings.Contains(reason, "blocked:") {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

func logPublishFailure(logger *slog.Logger, relayURL, eventID, reason string) {
	switch {
	case strings.Contains(reason, "rate-limited:"):
		logger.Warn("relay rate-limited publish", "relay", relayURL, "event_id", eventID, "reason", reason)
	case strings.Contains(reason, "blocked:"):
		logger.Warn("relay blocked publish", "relay", relayURL, "event_id", eventID, "reason", reason)
	case strings.Contains(reason, "invalid:"):
		logger.Error("relay rejected event as invalid", "relay", relayURL, "event_id", eventID, "reason", reason)
	default:
		logger.Warn("relay publish failed", "relay", relayURL, "event_id", eventID, "reason", reason)
	}
}

func sleepBackoff(ctx context.Context, base time.Duration, attempt int) error {
	if base == 0 {
		return nil
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func dedupeRelayURLs(relays []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(relays))
	for _, relay := range relays {
		relay = strings.TrimSpace(relay)
		if relay == "" {
			continue
		}
		if _, ok := seen[relay]; ok {
			continue
		}
		seen[relay] = struct{}{}
		out = append(out, relay)
	}
	return out
}
