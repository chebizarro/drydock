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

func TestFromEnvBetterleaksValidation(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "default off", want: false},
		{name: "explicit on", value: "true", want: true},
		{name: "invalid value fails closed", value: "not-a-bool", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DRYDOCK_BETTERLEAKS_VALIDATION", tt.value)

			if got := FromEnv().BetterleaksValidation; got != tt.want {
				t.Errorf("BetterleaksValidation = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFromEnvSecurityNostr(t *testing.T) {
	tests := []struct {
		name        string
		enabled     string
		targets     string
		active      string
		wantEnabled string
		wantTargets []string
		wantActive  bool
	}{
		{name: "defaults", wantEnabled: "auto"},
		{name: "configured", enabled: "true", targets: "wss://one.example, wss://two.example", active: "true", wantEnabled: "true", wantTargets: []string{"wss://one.example", "wss://two.example"}, wantActive: true},
		{name: "explicitly disabled", enabled: "false", wantEnabled: "false"},
		{name: "invalid values fail closed", enabled: "sometimes", active: "sometimes", wantEnabled: "false", wantActive: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DRYDOCK_SECURITY_NOSTR_ENABLED", tt.enabled)
			t.Setenv("DRYDOCK_SECURITY_NOSTR_PROBE_TARGETS", tt.targets)
			t.Setenv("DRYDOCK_SECURITY_NOSTR_PROBE_ACTIVE", tt.active)

			cfg := FromEnv()
			if cfg.SecurityNostrEnabled != tt.wantEnabled {
				t.Errorf("SecurityNostrEnabled = %q, want %q", cfg.SecurityNostrEnabled, tt.wantEnabled)
			}
			if len(cfg.SecurityNostrProbeTargets) != len(tt.wantTargets) {
				t.Fatalf("SecurityNostrProbeTargets = %#v, want %#v", cfg.SecurityNostrProbeTargets, tt.wantTargets)
			}
			for i := range tt.wantTargets {
				if cfg.SecurityNostrProbeTargets[i] != tt.wantTargets[i] {
					t.Fatalf("SecurityNostrProbeTargets = %#v, want %#v", cfg.SecurityNostrProbeTargets, tt.wantTargets)
				}
			}
			if cfg.SecurityNostrProbeActive != tt.wantActive {
				t.Errorf("SecurityNostrProbeActive = %v, want %v", cfg.SecurityNostrProbeActive, tt.wantActive)
			}
		})
	}
}
