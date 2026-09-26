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
}
