package config

import (
	"context"
	"testing"
	"time"
)

func TestValidate_NoRelays(t *testing.T) {
	cfg := Config{
		Relays:          []string{},
		ReadRelays:      []string{},
		PipelineWorkers: 2,
		DatabaseURL:     ":memory:",
	}

	result := cfg.Validate(context.Background())

	if !result.HasErrors() {
		t.Error("expected errors for missing relays")
	}

	foundRelayError := false
	for _, err := range result.Errors {
		if contains(err, "no relays configured") {
			foundRelayError = true
			break
		}
	}
	if !foundRelayError {
		t.Error("expected 'no relays configured' error")
	}
}

func TestValidate_InvalidRelayURL(t *testing.T) {
	cfg := Config{
		Relays:          []string{"http://invalid-scheme.com"},
		PipelineWorkers: 2,
		DatabaseURL:     ":memory:",
	}

	result := cfg.Validate(context.Background())

	foundSchemeError := false
	for _, err := range result.Errors {
		if contains(err, "ws://") || contains(err, "wss://") {
			foundSchemeError = true
			break
		}
	}
	if !foundSchemeError {
		t.Error("expected relay scheme error")
	}
}

func TestValidate_ValidRelays(t *testing.T) {
	cfg := Config{
		Relays:          []string{"wss://relay.damus.io", "ws://localhost:7777"},
		PipelineWorkers: 2,
		DatabaseURL:     ":memory:",
	}

	result := cfg.Validate(context.Background())

	// Should have no relay-related errors
	for _, err := range result.Errors {
		if contains(err, "relay") {
			t.Errorf("unexpected relay error: %s", err)
		}
	}
}

func TestValidate_NoSigner_Warning(t *testing.T) {
	cfg := Config{
		Relays:          []string{"wss://relay.damus.io"},
		PipelineWorkers: 2,
		DatabaseURL:     ":memory:",
		// No signer configured
	}

	result := cfg.Validate(context.Background())

	foundSignerWarning := false
	for _, warn := range result.Warnings {
		if contains(warn, "no signer configured") {
			foundSignerWarning = true
			break
		}
	}
	if !foundSignerWarning {
		t.Error("expected 'no signer configured' warning")
	}
}

func TestValidate_InvalidPipelineWorkers(t *testing.T) {
	cfg := Config{
		Relays:          []string{"wss://relay.damus.io"},
		PipelineWorkers: 0,
		DatabaseURL:     ":memory:",
	}

	result := cfg.Validate(context.Background())

	foundWorkerError := false
	for _, err := range result.Errors {
		if contains(err, "DRYDOCK_PIPELINE_WORKERS") {
			foundWorkerError = true
			break
		}
	}
	if !foundWorkerError {
		t.Error("expected pipeline workers error")
	}
}

func TestValidate_HighPipelineWorkers_Warning(t *testing.T) {
	cfg := Config{
		Relays:          []string{"wss://relay.damus.io"},
		PipelineWorkers: 64,
		DatabaseURL:     ":memory:",
	}

	result := cfg.Validate(context.Background())

	foundWorkerWarning := false
	for _, warn := range result.Warnings {
		if contains(warn, "very high") {
			foundWorkerWarning = true
			break
		}
	}
	if !foundWorkerWarning {
		t.Error("expected high pipeline workers warning")
	}
}

func TestValidate_PartialRAGConfig_Warning(t *testing.T) {
	cfg := Config{
		Relays:          []string{"wss://relay.damus.io"},
		PipelineWorkers: 2,
		DatabaseURL:     ":memory:",
		QdrantURL:       "http://localhost:6333",
		EmbedBaseURL:    "", // Missing
	}

	result := cfg.Validate(context.Background())

	foundRAGWarning := false
	for _, warn := range result.Warnings {
		if contains(warn, "RAG features disabled") {
			foundRAGWarning = true
			break
		}
	}
	if !foundRAGWarning {
		t.Error("expected RAG warning for partial config")
	}
}

func TestFromEnv_ProductionMissingUnsafeConfigReturnsErrors(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DRYDOCK_ENV", "production")

	cfg := FromEnv()
	if !cfg.IsProduction() {
		t.Fatal("expected production mode")
	}

	result := cfg.Validate(context.Background())
	if !result.HasErrors() {
		t.Fatal("expected production validation errors")
	}

	for _, want := range []string{
		"DRYDOCK_RELAYS",
		"DRYDOCK_PLANNER_BASE_URL",
		"DRYDOCK_PLANNER_MODEL",
		"DRYDOCK_CODER32B_BASE_URL",
		"DRYDOCK_CODER32B_MODEL",
		"DRYDOCK_LLM70B_BASE_URL",
		"DRYDOCK_LLM70B_MODEL",
		"DRYDOCK_CODER14B_BASE_URL",
		"DRYDOCK_CODER14B_MODEL",
		"DRYDOCK_META_BASE_URL",
		"DRYDOCK_META_MODEL",
		"DRYDOCK_LLM_API_KEY",
		"DRYDOCK_QDRANT_URL",
		"DRYDOCK_EMBED_BASE_URL",
		"DRYDOCK_EMBED_MODEL",
	} {
		if !hasErrorContaining(result, want) {
			t.Errorf("expected production validation error containing %q; got %#v", want, result.Errors)
		}
	}
}

func TestFromEnv_DevModePermitsLocalhostDefaults(t *testing.T) {
	clearConfigEnv(t)

	cfg := FromEnv()
	if cfg.IsProduction() {
		t.Fatal("expected development mode")
	}
	if cfg.PlannerBaseURL != defaultPlannerBaseURL {
		t.Fatalf("expected dev planner default %q, got %q", defaultPlannerBaseURL, cfg.PlannerBaseURL)
	}
	if cfg.MetaBaseURL != defaultMetaBaseURL {
		t.Fatalf("expected dev meta default %q, got %q", defaultMetaBaseURL, cfg.MetaBaseURL)
	}
	if len(cfg.Relays) == 0 {
		t.Fatal("expected dev public relay defaults")
	}

	result := cfg.Validate(context.Background())
	if result.HasErrors() {
		t.Fatalf("expected dev defaults to validate without fatal errors, got %#v", result.Errors)
	}
}

func TestFromEnv_RateLimitDefaultsAndOverrides(t *testing.T) {
	clearConfigEnv(t)

	cfg := FromEnv()
	if cfg.CodeChatLimit != 20 || cfg.CodeChatWindow != time.Hour {
		t.Fatalf("unexpected codechat rate limit defaults: %d per %s", cfg.CodeChatLimit, cfg.CodeChatWindow)
	}
	if cfg.FeedbackLimit != 100 || cfg.FeedbackWindow != 24*time.Hour {
		t.Fatalf("unexpected feedback rate limit defaults: %d per %s", cfg.FeedbackLimit, cfg.FeedbackWindow)
	}

	t.Setenv("DRYDOCK_CODECHAT_RATE_LIMIT_REQUESTS", "7")
	t.Setenv("DRYDOCK_CODECHAT_RATE_LIMIT_WINDOW", "15m")
	t.Setenv("DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_REQUESTS", "12")
	t.Setenv("DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_WINDOW", "6h")
	cfg = FromEnv()
	if cfg.CodeChatLimit != 7 || cfg.CodeChatWindow != 15*time.Minute {
		t.Fatalf("unexpected codechat rate limit overrides: %d per %s", cfg.CodeChatLimit, cfg.CodeChatWindow)
	}
	if cfg.FeedbackLimit != 12 || cfg.FeedbackWindow != 6*time.Hour {
		t.Fatalf("unexpected feedback rate limit overrides: %d per %s", cfg.FeedbackLimit, cfg.FeedbackWindow)
	}
}

func TestValidate_RateLimitsMustBePositive(t *testing.T) {
	cfg := FromEnv()
	cfg.DatabaseURL = ":memory:"
	cfg.CodeChatLimit = 0
	cfg.CodeChatWindow = 0
	cfg.FeedbackLimit = -1
	cfg.FeedbackWindow = -time.Second

	result := cfg.Validate(context.Background())
	for _, key := range []string{
		"DRYDOCK_CODECHAT_RATE_LIMIT_REQUESTS",
		"DRYDOCK_CODECHAT_RATE_LIMIT_WINDOW",
		"DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_REQUESTS",
		"DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_WINDOW",
	} {
		if !hasErrorContaining(result, key) {
			t.Errorf("expected validation error containing %q; got %#v", key, result.Errors)
		}
	}
}

func TestValidationResult_HasErrors(t *testing.T) {
	tests := []struct {
		name     string
		result   ValidationResult
		expected bool
	}{
		{
			name:     "no errors",
			result:   ValidationResult{Warnings: []string{"warning"}},
			expected: false,
		},
		{
			name:     "has errors",
			result:   ValidationResult{Errors: []string{"error"}},
			expected: true,
		},
		{
			name:     "empty",
			result:   ValidationResult{},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.result.HasErrors(); got != tc.expected {
				t.Errorf("HasErrors() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"DRYDOCK_ENV",
		"DRYDOCK_PRODUCTION",
		"DRYDOCK_RELAYS",
		"DRYDOCK_READ_RELAYS",
		"DRYDOCK_WRITE_RELAYS",
		"DRYDOCK_LLM_API_KEY",
		"DRYDOCK_PLANNER_BASE_URL",
		"DRYDOCK_PLANNER_MODEL",
		"DRYDOCK_PLANNER_API_KEY",
		"DRYDOCK_CODER32B_BASE_URL",
		"DRYDOCK_CODER32B_MODEL",
		"DRYDOCK_CODER32B_API_KEY",
		"DRYDOCK_LLM70B_BASE_URL",
		"DRYDOCK_LLM70B_MODEL",
		"DRYDOCK_LLM70B_API_KEY",
		"DRYDOCK_CODER14B_BASE_URL",
		"DRYDOCK_CODER14B_MODEL",
		"DRYDOCK_CODER14B_API_KEY",
		"DRYDOCK_META_BASE_URL",
		"DRYDOCK_META_MODEL",
		"DRYDOCK_META_API_KEY",
		"DRYDOCK_QDRANT_URL",
		"DRYDOCK_QDRANT_API_KEY",
		"DRYDOCK_EMBED_BASE_URL",
		"DRYDOCK_EMBED_MODEL",
		"DRYDOCK_EMBED_API_KEY",
		"DRYDOCK_CODECHAT_RATE_LIMIT_REQUESTS",
		"DRYDOCK_CODECHAT_RATE_LIMIT_WINDOW",
		"DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_REQUESTS",
		"DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_WINDOW",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("DRYDOCK_DATABASE_URL", ":memory:")
}

func hasErrorContaining(result ValidationResult, substr string) bool {
	for _, err := range result.Errors {
		if contains(err, substr) {
			return true
		}
	}
	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr, 0))
}

func containsAt(s, substr string, start int) bool {
	if start+len(substr) > len(s) {
		return false
	}
	for i := start; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
