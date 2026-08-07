package listener

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"drydock/internal/db"
	"drydock/internal/eventkind"
	"drydock/internal/metrics"
	"drydock/internal/monitoring"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip59"
)

type EventProcessor interface {
	ProcessEvent(ctx context.Context, event nostr.Event, relayURL string) error
	ProcessGiftWrappedEvent(ctx context.Context, event nostr.Event, relayURL string) error
}

type GiftWrapOpener interface {
	OpenGiftWrap(ctx context.Context, wrapper nostr.Event) (nostr.Event, error)
}

// GiftWrapDecrypter is the NIP-44 capability required to open NIP-59 wraps.
type GiftWrapDecrypter interface {
	Decrypt(ctx context.Context, ciphertext string, sender nostr.PubKey) (string, error)
}

type nip59GiftWrapOpener struct {
	decrypter GiftWrapDecrypter
}

// NewNIP59GiftWrapOpener opens and verifies NIP-59 gift wraps with the
// configured signer's NIP-44 decryption capability.
func NewNIP59GiftWrapOpener(decrypter GiftWrapDecrypter) GiftWrapOpener {
	return &nip59GiftWrapOpener{decrypter: decrypter}
}

func (o *nip59GiftWrapOpener) OpenGiftWrap(ctx context.Context, wrapper nostr.Event) (nostr.Event, error) {
	if o == nil || o.decrypter == nil {
		return nostr.Event{}, errors.New("gift wrap decrypter is not configured")
	}
	return nip59.GiftUnwrap(wrapper, func(sender nostr.PubKey, ciphertext string) (string, error) {
		return o.decrypter.Decrypt(ctx, ciphertext, sender)
	})
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
	eventkind.ZapReceipt,
}

func SubscribedKinds() []nostr.Kind {
	return append([]nostr.Kind(nil), subscribedKinds...)
}

func subscriptionKinds(giftWrapEnabled bool) []nostr.Kind {
	kinds := make([]nostr.Kind, 0, len(subscribedKinds))
	for _, kind := range subscribedKinds {
		if kind == eventkind.GiftWrap && !giftWrapEnabled {
			continue
		}
		kinds = append(kinds, kind)
	}
	return kinds
}

func subscriptionFilters(since nostr.Timestamp, cfg Config) []nostr.Filter {
	kinds := subscriptionKinds(cfg.GiftWrapEnabled)
	pubkey := strings.TrimSpace(cfg.ContextVMPubkey)
	methods := make([]string, 0, len(cfg.ContextVMMethods))
	seenMethods := make(map[string]struct{}, len(cfg.ContextVMMethods))
	for _, method := range cfg.ContextVMMethods {
		method = strings.TrimSpace(method)
		if method == "" {
			continue
		}
		if _, ok := seenMethods[method]; ok {
			continue
		}
		seenMethods[method] = struct{}{}
		methods = append(methods, method)
	}

	filters := make([]nostr.Filter, 0, 4)
	if pubkey == "" || len(methods) == 0 {
		filters = append(filters, nostr.Filter{Kinds: kinds, Since: since})
	} else {
		generalKinds := make([]nostr.Kind, 0, len(kinds)-1)
		for _, kind := range kinds {
			if kind != eventkind.ContextVM {
				generalKinds = append(generalKinds, kind)
			}
		}
		filters = append(filters,
			nostr.Filter{Kinds: generalKinds, Since: since},
			nostr.Filter{
				Kinds: []nostr.Kind{eventkind.ContextVM},
				Tags:  nostr.TagMap{"p": {pubkey}, "method": methods},
				Since: since,
			},
		)
	}

	operator, err := nostr.PubKeyFromHex(strings.TrimSpace(cfg.MonitoredReposAuthor))
	if err == nil && operator != nostr.ZeroPK {
		listAddress := monitoring.ListAddress(operator.Hex())
		// These control-plane filters intentionally have no Since. They must
		// recover the current replaceable list independently of the event HWM.
		filters = append(filters,
			nostr.Filter{
				Kinds:   []nostr.Kind{eventkind.MonitoredRepositories},
				Authors: []nostr.PubKey{operator},
				Tags:    nostr.TagMap{"d": {monitoring.ListIdentifier}},
			},
			nostr.Filter{
				Kinds:   []nostr.Kind{eventkind.Deletion},
				Authors: []nostr.PubKey{operator},
				Tags:    nostr.TagMap{"a": {listAddress}},
			},
		)
	}
	return filters
}

type Config struct {
	Relays               []string
	LookbackMinutes      int
	HighWaterMarkOverlap time.Duration
	CatchupMaxAge        time.Duration
	MaxFutureSkew        time.Duration
	GiftWrapEnabled      bool
	ContextVMPubkey      string
	ContextVMMethods     []string
	MonitoredReposAuthor string
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
	if s.cfg.GiftWrapEnabled && s.opener == nil {
		return errors.New("gift wrap subscription enabled without an opener")
	}

	lookback := s.cfg.LookbackMinutes
	if lookback <= 0 {
		lookback = 5
	}
	overlap := s.cfg.HighWaterMarkOverlap
	if overlap <= 0 {
		overlap = 30 * time.Second
	}
	catchupMaxAge := s.cfg.CatchupMaxAge
	if catchupMaxAge <= 0 {
		catchupMaxAge = 365 * 24 * time.Hour
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
			since, used = subscriptionSince(now, hwm, time.Duration(lookback)*time.Minute, overlap, catchupMaxAge, maxFutureSkew)
			if used {
				s.logger.Info("using persisted high-water-mark for catch-up",
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

	filters := subscriptionFilters(nostr.Timestamp(since), s.cfg)

	s.logger.Info("starting nostr listener", "relay_count", len(s.cfg.Relays))

	var lastSeen atomic.Int64
	backoff := time.Second

	for {
		var stream chan nostr.RelayEvent
		var closedCh chan nostr.RelayClosed
		if len(filters) == 1 {
			stream, closedCh = s.pool.SubscribeManyNotifyClosed(ctx, s.cfg.Relays, filters[0], nostr.SubscriptionOptions{
				Label: "drydock-listener",
			})
		} else {
			directed := make([]nostr.DirectedFilter, 0, len(filters)*len(s.cfg.Relays))
			for _, relay := range s.cfg.Relays {
				for _, filter := range filters {
					directed = append(directed, nostr.DirectedFilter{Relay: relay, Filter: filter})
				}
			}
			stream, closedCh = s.pool.BatchedSubscribeManyNotifyClosed(ctx, directed, nostr.SubscriptionOptions{
				Label: "drydock-listener",
			})
		}

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
	giftWrapped := false
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
		giftWrapped = true
	}

	var err error
	if giftWrapped {
		err = s.processor.ProcessGiftWrappedEvent(ctx, event, relayURL)
	} else {
		err = s.processor.ProcessEvent(ctx, event, relayURL)
	}
	if err != nil {
		s.logger.Error("failed to process event", "event_id", event.ID.Hex(), "kind", int(event.Kind), "relay", relayURL, "error", err)
		return
	}

	// Track high-water-mark for restart resilience only after successful processing.
	if s.store != nil {
		maxFutureSkew := s.cfg.MaxFutureSkew
		if maxFutureSkew <= 0 {
			maxFutureSkew = 10 * time.Minute
		}
		ts, ok := checkpointTimestamp(time.Now(), int64(ie.Event.CreatedAt), maxFutureSkew)
		if !ok {
			s.logger.Warn("refusing to checkpoint implausible future event timestamp",
				"event_id", ie.Event.ID.Hex(),
				"high_water_mark", int64(ie.Event.CreatedAt),
				"max_future_skew", maxFutureSkew.String(),
			)
			return
		}
		if ts > lastSeen.Load() && s.persistHighWaterMark(ctx, ts) {
			lastSeen.Store(ts)
		}
	}
}

func checkpointTimestamp(now time.Time, eventTimestamp int64, maxFutureSkew time.Duration) (int64, bool) {
	if eventTimestamp > now.Add(maxFutureSkew).Unix() {
		return 0, false
	}
	if eventTimestamp > now.Unix() {
		return now.Unix(), true
	}
	return eventTimestamp, true
}

func subscriptionSince(now time.Time, highWaterMark int64, lookback, overlap, catchupMaxAge, maxFutureSkew time.Duration) (int64, bool) {
	freshStart := now.Add(-lookback).Unix()
	if highWaterMark > now.Add(maxFutureSkew).Unix() {
		return freshStart, false
	}
	if highWaterMark > now.Unix() {
		highWaterMark = now.Unix()
	}
	since := highWaterMark - int64(overlap/time.Second)
	catchupBound := now.Add(-catchupMaxAge).Unix()
	if since < catchupBound {
		since = catchupBound
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
