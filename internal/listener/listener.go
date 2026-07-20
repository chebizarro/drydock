package listener

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"drydock/internal/db"
	"drydock/internal/eventkind"
	"drydock/internal/metrics"

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
	ResetListenerHighWaterMark(ctx context.Context, ts int64) error
}

var ListenerCheckpointPersistFailures = &metrics.Counter{}

const (
	checkpointPersistAttempts = 3
	checkpointRetryBackoff    = 10 * time.Millisecond
)

var subscribedKinds = []nostr.Kind{
	eventkind.RepositoryAnnouncement, eventkind.RepositoryState,
	eventkind.Patch, eventkind.GitPullRequest, eventkind.GitPullRequestUpdate,
	eventkind.Issue, eventkind.Comment,
	eventkind.StatusOpen, eventkind.StatusApplied, eventkind.StatusClosed, eventkind.StatusDraft,
	eventkind.Label,
	eventkind.EncryptedDirectMessage,
	eventkind.SealedDirectMessage,
	eventkind.GiftWrap,
	eventkind.IDESession,
	eventkind.ContextVM,
	eventkind.ReviewerProfile,
	eventkind.ReviewFeedback,
	eventkind.ZapReceipt,
}

func SubscribedKinds() []nostr.Kind {
	return append([]nostr.Kind(nil), subscribedKinds...)
}

type Config struct {
	Relays               []string
	LookbackMinutes      int
	HighWaterMarkOverlap time.Duration
	MaxFutureSkew        time.Duration
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
		s.pool = nostr.NewPool()
	}
	return s
}

func (s *Service) Run(ctx context.Context) error {
	if len(s.cfg.Relays) == 0 {
		return errors.New("no relays configured")
	}

	lookback := s.cfg.LookbackMinutes
	if lookback <= 0 {
		lookback = 5
	}
	overlap := s.cfg.HighWaterMarkOverlap
	if overlap <= 0 {
		overlap = 30 * time.Second
	}

	maxFutureSkew := s.cfg.MaxFutureSkew
	if maxFutureSkew <= 0 {
		maxFutureSkew = 10 * time.Minute
	}

	// Determine Since: use a plausible persisted high-water-mark if available,
	// else recover from the configured lookback window.
	now := time.Now()
	since := now.Add(-time.Duration(lookback) * time.Minute).Unix()
	if s.store != nil {
		if hwm, err := s.store.GetListenerHighWaterMark(ctx); err == nil && hwm > 0 {
			var used bool
			since, used = subscriptionSince(now, hwm, time.Duration(lookback)*time.Minute, overlap, maxFutureSkew)
			if used {
				s.logger.Info("using persisted high-water-mark for lookback",
					"high_water_mark", hwm,
					"since", since,
				)
			} else {
				s.logger.Warn("ignoring implausible future listener high-water-mark",
					"high_water_mark", hwm,
					"max_allowed", now.Add(maxFutureSkew).Unix(),
					"since", since,
				)
				if err := s.store.ResetListenerHighWaterMark(ctx, since); err != nil {
					s.logger.Error("failed to reset implausible future listener high-water-mark",
						"high_water_mark", hwm,
						"reset_to", since,
						"error", err,
					)
				} else {
					s.logger.Info("reset implausible future listener high-water-mark",
						"previous", hwm,
						"high_water_mark", since,
					)
				}
			}
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
		maxFutureSkew := s.cfg.MaxFutureSkew
		if maxFutureSkew <= 0 {
			maxFutureSkew = 10 * time.Minute
		}
		if ts > time.Now().Add(maxFutureSkew).Unix() {
			s.logger.Warn("refusing to checkpoint implausible future event timestamp",
				"event_id", ie.Event.ID.Hex(),
				"high_water_mark", ts,
				"max_future_skew", maxFutureSkew.String(),
			)
			return
		}
		if ts > lastSeen.Load() && s.persistHighWaterMark(ctx, ts) {
			lastSeen.Store(ts)
		}
	}
}

func subscriptionSince(now time.Time, highWaterMark int64, lookback, overlap, maxFutureSkew time.Duration) (int64, bool) {
	since := now.Add(-lookback).Unix()
	if highWaterMark > now.Add(maxFutureSkew).Unix() {
		return since, false
	}
	hwmWithOverlap := highWaterMark - int64(overlap/time.Second)
	if hwmWithOverlap > since {
		since = hwmWithOverlap
	}
	return since, true
}

func (s *Service) persistHighWaterMark(ctx context.Context, ts int64) bool {
	var err error
	for attempt := 1; attempt <= checkpointPersistAttempts; attempt++ {
		err = s.store.UpdateListenerHighWaterMark(ctx, ts)
		if err == nil {
			return true
		}
		if attempt == checkpointPersistAttempts {
			break
		}

		timer := time.NewTimer(time.Duration(attempt) * checkpointRetryBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			err = errors.Join(err, ctx.Err())
			attempt = checkpointPersistAttempts
		case <-timer.C:
		}
	}

	ListenerCheckpointPersistFailures.Inc()
	s.logger.Error("failed to persist listener high-water-mark",
		"high_water_mark", ts,
		"attempts", checkpointPersistAttempts,
		"error", err,
	)
	return false
}
