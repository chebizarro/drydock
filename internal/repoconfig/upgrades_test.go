package repoconfig

import (
	"reflect"
	"strings"
	"testing"
)

func TestUpgradesAllowScriptsRejected(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		t.Run(value, func(t *testing.T) {
			cfg, err := Parse([]byte("version: 1\nupgrades:\n  allow_scripts: " + value + "\n"))
			if err == nil || !strings.Contains(err.Error(), "upgrades.allow_scripts is operator-only") {
				t.Fatalf("Parse() error = %v, want operator-only rejection", err)
			}
			// Fail closed: an invalid/rejected config returns defaults.
			if !reflect.DeepEqual(cfg, Default()) {
				t.Fatalf("rejected upgrades config did not fail closed to defaults")
			}
			if cfg.Upgrades.AllowScripts != nil {
				t.Fatalf("rejected allow_scripts leaked into config: %#v", cfg.Upgrades)
			}
		})
	}
}

func TestUpgradesAbsentIsAccepted(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Upgrades.AllowScripts != nil {
		t.Fatalf("default Upgrades.AllowScripts = %#v, want nil", cfg.Upgrades.AllowScripts)
	}
	// Absent block still defaults to a usable (but disabled) policy.
	if cfg.Upgrades.Enabled {
		t.Fatalf("absent upgrades block should default disabled")
	}
	if cfg.Upgrades.Policy != "next_patch" {
		t.Fatalf("default policy = %q, want next_patch", cfg.Upgrades.Policy)
	}
	if cfg.Upgrades.MaxUpgradesPerRun != 5 {
		t.Fatalf("default max_upgrades_per_run = %d, want 5", cfg.Upgrades.MaxUpgradesPerRun)
	}
}

func TestUpgradesPolicyBlockParsesAndValidates(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nupgrades:\n  enabled: true\n  policy: latest\n  ecosystems: [go, NPM, go]\n  ignore: [\" x \", x, \"\"]\n  allow: [y]\n  max_upgrades_per_run: 2\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !cfg.Upgrades.Enabled || cfg.Upgrades.Policy != "latest" {
		t.Fatalf("unexpected upgrades config: %#v", cfg.Upgrades)
	}
	if got := cfg.Upgrades.Ecosystems; len(got) != 2 || got[0] != "go" || got[1] != "npm" {
		t.Fatalf("ecosystems normalization = %#v, want [go npm]", got)
	}
	if got := cfg.Upgrades.Ignore; len(got) != 1 || got[0] != "x" {
		t.Fatalf("ignore normalization = %#v, want [x]", got)
	}
	if cfg.Upgrades.MaxUpgradesPerRun != 2 {
		t.Fatalf("max_upgrades_per_run = %d, want 2", cfg.Upgrades.MaxUpgradesPerRun)
	}
}

func TestUpgradesRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{"policy", "version: 1\nupgrades:\n  policy: newest\n", "invalid upgrades.policy"},
		{"ecosystem", "version: 1\nupgrades:\n  ecosystems: [ruby]\n", "invalid upgrades.ecosystem"},
		{"max_negative", "version: 1\nupgrades:\n  max_upgrades_per_run: -1\n", "max_upgrades_per_run must be >= 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tc.want)
			}
		})
	}
}
