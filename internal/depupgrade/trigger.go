package depupgrade

import (
	"context"
	"strings"
	"sync"

	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
)

// OnDefaultBranchHeadChanged is the reactive trigger: ingest calls it when a
// monitored repository's default-branch head commit moves. It applies the
// fail-closed monitoring gate and enqueues a scan. It never returns an error:
// the head is already persisted by the time this runs, so returning one would
// make ingest retry an event whose HeadChanged signal is now false (lost anyway)
// — instead a full queue is dropped-and-logged so a transient backlog cannot
// wedge ingestion. A dropped reactive scan is healed by the next head movement,
// or by the periodic sweep for repositories that also opt into triggers.schedule.
// Durable reactive-scan persistence is tracked as a follow-up (DRYDOCK-qdp8).
func (s *Service) OnDefaultBranchHeadChanged(ctx context.Context, repoID, newHead string) error {
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return nil
	}
	if !s.deps.Monitoring.Contains(repositoryAddress(repoID)) {
		s.deps.Logger.Debug("ignoring head change for unmonitored repository", "repo_id", repoID)
		return nil
	}
	s.enqueue(repoID, TriggerDefaultBranch)
	return nil
}

// enqueue offers a scan to the worker pool without blocking. A full queue is
// logged (with the repo id) and dropped rather than blocking the caller.
func (s *Service) enqueue(repoID string, trigger Trigger) bool {
	select {
	case s.queue <- scanJob{repoID: repoID, trigger: trigger}:
		s.deps.Logger.Info("queued dependency-upgrade scan", "repo_id", repoID, "trigger", string(trigger))
		return true
	default:
		s.deps.Logger.Warn("dependency-upgrade queue full; dropping scan", "repo_id", repoID, "trigger", string(trigger))
		return false
	}
}

// Run drains the scan queue with the configured number of workers until the
// context is cancelled. The composition root runs it in a background goroutine.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < s.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-s.queue:
					if err := s.ScanRepository(ctx, job.repoID, job.trigger); err != nil {
						s.deps.Logger.Warn("dependency-upgrade scan failed",
							"repo_id", job.repoID, "trigger", string(job.trigger), "error", err)
					}
				}
			}
		}()
	}
	wg.Wait()
}

// RunScheduled is one periodic sweep: it reconciles open upgrades against their
// published patches' NIP-34 root statuses (cheap, DB-only) and enqueues a scan
// for every monitored repository. The scan itself self-gates on the repository's
// enabled flag and schedule trigger after its config is loaded, so a repository
// that has not opted into the schedule is a no-op beyond the workspace prepare.
func (s *Service) RunScheduled(ctx context.Context) {
	repoIDs := s.deps.Repositories.MonitoredRepositoryIDs()
	for _, repoID := range repoIDs {
		if ctx.Err() != nil {
			return
		}
		s.reconcile(ctx, repoID)
		s.enqueue(repoID, TriggerSchedule)
	}
}

// reconcile marks open upgrades merged or rejected when their published patch's
// root status has become applied (1631) or closed (1632). This is what stops the
// service re-proposing work a maintainer already actioned, without a second
// ingest notification path. Errors are logged, never fatal to the sweep.
func (s *Service) reconcile(ctx context.Context, repoID string) {
	open, err := s.deps.Store.GetOpenDependencyUpgrades(ctx, repoID)
	if err != nil {
		s.deps.Logger.Warn("reconcile: failed to load open upgrades", "repo_id", repoID, "error", err)
		return
	}
	for _, up := range open {
		if strings.TrimSpace(up.PatchEventID) == "" {
			continue // never published; nothing to reconcile
		}
		kind, _, _, ok, err := s.deps.Store.GetRootStatus(ctx, up.PatchEventID, repoID)
		if err != nil {
			s.deps.Logger.Warn("reconcile: failed to read root status",
				"repo_id", repoID, "upgrade_id", up.ID, "error", err)
			continue
		}
		if !ok {
			continue
		}
		var next db.DependencyUpgradeStatus
		switch kind {
		case int(eventkind.StatusApplied):
			next = db.DependencyUpgradeMerged
		case int(eventkind.StatusClosed):
			next = db.DependencyUpgradeRejected
		default:
			continue
		}
		if err := s.deps.Store.MarkDependencyUpgradeStatus(ctx, up.ID, next, ""); err != nil {
			s.deps.Logger.Warn("reconcile: failed to mark upgrade status",
				"repo_id", repoID, "upgrade_id", up.ID, "status", string(next), "error", err)
			continue
		}
		s.deps.Logger.Info("reconciled dependency upgrade",
			"repo_id", repoID, "upgrade_id", up.ID, "status", string(next))
	}
}
