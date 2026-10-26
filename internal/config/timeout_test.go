package config

import (
	"context"
	"strings"
	"testing"
	"time"
)

var timeoutEnvKeys = []string{
	"DRYDOCK_LLM_TIMEOUT",
	"DRYDOCK_PLANNER_TIMEOUT",
	"DRYDOCK_CODER32B_TIMEOUT",
	"DRYDOCK_LLM70B_TIMEOUT",
	"DRYDOCK_CODER14B_TIMEOUT",
	"DRYDOCK_SEC70B_TIMEOUT",
	"DRYDOCK_SECCLASSIFY_TIMEOUT",
	"DRYDOCK_SECLOCALIZE_TIMEOUT",
	"DRYDOCK_META_TIMEOUT",
	"DRYDOCK_AGENTIC_DISCOVERY_TIMEOUT",
}

func clearTimeoutEnv(t *testing.T) {
	t.Helper()
	for _, key := range timeoutEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv("DRYDOCK_IDE_AGENTIC_TIMEOUT", "")
}

func roleTimeouts(cfg Config) map[string]time.Duration {
	return map[string]time.Duration{
		"planner": cfg.PlannerTimeout, "coder32b": cfg.Coder32BTimeout, "llm70b": cfg.LLM70BTimeout,
		"coder14b": cfg.Coder14BTimeout, "sec70b": cfg.Sec70BTimeout, "secclassify": cfg.SecClassifyTimeout,
		"seclocalize": cfg.SecLocalizeTimeout, "meta": cfg.MetaTimeout, "agentic-discovery": cfg.AgenticDiscoveryTimeout,
	}
}

func TestFromEnvLLMTimeoutDefaultsTo180s(t *testing.T) {
	clearTimeoutEnv(t)
	cfg := FromEnv()
	if cfg.LLMTimeout != 180*time.Second {
		t.Fatalf("LLMTimeout = %s, want 3m0s", cfg.LLMTimeout)
	}
	for role, got := range roleTimeouts(cfg) {
		if got != 180*time.Second {
			t.Fatalf("%s timeout = %s, want 3m0s", role, got)
		}
	}
}

func TestFromEnvLLMTimeoutGlobalAndOverrides(t *testing.T) {
	clearTimeoutEnv(t)
	t.Setenv("DRYDOCK_LLM_TIMEOUT", "4m")
	t.Setenv("DRYDOCK_PLANNER_TIMEOUT", "90s")
	t.Setenv("DRYDOCK_CODER32B_TIMEOUT", "8m")
	t.Setenv("DRYDOCK_META_TIMEOUT", "1m30s")

	cfg := FromEnv()
	want := map[string]time.Duration{
		"planner": 90 * time.Second, "coder32b": 8 * time.Minute, "llm70b": 4 * time.Minute,
		"coder14b": 4 * time.Minute, "sec70b": 4 * time.Minute, "secclassify": 4 * time.Minute,
		"seclocalize": 4 * time.Minute, "meta": 90 * time.Second,
		// Discovery inherits coder32b, matching its base URL/model fallback.
		"agentic-discovery": 8 * time.Minute,
	}
	if cfg.LLMTimeout != 4*time.Minute {
		t.Fatalf("LLMTimeout = %s, want 4m0s", cfg.LLMTimeout)
	}
	for role, got := range roleTimeouts(cfg) {
		if got != want[role] {
			t.Fatalf("%s timeout = %s, want %s", role, got, want[role])
		}
	}

	t.Setenv("DRYDOCK_AGENTIC_DISCOVERY_TIMEOUT", "2m")
	if got := FromEnv().AgenticDiscoveryTimeout; got != 2*time.Minute {
		t.Fatalf("AgenticDiscoveryTimeout = %s, want explicit 2m0s", got)
	}
}

func timeoutErrors(result ValidationResult) []string {
	var out []string
	for _, msg := range result.Errors {
		if strings.Contains(msg, "_TIMEOUT") && strings.Contains(msg, "Go duration") {
			out = append(out, msg)
		}
	}
	return out
}

func TestValidateRejectsInvalidLLMTimeout(t *testing.T) {
	for _, tc := range []struct {
		key, value string
	}{
		{"DRYDOCK_LLM_TIMEOUT", "0s"},
		{"DRYDOCK_LLM_TIMEOUT", "-5s"},
		{"DRYDOCK_LLM_TIMEOUT", "180"}, // a bare number has no unit
		{"DRYDOCK_LLM_TIMEOUT", "lots"},
		{"DRYDOCK_LLM_TIMEOUT", "31m"}, // above the 30m ceiling
		{"DRYDOCK_PLANNER_TIMEOUT", "0"},
		{"DRYDOCK_CODER32B_TIMEOUT", "1h"},
		{"DRYDOCK_SECLOCALIZE_TIMEOUT", "3 minutes"},
		{"DRYDOCK_AGENTIC_DISCOVERY_TIMEOUT", "-1m"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearTimeoutEnv(t)
			t.Setenv(tc.key, tc.value)
			cfg := FromEnv()
			errs := timeoutErrors(cfg.Validate(context.Background()))
			// One bad variable yields exactly one error naming it, even
			// though other roles inherit from it.
			if len(errs) != 1 || !strings.Contains(errs[0], tc.key) {
				t.Fatalf("timeout errors = %q, want exactly one naming %s", errs, tc.key)
			}
		})
	}
}

func TestValidateAcceptsValidLLMTimeout(t *testing.T) {
	clearTimeoutEnv(t)
	t.Setenv("DRYDOCK_LLM_TIMEOUT", "30m") // the ceiling itself is allowed
	t.Setenv("DRYDOCK_IDE_AGENTIC_TIMEOUT", "1h")
	t.Setenv("DRYDOCK_PLANNER_TIMEOUT", "500ms")
	cfg := FromEnv()
	if errs := timeoutErrors(cfg.Validate(context.Background())); len(errs) != 0 {
		t.Fatalf("unexpected timeout errors: %q", errs)
	}
}

func ideTimeoutWarnings(result ValidationResult) []string {
	var out []string
	for _, msg := range result.Warnings {
		if strings.Contains(msg, "DRYDOCK_IDE_AGENTIC_TIMEOUT") {
			out = append(out, msg)
		}
	}
	return out
}

func TestValidateWarnsWhenLLMTimeoutReachesIDEAgenticTimeout(t *testing.T) {
	clearTimeoutEnv(t)
	cfg := FromEnv()
	if warns := ideTimeoutWarnings(cfg.Validate(context.Background())); len(warns) != 0 {
		t.Fatalf("defaults (180s vs 10m) warned: %q", warns)
	}

	t.Setenv("DRYDOCK_IDE_AGENTIC_TIMEOUT", "5m")
	t.Setenv("DRYDOCK_CODER32B_TIMEOUT", "5m")
	cfg = FromEnv()
	warns := ideTimeoutWarnings(cfg.Validate(context.Background()))
	// coder32b and agentic discovery (which inherits it) both reach 5m.
	if len(warns) != 2 || !strings.Contains(warns[0], "DRYDOCK_CODER32B_TIMEOUT") ||
		!strings.Contains(warns[1], "DRYDOCK_AGENTIC_DISCOVERY_TIMEOUT") {
		t.Fatalf("warnings = %q, want coder32b and agentic-discovery", warns)
	}
}
