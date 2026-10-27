package relayroute

import (
	"context"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/loom"
)

// WithLoom merges claim-lineage and membership changes before the listener's
// existing equality/debounce/resubscribe path. Call once, before using the source.
func (m *MonitoringSource) WithLoom(ctx context.Context, service *loom.Service) *MonitoringSource {
	if service == nil {
		return m
	}
	m.loom = service
	m.changes = make(chan struct{}, 1)
	go func() {
		for {
			var timer *time.Timer
			var expires <-chan time.Time
			if next, err := service.NextExpiry(ctx); err == nil && !next.IsZero() {
				timer = time.NewTimer(time.Until(next))
				expires = timer.C
			}
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case <-m.registry.Changes():
			case <-service.Changes():
			case <-expires:
			}
			if timer != nil {
				timer.Stop()
			}
			select {
			case m.changes <- struct{}{}:
			default:
			}
		}
	}()
	return m
}
