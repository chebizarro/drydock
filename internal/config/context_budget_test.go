package config

import (
	"context"
	"strings"
	"testing"
)

func TestContextTokenBudgetFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name, context, pkg       string
		wantContext, wantPackage int
		wantError                string
	}{
		{name: "defaults", wantContext: 64000, wantPackage: 64000},
		{name: "small inherited", context: "24000", wantContext: 24000, wantPackage: 24000},
		{name: "explicit larger package", context: "24000", pkg: "64000", wantContext: 24000, wantPackage: 64000},
		{name: "explicit equal package", context: "24000", pkg: "24000", wantContext: 24000, wantPackage: 24000},
		{name: "explicit smaller package", context: "24000", pkg: "16000", wantContext: 24000, wantPackage: 16000, wantError: "DRYDOCK_AGENTIC_PACKAGE_TOKEN_BUDGET must be at least DRYDOCK_CONTEXT_TOKEN_BUDGET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DRYDOCK_CONTEXT_TOKEN_BUDGET", tc.context)
			t.Setenv("DRYDOCK_AGENTIC_PACKAGE_TOKEN_BUDGET", tc.pkg)
			cfg := FromEnv()
			if cfg.ContextTokenBudget != tc.wantContext || cfg.AgenticPackageTokenBudget != tc.wantPackage {
				t.Fatalf("context=%d package=%d, want %d/%d", cfg.ContextTokenBudget, cfg.AgenticPackageTokenBudget, tc.wantContext, tc.wantPackage)
			}
			result := cfg.Validate(context.Background())
			var budgetErrors []string
			for _, err := range result.Errors {
				if strings.Contains(err, "DRYDOCK_CONTEXT_TOKEN_BUDGET") {
					budgetErrors = append(budgetErrors, err)
				}
			}
			if tc.wantError == "" && len(budgetErrors) != 0 || tc.wantError != "" && (len(budgetErrors) != 1 || budgetErrors[0] != tc.wantError) {
				t.Fatalf("budget errors = %v, want %q", budgetErrors, tc.wantError)
			}
		})
	}
}

func TestContextTokenBudgetValidation(t *testing.T) {
	for _, value := range []string{"0", "-1", "1023", "1048577", "nope", "24000junk", "24000.5", "999999999999999999999999"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("DRYDOCK_CONTEXT_TOKEN_BUDGET", value)
			t.Setenv("DRYDOCK_AGENTIC_PACKAGE_TOKEN_BUDGET", "")
			cfg := FromEnv()
			result := cfg.Validate(context.Background())
			count := 0
			for _, err := range result.Errors {
				if strings.Contains(err, "DRYDOCK_CONTEXT_TOKEN_BUDGET") {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("want one clear context-budget error, got %v", result.Errors)
			}
		})
	}
	for _, value := range []string{"1024", "1048576"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("DRYDOCK_CONTEXT_TOKEN_BUDGET", value)
			t.Setenv("DRYDOCK_AGENTIC_PACKAGE_TOKEN_BUDGET", "")
			cfg := FromEnv()
			if result := cfg.Validate(context.Background()); hasErrorContaining(result, "DRYDOCK_CONTEXT_TOKEN_BUDGET") {
				t.Fatalf("valid boundary rejected: %v", result.Errors)
			}
		})
	}
}
