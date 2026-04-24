package config

import (
	"context"
	"testing"
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
