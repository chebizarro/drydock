package listener

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
	"git.sharegap.net/cascadia/drydock/internal/metrics"
	"git.sharegap.net/cascadia/drydock/internal/monitoring"
	"git.sharegap.net/cascadia/drydock/internal/relayroute"
	"git.sharegap.net/cascadia/drydock/internal/scope"

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

	// Maintainer-authored lists name this service in a p tag, which keeps the
	// discovery filter scoped without knowing the authors in advance. No Since:
	// control-plane filters must recover the current replaceable lists
	// independently of the event HWM.
	if service, err := nostr.PubKeyFromHex(strings.TrimSpace(cfg.ContextVMPubkey)); err == nil && service != nostr.ZeroPK {
		filters = append(filters, nostr.Filter{
			Kinds: []nostr.Kind{eventkind.MonitoredRepositories},
			Tags:  nostr.TagMap{"d": {monitoring.ListIdentifier}, "p": {service.Hex()}},
		})
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
}

// RoutePlanner supplies live routing inputs (customer relays, declared
// repositories, known list authors) and signals when they may have changed.
type RoutePlanner interface {
	Routes(ctx context.Context) (relayroute.Routes, error)
	Changes() <-chan struct{}
}

// routeChangeDebounce coalesces bursts of control-plane changes into one
// resubscription; routeRetryInterval re-attempts a failed route load.
const (
	routeChangeDebounce = 2 * time.Second
	routeRetryInterval  = 30 * time.Second
)

type Service struct {
	cfg       Config
	processor EventProcessor
	logger    *slog.Logger
	pool      *nostr.Pool
	store     highWaterStore
	opener    GiftWrapOpener
	routes    RoutePlanner
	// subscribe opens one batched subscription; tests replace it.
	subscribe func(ctx context.Context, directed []nostr.DirectedFilter) (chan nostr.RelayEvent, chan nostr.RelayClosed)
	debounce  time.Duration
}

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
// WithRoutes enables per-repository relay routing and live resubscription.
func WithRoutes(routes RoutePlanner) Option {
	return func(s *Service) { s.routes = routes }
}

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
	if s.subscribe == nil {
		s.subscribe = func(ctx context.Context, directed []nostr.DirectedFilter) (chan nostr.RelayEvent, chan nostr.RelayClosed) {
			return s.pool.BatchedSubscribeManyNotifyClosed(ctx, directed, nostr.SubscriptionOptions{Label: "drydock-listener"})
		}
	}
	if s.debounce <= 0 {
		s.debounce = routeChangeDebounce
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

	s.logger.Info("starting nostr listener", "relay_count", len(s.cfg.Relays), "routing", s.routes != nil)

	var lastSeen atomic.Int64
	backoff := time.Second
	var changes <-chan struct{}
	if s.routes != nil {
		changes = s.routes.Changes()
	}

	// Resubscriptions reuse the initial catch-up bound rather than a live
	// high-water mark: the mark is a global maximum, so one fast relay would
	// make us skip another relay's unfinished backlog, and a newly admitted
	// customer relay has never been read at all. Ingest handler completion
	// makes the resulting replays idempotent.
	var pending []nostr.DirectedFilter
	for {
		directed := pending
		var retry <-chan time.Time
		if directed == nil {
			routes, ok := s.currentRoutes(ctx)
			directed = s.directedFilters(nostr.Timestamp(since), routes)
			if !ok {
				retry = time.After(routeRetryInterval)
			}
		}
		pending = nil
		currentKey := directedKey(directed)
		subCtx, cancel := s.withCancel(ctx)
		stream, closedCh := s.subscribe(subCtx, directed)

		var debounce <-chan time.Time
		resubscribe := false
		streamEnded := false
		for !streamEnded {
			select {
			case <-ctx.Done():
				cancel()
				s.pool.Close("shutdown")
				return nil
			case <-changes:
				if debounce == nil {
					debounce = time.After(s.debounce)
				}
			case <-retry:
				retry = nil
				if debounce == nil {
					debounce = time.After(s.debounce)
				}
			case <-debounce:
				debounce = nil
				routes, ok := s.currentRoutes(ctx)
				if !ok && retry == nil {
					retry = time.After(routeRetryInterval)
				}
				next := s.directedFilters(nostr.Timestamp(since), routes)
				if directedKey(next) == currentKey {
					continue // nothing we subscribe to actually changed
				}
				pending = next
				resubscribe = true
				streamEnded = true
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
		cancel()

		if resubscribe {
			s.logger.Info("control plane changed; resubscribing", "filters", len(pending))
			continue
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

// directedKey canonicalizes a subscription set so unchanged routing does not
// cause a resubscription.
func directedKey(directed []nostr.DirectedFilter) string {
	parts := make([]string, 0, len(directed))
	for _, d := range directed {
		parts = append(parts, d.String())
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

func (s *Service) withCancel(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

// currentRoutes loads routing inputs. On error the planner still returns
// control-plane coverage with its last good relay plan; ok=false asks the
// caller to retry later.
func (s *Service) currentRoutes(ctx context.Context) (relayroute.Routes, bool) {
	if s.routes == nil {
		return relayroute.Routes{}, true
	}
	routes, err := s.routes.Routes(ctx)
	if err != nil {
		s.logger.Error("failed to load relay routes; keeping previous relay plan", "error", err)
		return routes, false
	}
	if len(routes.Plan.Rejected) > 0 || routes.Plan.Truncated {
		s.logger.Warn("relay routing policy rejected declared relays",
			"rejected", len(routes.Plan.Rejected),
			"truncated", routes.Plan.Truncated,
			"admitted", len(routes.Plan.Relays),
		)
	}
	return routes, true
}

// directedFilters builds the full subscription set: every base relay gets the
// general filters plus the control-plane filters, and each admitted customer
// relay gets filters scoped to the repositories that declared it.
func (s *Service) directedFilters(since nostr.Timestamp, routes relayroute.Routes) []nostr.DirectedFilter {
	base := append(subscriptionFilters(since, s.cfg), controlPlaneFilters(routes)...)
	directed := make([]nostr.DirectedFilter, 0, len(base)*len(s.cfg.Relays)+2*len(routes.Plan.Relays))
	for _, relay := range s.cfg.Relays {
		for _, filter := range base {
			directed = append(directed, nostr.DirectedFilter{Relay: relay, Filter: filter})
		}
	}
	return append(directed, routeDirectedFilters(since, routes)...)
}

// controlPlaneFilters are the no-Since filters added on base relays for state
// the registry already knows about: known authors' list revisions and
// deletions, and the announcements of every declared repository.
func controlPlaneFilters(routes relayroute.Routes) []nostr.Filter {
	var filters []nostr.Filter
	// The operator ceiling list and its deletion. Coming from live routes
	// rather than static config, an operator rotation resubscribes here.
	if operator, err := nostr.PubKeyFromHex(strings.TrimSpace(routes.Operator)); err == nil && operator != nostr.ZeroPK {
		filters = append(filters,
			nostr.Filter{
				Kinds:   []nostr.Kind{eventkind.MonitoredRepositories},
				Authors: []nostr.PubKey{operator},
				Tags:    nostr.TagMap{"d": {monitoring.ListIdentifier}},
			},
			nostr.Filter{
				Kinds:   []nostr.Kind{eventkind.Deletion},
				Authors: []nostr.PubKey{operator},
				Tags:    nostr.TagMap{"a": {monitoring.ListAddress(operator.Hex())}},
			},
		)
	}
	var authors []nostr.PubKey
	var addresses []string
	for _, raw := range routes.ListAuthors {
		author, err := nostr.PubKeyFromHex(strings.TrimSpace(raw))
		if err != nil || author == nostr.ZeroPK {
			continue
		}
		authors = append(authors, author)
		addresses = append(addresses, monitoring.ListAddress(author.Hex()))
	}
	if len(authors) > 0 {
		filters = append(filters,
			nostr.Filter{
				Kinds:   []nostr.Kind{eventkind.MonitoredRepositories},
				Authors: authors,
				Tags:    nostr.TagMap{"d": {monitoring.ListIdentifier}},
			},
			nostr.Filter{
				Kinds:   []nostr.Kind{eventkind.Deletion},
				Authors: authors,
				Tags:    nostr.TagMap{"a": addresses},
			},
		)
	}
	return append(filters, announcementFilters(routes.DeclaredRepositories, false)...)
}

// routeDirectedFilters scopes each admitted customer relay to the repositories
// that declared it: NIP-34 work items by repository address, and the
// repository's own announcement and state by owner and identifier.
func routeDirectedFilters(since nostr.Timestamp, routes relayroute.Routes) []nostr.DirectedFilter {
	var directed []nostr.DirectedFilter
	for _, relay := range routes.Plan.RelayURLs() {
		refs := routes.Plan.Relays[relay]
		addresses := make([]string, 0, len(refs))
		for _, ref := range refs {
			addresses = append(addresses, ref.Address)
		}
		directed = append(directed, nostr.DirectedFilter{Relay: relay, Filter: nostr.Filter{
			Kinds: []nostr.Kind{eventkind.Patch, eventkind.GitPullRequest, eventkind.GitPullRequestUpdate},
			Tags:  nostr.TagMap{"a": addresses},
			Since: since,
		}})
		for _, filter := range announcementFilters(refs, true) {
			directed = append(directed, nostr.DirectedFilter{Relay: relay, Filter: filter})
		}
	}
	return directed
}

// announcementFilters returns one no-Since filter per owner for the kind-30617
// announcements (and, when withState, kind-30618 state) of refs. One filter per
// owner keeps the scope exact: a combined owners × identifiers filter would
// also request repositories nobody declared.
func announcementFilters(refs []scope.RepositoryRef, withState bool) []nostr.Filter {
	byOwner := map[string][]string{}
	for _, ref := range refs {
		byOwner[ref.OwnerPubkey] = append(byOwner[ref.OwnerPubkey], ref.Identifier)
	}
	owners := make([]string, 0, len(byOwner))
	for owner := range byOwner {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	filters := make([]nostr.Filter, 0, len(owners))
	for _, owner := range owners {
		pk, err := nostr.PubKeyFromHex(owner)
		if err != nil {
			continue
		}
		identifiers := byOwner[owner]
		sort.Strings(identifiers)
		kinds := []nostr.Kind{eventkind.RepositoryAnnouncement}
		if withState {
			kinds = append(kinds, eventkind.RepositoryState)
		}
		filters = append(filters, nostr.Filter{Kinds: kinds, Authors: []nostr.PubKey{pk}, Tags: nostr.TagMap{"d": identifiers}})
	}
	return filters
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
