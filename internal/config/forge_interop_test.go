package config

import (
	"testing"
	"time"
)

func TestForgeInteropDefaultOffAndValidation(t *testing.T) {
	for _, name := range []string{"DRYDOCK_LOOM_ENABLED", "DRYDOCK_CONCORD_ENABLED", "DRYDOCK_LOOM_MAX_RUNS_PER_REPO", "DRYDOCK_LOOM_MAX_RUNS", "DRYDOCK_LOOM_RUN_TTL"} {
		t.Setenv(name, "")
	}
	c := FromEnv()
	if c.LoomEnabled || c.ConcordEnabled || c.LoomMaxRunsPerRepo != 64 || c.LoomMaxRuns != 1024 || c.LoomRunTTL != 24*time.Hour {
		t.Fatalf("defaults: %+v", c.ForgeInteropConfig)
	}
	for _, tc := range []struct{ name, value string }{
		{"DRYDOCK_LOOM_ENABLED", "maybe"}, {"DRYDOCK_CONCORD_ENABLED", "maybe"},
		{"DRYDOCK_LOOM_MAX_RUNS_PER_REPO", "-1"}, {"DRYDOCK_LOOM_MAX_RUNS", "1"},
		{"DRYDOCK_LOOM_MAX_RUNS", "bad"}, {"DRYDOCK_LOOM_RUN_TTL", "0s"}, {"DRYDOCK_LOOM_RUN_TTL", "bad"},
	} {
		t.Run(tc.name+tc.value, func(t *testing.T) {
			t.Setenv("DRYDOCK_LOOM_ENABLED", "true")
			t.Setenv(tc.name, tc.value)
			cfg := FromEnv()
			cfg.SignerNsec = "configured"
			var result ValidationResult
			cfg.validateForgeInterop(&result)
			if len(result.Errors) == 0 {
				t.Fatal("invalid interop config accepted")
			}
		})
	}
	t.Setenv("DRYDOCK_LOOM_ENABLED", "true")
	t.Setenv("DRYDOCK_CONCORD_ENABLED", "true")
	c = FromEnv()
	c.SignerBunkerURL = ""
	c.SignerNsec = ""
	var result ValidationResult
	c.validateForgeInterop(&result)
	if len(result.Errors) == 0 {
		t.Fatal("signer requirement not validated")
	}
	c.SignerNsec = "configured"
	result = ValidationResult{}
	c.validateForgeInterop(&result)
	if len(result.Errors) != 0 {
		t.Fatalf("valid interop config: %v", result.Errors)
	}
}
