package listener

import (
	"context"
	"log/slog"
	"time"

	"fiatjaf.com/nostr"
)

type EventProcessor interface {
	ProcessEvent(ctx context.Context, event nostr.Event, relayURL string) error
}

var subscribedKinds = []nostr.Kind{
	30617, 30618,
	1617, 1618, 1619,
	1621, 1622,
	1630, 1631, 1632, 1633,
	1985,
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
}

func New(cfg Config, processor EventProcessor, logger *slog.Logger) *Service {
	return &Service{
		cfg:       cfg,
		processor: processor,
		logger:    logger,
		pool:      nostr.NewPool(nostr.PoolOptions{}),
	}
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

	filter := nostr.Filter{
		Kinds: append([]nostr.Kind(nil), subscribedKinds...),
		Since: nostr.Timestamp(time.Now().Add(-time.Duration(lookback) * time.Minute).Unix()),
	}

	s.logger.Info("starting nostr listener", "relay_count", len(s.cfg.Relays))
	stream := s.pool.SubscribeMany(ctx, s.cfg.Relays, filter, nostr.SubscriptionOptions{
		Label: "drydock-listener",
	})

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
		}
	}
}
