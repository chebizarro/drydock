package ingest

import (
	"context"
	"errors"
	"log/slog"

	"drydock/internal/db"

	"fiatjaf.com/nostr"
)

type Processor struct {
	store       *db.Store
	logger      *slog.Logger
	ReviewQueue chan db.ReviewTask
}

func NewProcessor(store *db.Store, logger *slog.Logger) *Processor {
	return &Processor{
		store:       store,
		logger:      logger,
		ReviewQueue: make(chan db.ReviewTask, 256),
	}
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
	case 1630, 1631, 1632, 1633:
		return p.store.UpsertRootStatus(ctx, event)
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
		closed, closedReason, err := p.store.IsRootClosedByStatus(ctx, db.RootEventID(event), repoID)
		if err != nil {
			return err
		}
		if closed {
			p.logger.Info("skipping review for closed/applied root", "event_id", event.ID.Hex(), "repo_id", repoID, "reason", closedReason)
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
			task := db.ReviewTask{PatchEventID: event.ID.Hex(), RepoID: repoID}
			select {
			case p.ReviewQueue <- task:
				p.logger.Info("queued patch review", "event_id", event.ID.Hex(), "repo_id", repoID, "kind", int(event.Kind))
			default:
				// Queue is full — mark task back to failed so it can be retried
				// by the next startup's ResetStuckReviews or a future re-enqueue sweep.
				p.logger.Warn("review queue full, marking task for retry", "event_id", event.ID.Hex(), "repo_id", repoID)
				_ = p.store.MarkReviewFailed(ctx, event.ID.Hex(), repoID, "review queue full")
			}
		}
		return nil
	default:
		return nil
	}
}
