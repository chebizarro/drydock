package enrollment

import (
	"git.sharegap.net/cascadia/drydock/internal/monitoring"
)

func monitoringRegistryFromBoot(f *fixture, boot OperatorBootstrap) (*monitoring.Registry, error) {
	registry, err := monitoring.NewRegistry(f.store, monitoring.Config{
		Drydock: f.servicePK.Hex(), Operator: boot.Operator, PreviousOperator: boot.PreviousOperator, HasPreviousOperator: boot.HasPrevious,
	})
	if err != nil {
		return nil, err
	}
	if err := registry.Load(f.ctx); err != nil {
		return nil, err
	}
	return registry, nil
}
