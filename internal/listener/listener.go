package listener

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"drydock/internal/db"

	"fiatjaf.com/nostr"
)

type EventProcessor interface {
	ProcessEvent(ctx context.Context, event nostr.Event, relayURL string) error
}

var subscribedKinds = []nostr.Kind{
	30617, 30618,
	1617, 1618, 1619,
	1621, nostr.KindComment,
	1630, 1631, 1632, 1633,
	1985,
	nostr.KindEncryptedDirectMessage, // NIP-04 DMs (kind 4)
	14,                                // NIP-17 sealed DMs
	31650,                             // IDE workspace session
	1651,                              // IDE review request
	1653,                              // IDE fix request
	30620,                             // Reviewer profile (replaceable)
	1660,                              // Review assignment
	1661,                              // Assignment acceptance
	1662,                              // Assignment rejection
	1663,                              // Review feedback
}

func SubscribedKinds() []nostr.Kind {
	return append([]nostr.Kind(nil), subscribedKinds...)
}

type Config struct {
	Relays          []string
	LookbackMinutes int
}

type Service struct {
	cfg       Config
	processor EventProcessor
	logger    *slog.Logger
	pool      *nostr.Pool
	store     *db.Store
}

// Option is a functional option for the listener Service.
type Option func(*Service)

// WithPool injects a shared nostr.Pool instead of creating a new one.
func WithPool(pool *nostr.Pool) Option {
	return func(s *Service) {
		s.pool = pool
	}
}

// WithStore injects a DB store for persisting listener state (e.g. high-water-mark).
func WithStore(store *db.Store) Option {
	return func(s *Service) {
		s.store = store
	}
}

func New(cfg Config, processor EventProcessor, logger *slog.Logger, opts ...Option) *Service {
	s := &Service{
		cfg:       cfg,
		processor: processor,
		logger:    logger,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.pool == nil {
		s.pool = nostr.NewPool(nostr.PoolOptions{})
	}
	return s
}

func (s *Service) Run(ctx context.Context) error {
	if len(s.cfg.Relays) == 0 {
		s.logger.Warn("no relays configured; listener is idle")
		<-ctx.Done()
		return nil
	}

	lookback := s.cfg.LookbackMinutes
	if lookback <= 0 {
		lookback = 5
	}

	// Determine Since: use persisted high-water-mark if available, else lookback.
	since := time.Now().Add(-time.Duration(lookback) * time.Minute).Unix()
	if s.store != nil {
		if hwm, err := s.store.GetListenerHighWaterMark(ctx); err == nil && hwm > 0 {
			// Use high-water-mark with a small overlap to handle clock skew.
			hwmWithOverlap := hwm - 30
			if hwmWithOverlap < since {
				since = hwmWithOverlap
			}
			s.logger.Info("using persisted high-water-mark for lookback",
				"high_water_mark", hwm,
				"since", since,
			)
		}
	}

	filter := nostr.Filter{
		Kinds: append([]nostr.Kind(nil), subscribedKinds...),
		Since: nostr.Timestamp(since),
	}

	s.logger.Info("starting nostr listener", "relay_count", len(s.cfg.Relays))

	// SubscribeManyNotifyClosed gives us visibility into relay CLOSED reasons (NIP-01)
	// while maintaining long-lived reconnectable subscriptions.
	// NOTE: the pool does not currently expose a combined EOSE+CLOSED notification method.
	// EOSE is still handled internally by the pool for dedup purposes; when the library
	// adds a combined API we should switch to it for application-level EOSE logging.
	stream, closedCh := s.pool.SubscribeManyNotifyClosed(ctx, s.cfg.Relays, filter, nostr.SubscriptionOptions{
		Label: "drydock-listener",
	})

	var lastSeen atomic.Int64

	// Log relay CLOSED reasons in a background goroutine.
	go func() {
		for closed := range closedCh {
			relayURL := ""
			if closed.Relay != nil {
				relayURL = closed.Relay.URL
			}
			if closed.HandledAuth {
				s.logger.Info("relay required auth and was re-authenticated",
					"relay", relayURL,
					"reason", closed.Reason,
				)
			} else {
				s.logger.Warn("relay subscription closed",
					"relay", relayURL,
					"reason", closed.Reason,
				)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			s.pool.Close("shutdown")
			return nil
		case ie, ok := <-stream:
			if !ok {
				return nil
			}
			relayURL := ""
			if ie.Relay != nil {
				relayURL = ie.Relay.URL
			}
			if err := s.processor.ProcessEvent(ctx, ie.Event, relayURL); err != nil {
				s.logger.Error("failed to process event", "event_id", ie.Event.ID.Hex(), "kind", int(ie.Event.Kind), "relay", relayURL, "error", err)
			}

			// Track high-water-mark for restart resilience.
			if s.store != nil {
				ts := int64(ie.Event.CreatedAt)
				if ts > lastSeen.Load() {
					lastSeen.Store(ts)
					_ = s.store.UpdateListenerHighWaterMark(ctx, ts)
				}
			}
		}
	}
}
