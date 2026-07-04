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

type GiftWrapOpener interface {
	OpenGiftWrap(ctx context.Context, wrapper nostr.Event) (nostr.Event, error)
}

type highWaterStore interface {
	GetListenerHighWaterMark(ctx context.Context) (int64, error)
	UpdateListenerHighWaterMark(ctx context.Context, ts int64) error
}

var subscribedKinds = []nostr.Kind{
	30617, 30618,
	1617, 1618, 1619,
	1621, nostr.KindComment,
	1630, 1631, 1632, 1633,
	1985,
	nostr.KindEncryptedDirectMessage, // NIP-04 DMs (kind 4)
	14,                               // NIP-17 sealed DMs
	1059,                             // NIP-59 gift wraps for private ContextVM payloads
	31650,                            // IDE workspace session
	1651,                             // IDE review request
	1653,                             // IDE fix request
	30620,                            // Reviewer profile (replaceable)
	1660,                             // Review assignment
	1661,                             // Assignment acceptance
	1662,                             // Assignment rejection
	1663,                             // Review feedback
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
	store     highWaterStore
	opener    GiftWrapOpener
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

// WithGiftWrapOpener injects NIP-59 opening/verification for kind-1059 events.
func WithGiftWrapOpener(opener GiftWrapOpener) Option {
	return func(s *Service) {
		s.opener = opener
	}
}

func New(cfg Config, processor EventProcessor, logger *slog.Logger, opts ...Option) *Service {
	if logger == nil {
		logger = slog.Default()
	}
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
			// Choose the MORE RECENT timestamp (larger unix timestamp) to avoid
			// re-processing events we've already seen, while still respecting
			// the lookback window for initial startup.
			hwmWithOverlap := hwm - 30
			if hwmWithOverlap > since {
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

	var lastSeen atomic.Int64
	backoff := time.Second

	for {
		stream, closedCh := s.pool.SubscribeManyNotifyClosed(ctx, s.cfg.Relays, filter, nostr.SubscriptionOptions{
			Label: "drydock-listener",
		})

		streamEnded := false
		for !streamEnded {
			select {
			case <-ctx.Done():
				s.pool.Close("shutdown")
				return nil
			case closed, ok := <-closedCh:
				if !ok {
					closedCh = nil
					continue
				}
				s.logClosed(closed)
			case ie, ok := <-stream:
				if !ok {
					streamEnded = true
					break
				}
				backoff = time.Second
				s.processRelayEvent(ctx, ie, &lastSeen)
			}
		}

		select {
		case <-ctx.Done():
			s.pool.Close("shutdown")
			return nil
		case <-time.After(backoff):
			s.logger.Warn("listener stream ended; resubscribing", "backoff", backoff.String())
			if backoff < time.Minute {
				backoff *= 2
				if backoff > time.Minute {
					backoff = time.Minute
				}
			}
		}
	}
}

func (s *Service) logClosed(closed nostr.RelayClosed) {
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

func (s *Service) processRelayEvent(ctx context.Context, ie nostr.RelayEvent, lastSeen *atomic.Int64) {
	relayURL := ""
	if ie.Relay != nil {
		relayURL = ie.Relay.URL
	}
	event := ie.Event
	if event.Kind == 1059 {
		if s.opener == nil {
			s.logger.Warn("dropping gift wrap without configured opener", "event_id", event.ID.Hex(), "relay", relayURL)
			return
		}
		opened, err := s.opener.OpenGiftWrap(ctx, event)
		if err != nil {
			s.logger.Warn("failed to open gift wrap", "event_id", event.ID.Hex(), "relay", relayURL, "error", err)
			return
		}
		event = opened
	}

	if err := s.processor.ProcessEvent(ctx, event, relayURL); err != nil {
		s.logger.Error("failed to process event", "event_id", event.ID.Hex(), "kind", int(event.Kind), "relay", relayURL, "error", err)
		return
	}

	// Track high-water-mark for restart resilience only after successful processing.
	if s.store != nil {
		ts := int64(ie.Event.CreatedAt)
		if ts > lastSeen.Load() {
			lastSeen.Store(ts)
			_ = s.store.UpdateListenerHighWaterMark(ctx, ts)
		}
	}
}
