package depupgrade

import (
	"context"
	"strings"
	"sync"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
)

const (
	pendingScanLease         = 30 * time.Minute
	pendingScanRetryCooldown = 15 * time.Minute
	pendingScanDrainLimit    = 256
)

// durableScanStore is intentionally narrower than Store so trigger durability
// can live in this file without widening the service persistence interface.
// *db.Store implements it; lightweight service tests may continue using stores
// that exercise only the core upgrade lifecycle.
type durableScanStore interface {
	UpsertPendingDependencyUpgradeScan(context.Context, string, int64) (bool, error)
	ReservePendingDependencyUpgradeScan(context.Context, string, int64, int64) (db.PendingDependencyUpgradeScan, bool, error)
	CompletePendingDependencyUpgradeScan(context.Context, string, int64, int64) (bool, error)
	DeferPendingDependencyUpgradeScan(context.Context, string, int64, int64, int64, string) error
	ResetStuckPendingDependencyUpgradeScans(context.Context, int64) (int64, error)
	ListDuePendingDependencyUpgradeScans(context.Context, int64, int) ([]db.PendingDependencyUpgradeScan, error)
}

// OnDefaultBranchHeadChanged is the reactive trigger: ingest calls it when a
// monitored repository's default-branch head commit moves. It applies the
// fail-closed monitoring gate, durably coalesces one pending scan per repository,
// and offers a nonblocking wake-up hint to the worker queue. It never returns an
// error: the head is already persisted by the time this runs, so returning one
// would make ingest retry an event whose HeadChanged signal is now false.
func (s *Service) OnDefaultBranchHeadChanged(ctx context.Context, repoID, newHead string) error {
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return nil
	}
	if !s.deps.Monitoring.Contains(repositoryAddress(repoID)) {
		s.deps.Logger.Debug("ignoring head change for unmonitored repository", "repo_id", repoID)
		return nil
	}
	store, durable := s.deps.Store.(durableScanStore)
	if !durable {
		s.enqueue(repoID, TriggerDefaultBranch)
		return nil
	}
	shouldEnqueue, err := store.UpsertPendingDependencyUpgradeScan(ctx, repoID, time.Now().Unix())
	if err != nil {
		// Do not block relay ingest. Without a durable row the worker cannot
		// safely distinguish this hint from a duplicate reservation. A database
		// outage can still lose this signal; recovery then requires a later head
		// movement because the already-persisted head will not compare as new.
		s.deps.Logger.Error("failed to persist dependency-upgrade scan",
			"repo_id", repoID, "error", err)
		return nil
	}
	if shouldEnqueue {
		s.enqueue(repoID, TriggerDefaultBranch)
	} else {
		s.deps.Logger.Debug("coalesced dependency-upgrade scan", "repo_id", repoID)
	}
	return nil
}

// enqueue offers a scan to the worker pool without blocking. For reactive
// scans a full queue drops only this wake-up hint; the durable row remains for
// the scheduled sweep. Scheduled scans are naturally retried by the next sweep.
func (s *Service) enqueue(repoID string, trigger Trigger) bool {
	select {
	case s.queue <- scanJob{repoID: repoID, trigger: trigger}:
		s.deps.Logger.Info("queued dependency-upgrade scan", "repo_id", repoID, "trigger", string(trigger))
		return true
	default:
		s.deps.Logger.Warn("dependency-upgrade queue full; deferring scan", "repo_id", repoID, "trigger", string(trigger))
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
					s.runJob(ctx, job)
				}
			}
		}()
	}
	wg.Wait()
}

func (s *Service) runJob(ctx context.Context, job scanJob) {
	store, durable := s.deps.Store.(durableScanStore)
	var reservation db.PendingDependencyUpgradeScan
	if durable && job.trigger == TriggerDefaultBranch {
		now := time.Now()
		var ok bool
		var err error
		reservation, ok, err = store.ReservePendingDependencyUpgradeScan(
			ctx, job.repoID, now.Unix(), now.Add(pendingScanLease).Unix())
		if err != nil {
			s.deps.Logger.Warn("failed to reserve dependency-upgrade scan",
				"repo_id", job.repoID, "error", err)
			return
		}
		if !ok {
			return // duplicate or cooldown-delayed queue hint
		}
	}

	err := s.ScanRepository(ctx, job.repoID, job.trigger)
	if err != nil {
		s.deps.Logger.Warn("dependency-upgrade scan failed",
			"repo_id", job.repoID, "trigger", string(job.trigger), "error", err)
		if durable && job.trigger == TriggerDefaultBranch {
			now := time.Now()
			availableAt := now.Add(pendingScanRetryCooldown)
			if ctx.Err() != nil {
				// Routine shutdown is not a repository failure. Make the row
				// immediately eligible for recovery by the next process.
				availableAt = now
			}
			if deferErr := store.DeferPendingDependencyUpgradeScan(context.WithoutCancel(ctx),
				job.repoID, reservation.Generation, now.Unix(), availableAt.Unix(), err.Error()); deferErr != nil {
				s.deps.Logger.Warn("failed to defer dependency-upgrade scan",
					"repo_id", job.repoID, "error", deferErr)
			}
		}
		return
	}
	if durable && job.trigger == TriggerDefaultBranch {
		requeue, completeErr := store.CompletePendingDependencyUpgradeScan(
			context.WithoutCancel(ctx), job.repoID, reservation.Generation, time.Now().Unix())
		if completeErr != nil {
			s.deps.Logger.Warn("failed to complete dependency-upgrade scan",
				"repo_id", job.repoID, "error", completeErr)
			return
		}
		if requeue {
			s.enqueue(job.repoID, TriggerDefaultBranch)
		}
	}
}

// RunScheduled is one periodic sweep: it reconciles open upgrades against their
// published patches' NIP-34 root statuses (cheap, DB-only) and enqueues a scan
// for every monitored repository. The scan itself self-gates on the repository's
// enabled flag and schedule trigger after its config is loaded, so a repository
// that has not opted into the schedule is a no-op beyond the workspace prepare.
func (s *Service) RunScheduled(ctx context.Context) {
	if store, ok := s.deps.Store.(durableScanStore); ok {
		now := time.Now().Unix()
		if _, err := store.ResetStuckPendingDependencyUpgradeScans(ctx, now); err != nil {
			s.deps.Logger.Warn("failed to reset stuck dependency-upgrade scans", "error", err)
		}
		pending, err := store.ListDuePendingDependencyUpgradeScans(ctx, now, pendingScanDrainLimit)
		if err != nil {
			s.deps.Logger.Warn("failed to list pending dependency-upgrade scans", "error", err)
		} else {
			for _, scan := range pending {
				if ctx.Err() != nil {
					return
				}
				s.enqueue(scan.RepoID, TriggerDefaultBranch)
			}
		}
	}

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
