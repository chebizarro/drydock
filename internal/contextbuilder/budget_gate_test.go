package contextbuilder_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/agenttools"
	"git.sharegap.net/cascadia/drydock/internal/config"
	"git.sharegap.net/cascadia/drydock/internal/contextbuilder"
	"git.sharegap.net/cascadia/drydock/internal/repoconfig"
)

type largeBudgetProvider struct{}

func (largeBudgetProvider) LayerName() string { return "large-context" }
func (largeBudgetProvider) Priority() int     { return 2 }
func (largeBudgetProvider) Build(context.Context, contextbuilder.BuildInput) (string, error) {
	return strings.Repeat("source context to review\n", 30000), nil
}

func TestDeterministicBudgetPassesPackageGate(t *testing.T) {
	counter, err := contextbuilder.NewRequiredTiktokenCounter("cl100k_base")
	if err != nil {
		t.Fatal(err)
	}
	const lines = 30000
	patch := fmt.Sprintf("diff --git a/large.go b/large.go\nnew file mode 100644\n--- /dev/null\n+++ b/large.go\n@@ -0,0 +1,%d @@\n", lines) + strings.Repeat("+// some source code to review\n", lines)
	for _, tc := range []struct {
		name                    string
		operator, override, pkg int
		headroom                string
	}{
		{name: "default", operator: 64000},
		{name: "small operator", operator: 24000},
		{name: "repo cannot raise", operator: 24000, override: 128000},
		{name: "repo can lower", operator: 24000, override: 12000},
		{name: "larger package", operator: 24000, pkg: 64000},
		{name: "custom headroom", operator: 24000, headroom: "0.25"},
		{name: "zero selects default headroom", operator: 24000, headroom: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DRYDOCK_CONTEXT_TOKEN_BUDGET", strconv.Itoa(tc.operator))
			t.Setenv("DRYDOCK_AGENTIC_PACKAGE_TOKEN_BUDGET", "")
			t.Setenv("DRYDOCK_AGENTIC_TOKEN_HEADROOM", tc.headroom)
			if tc.pkg > 0 {
				t.Setenv("DRYDOCK_AGENTIC_PACKAGE_TOKEN_BUDGET", strconv.Itoa(tc.pkg))
			}
			cfg := config.FromEnv()
			repoCfg, err := repoconfig.Parse([]byte(fmt.Sprintf("context:\n  token_budget: %d\n", tc.override)))
			if err != nil {
				t.Fatal(err)
			}
			builder := contextbuilder.NewWithOptions(contextbuilder.NewBuilderOptions(
				contextbuilder.WithTokenBudget(cfg.ContextTokenBudget, cfg.AgenticTokenHeadroom),
				contextbuilder.WithExtraProviders(largeBudgetProvider{}),
			))
			bundle, err := builder.Build(context.Background(), contextbuilder.BuildInput{
				PatchEventContent: patch, TokenBudgetOverride: repoCfg.Context.TokenBudget,
			})
			if err != nil {
				t.Fatal(err)
			}
			nominal := tc.operator
			if tc.override > 0 && tc.override < nominal {
				nominal = tc.override
			}
			limit := contextbuilder.EffectiveTokenLimit(nominal, cfg.AgenticTokenHeadroom)
			exact := counter.Count(bundle.Content)
			if bundle.TokenBudget != limit || bundle.TokenCount != exact || exact > limit || exact < limit-32 {
				t.Fatalf("budget=%d count=%d exact=%d, want trimmed rendered bundle near limit=%d", bundle.TokenBudget, bundle.TokenCount, exact, limit)
			}
			if builder.TokenBudget != tc.operator {
				t.Fatalf("repo override mutated operator budget: %d", builder.TokenBudget)
			}
			if _, err := agenttools.GateBundle(bundle, counter, cfg.AgenticPackageTokenBudget, cfg.AgenticTokenHeadroom); err != nil {
				t.Fatalf("builder produced a bundle rejected by the single-shot gate: %v", err)
			}
		})
	}
}
