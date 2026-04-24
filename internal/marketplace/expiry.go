package marketplace

import (
	"context"
	"log/slog"
	"time"

	"drydock/internal/db"
	"drydock/internal/metrics"
)

// ExpiryConfig configures the assignment expiry service.
type ExpiryConfig struct {
	CheckInterval time.Duration // How often to check for expired assignments
	BatchSize     int           // Maximum assignments to process per tick
}

// DefaultExpiryConfig returns sensible defaults.
func DefaultExpiryConfig() ExpiryConfig {
	return ExpiryConfig{
		CheckInterval: 5 * time.Minute,
		BatchSize:     100,
	}
}

// ExpiryService handles background expiration of stale assignments.
type ExpiryService struct {
	cfg      ExpiryConfig
	store    *db.Store
	registry *Registry // Optional: for reassignment
	logger   *slog.Logger
}

// NewExpiryService creates an assignment expiry service.
func NewExpiryService(cfg ExpiryConfig, store *db.Store, logger *slog.Logger) *ExpiryService {
	if cfg.CheckInterval == 0 {
		cfg.CheckInterval = 5 * time.Minute
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ExpiryService{
		cfg:    cfg,
		store:  store,
		logger: logger,
	}
}

// WithRegistry enables automatic reassignment when assignments expire.
func (s *ExpiryService) WithRegistry(registry *Registry) *ExpiryService {
	s.registry = registry
	return s
}

// Run starts the background expiry loop. Blocks until context is cancelled.
func (s *ExpiryService) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.CheckInterval)
	defer ticker.Stop()

	s.logger.Info("assignment expiry service started",
		"check_interval", s.cfg.CheckInterval,
		"batch_size", s.cfg.BatchSize,
	)

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("assignment expiry service stopped")
			return
		case <-ticker.C:
			if err := s.expireStaleAssignments(ctx); err != nil {
				s.logger.Error("assignment expiry check failed", "error", err)
			}
		}
	}
}

// expireStaleAssignments marks expired assignments and updates metrics.
func (s *ExpiryService) expireStaleAssignments(ctx context.Context) error {
	expired, err := s.store.ExpireStaleAssignments(ctx)
	if err != nil {
		return err
	}

	if expired > 0 {
		metrics.MarketplaceAssignmentsExpired.Add(expired)
		s.logger.Info("expired stale assignments",
			"count", expired,
		)

		// TODO: When reassignment is implemented (drydock-j14), 
		// query the expired assignments and attempt to reassign them
		// to other available reviewers using the registry.
	}

	return nil
}

// ExpireNow runs an immediate expiry check (useful for testing).
func (s *ExpiryService) ExpireNow(ctx context.Context) (int64, error) {
	expired, err := s.store.ExpireStaleAssignments(ctx)
	if err != nil {
		return 0, err
	}
	if expired > 0 {
		metrics.MarketplaceAssignmentsExpired.Add(expired)
	}
	return expired, nil
}
