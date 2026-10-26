package config

import (
	"context"
	"strings"
	"testing"
)

var maxTokensEnvKeys = []string{
	"DRYDOCK_LLM_MAX_TOKENS",
	"DRYDOCK_PLANNER_MAX_TOKENS",
	"DRYDOCK_CODER32B_MAX_TOKENS",
	"DRYDOCK_LLM70B_MAX_TOKENS",
	"DRYDOCK_CODER14B_MAX_TOKENS",
	"DRYDOCK_SEC70B_MAX_TOKENS",
	"DRYDOCK_SECCLASSIFY_MAX_TOKENS",
	"DRYDOCK_SECLOCALIZE_MAX_TOKENS",
	"DRYDOCK_META_MAX_TOKENS",
	"DRYDOCK_AGENTIC_DISCOVERY_MAX_TOKENS",
}

func clearMaxTokensEnv(t *testing.T) {
	t.Helper()
	for _, key := range maxTokensEnvKeys {
		t.Setenv(key, "")
	}
}

func roleMaxTokens(cfg Config) map[string]int {
	return map[string]int{
		"planner": cfg.PlannerMaxTokens, "coder32b": cfg.Coder32BMaxTokens, "llm70b": cfg.LLM70BMaxTokens,
		"coder14b": cfg.Coder14BMaxTokens, "sec70b": cfg.Sec70BMaxTokens, "secclassify": cfg.SecClassifyMaxTokens,
		"seclocalize": cfg.SecLocalizeMaxTokens, "meta": cfg.MetaMaxTokens, "agentic-discovery": cfg.AgenticDiscoveryMaxTokens,
	}
}

func TestFromEnvMaxTokensDefaultsToNoCap(t *testing.T) {
	clearMaxTokensEnv(t)
	cfg := FromEnv()
	if cfg.LLMMaxTokens != 0 {
		t.Fatalf("LLMMaxTokens = %d, want 0 (no cap)", cfg.LLMMaxTokens)
	}
	for role, got := range roleMaxTokens(cfg) {
		if got != 0 {
			t.Fatalf("%s max tokens = %d, want 0", role, got)
		}
	}
}

func TestFromEnvMaxTokensGlobalAndOverrides(t *testing.T) {
	clearMaxTokensEnv(t)
	t.Setenv("DRYDOCK_LLM_MAX_TOKENS", "8192")
	t.Setenv("DRYDOCK_PLANNER_MAX_TOKENS", "2048")
	t.Setenv("DRYDOCK_CODER32B_MAX_TOKENS", "24576")
	t.Setenv("DRYDOCK_META_MAX_TOKENS", "0")

	cfg := FromEnv()
	want := map[string]int{
		"planner": 2048, "coder32b": 24576, "llm70b": 8192, "coder14b": 8192,
		"sec70b": 8192, "secclassify": 8192, "seclocalize": 8192,
		"meta": 0, // explicit 0 disables the cap for one role
		// Discovery inherits coder32b, matching its base URL/model fallback.
		"agentic-discovery": 24576,
	}
	if cfg.LLMMaxTokens != 8192 {
		t.Fatalf("LLMMaxTokens = %d, want 8192", cfg.LLMMaxTokens)
	}
	for role, got := range roleMaxTokens(cfg) {
		if got != want[role] {
			t.Fatalf("%s max tokens = %d, want %d", role, got, want[role])
		}
	}

	t.Setenv("DRYDOCK_AGENTIC_DISCOVERY_MAX_TOKENS", "4096")
	if got := FromEnv().AgenticDiscoveryMaxTokens; got != 4096 {
		t.Fatalf("AgenticDiscoveryMaxTokens = %d, want explicit 4096", got)
	}
}

func maxTokensErrors(result ValidationResult) []string {
	var out []string
	for _, msg := range result.Errors {
		if strings.Contains(msg, "_MAX_TOKENS") {
			out = append(out, msg)
		}
	}
	return out
}

func TestValidateRejectsInvalidMaxTokens(t *testing.T) {
	for _, tc := range []struct {
		key, value string
	}{
		{"DRYDOCK_LLM_MAX_TOKENS", "-1"},
		{"DRYDOCK_LLM_MAX_TOKENS", "lots"},
		{"DRYDOCK_PLANNER_MAX_TOKENS", "-5"},
		{"DRYDOCK_CODER32B_MAX_TOKENS", "4k"},
		{"DRYDOCK_SECLOCALIZE_MAX_TOKENS", "1.5"},
		{"DRYDOCK_AGENTIC_DISCOVERY_MAX_TOKENS", "-2"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearMaxTokensEnv(t)
			t.Setenv(tc.key, tc.value)
			cfg := FromEnv()
			errs := maxTokensErrors(cfg.Validate(context.Background()))
			// One bad variable yields exactly one error naming it, even
			// though other roles inherit from it.
			if len(errs) != 1 || !strings.Contains(errs[0], tc.key) {
				t.Fatalf("max-tokens errors = %q, want exactly one naming %s", errs, tc.key)
			}
		})
	}
}

func TestValidateAcceptsValidMaxTokens(t *testing.T) {
	clearMaxTokensEnv(t)
	t.Setenv("DRYDOCK_LLM_MAX_TOKENS", "32768")
	t.Setenv("DRYDOCK_PLANNER_MAX_TOKENS", "0")
	cfg := FromEnv()
	if errs := maxTokensErrors(cfg.Validate(context.Background())); len(errs) != 0 {
		t.Fatalf("unexpected max-tokens errors: %q", errs)
	}
}
