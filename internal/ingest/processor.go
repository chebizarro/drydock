package ingest

import (
	"context"
	"errors"
	"log/slog"

	"drydock/internal/db"

	"fiatjaf.com/nostr"
)

type Processor struct {
	store  *db.Store
	logger *slog.Logger
}

func NewProcessor(store *db.Store, logger *slog.Logger) *Processor {
	return &Processor{store: store, logger: logger}
}

func (p *Processor) ProcessEvent(ctx context.Context, event nostr.Event, relayURL string) error {
	inserted, err := p.store.InsertIngestedEvent(ctx, event)
	if err != nil {
		return err
	}
	if !inserted {
		p.logger.Debug("skipping duplicate event", "event_id", event.ID.Hex(), "kind", int(event.Kind))
		return nil
	}

	switch event.Kind {
	case nostr.KindRepositoryAnnouncement:
		return p.store.UpsertRepositoryAnnouncement(ctx, event)
	case nostr.KindRepositoryState:
		return p.store.UpsertRepositorySnapshot(ctx, event)
	case 1617, 1618, 1619:
		if err := p.store.InsertPatchEvent(ctx, event); err != nil {
			return err
		}
		if err := p.store.RecordPatchEventRelay(ctx, event.ID.Hex(), relayURL); err != nil {
			return err
		}
		repoID := db.RepoIDFromPatch(event)
		if repoID == "" {
			p.logger.Warn("patch event missing resolvable repository pointer", "event_id", event.ID.Hex(), "kind", int(event.Kind))
			return nil
		}
		stale, reason, err := p.store.IsPatchStaleBySnapshot(ctx, event)
		if err != nil {
			return err
		}
		if stale {
			p.logger.Info("skipping stale patch from snapshot", "event_id", event.ID.Hex(), "repo_id", repoID, "reason", reason)
			return nil
		}
		acquired, err := p.store.BeginReview(ctx, event.ID.Hex(), repoID)
		if err != nil {
			if errors.Is(err, db.ErrReviewAlreadyPublished) {
				p.logger.Debug("skipping already-published review target", "event_id", event.ID.Hex(), "repo_id", repoID)
				return nil
			}
			return err
		}
		if acquired {
			p.logger.Info("queued patch review", "event_id", event.ID.Hex(), "repo_id", repoID, "kind", int(event.Kind))
		}
		return nil
	default:
		return nil
	}
}
