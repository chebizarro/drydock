package config

import "testing"

func TestFromEnvSecurity(t *testing.T) {
	tests := []struct {
		name        string
		enabled     string
		workers     string
		wantEnabled bool
		wantWorkers int
	}{
		{
			name:        "defaults",
			wantEnabled: false,
			wantWorkers: 2,
		},
		{
			name:        "configured",
			enabled:     "true",
			workers:     "7",
			wantEnabled: true,
			wantWorkers: 7,
		},
		{
			name:        "invalid values use defaults",
			enabled:     "not-a-bool",
			workers:     "not-an-int",
			wantEnabled: false,
			wantWorkers: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DRYDOCK_SECURITY_ENABLED", tt.enabled)
			t.Setenv("DRYDOCK_SECURITY_AUDIT_WORKERS", tt.workers)

			cfg := FromEnv()
			if cfg.SecurityEnabled != tt.wantEnabled {
				t.Errorf("SecurityEnabled = %v, want %v", cfg.SecurityEnabled, tt.wantEnabled)
			}
			if cfg.SecurityAuditWorkers != tt.wantWorkers {
				t.Errorf("SecurityAuditWorkers = %d, want %d", cfg.SecurityAuditWorkers, tt.wantWorkers)
			}
		})
	}
}
