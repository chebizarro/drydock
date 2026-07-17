package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"

	"drydock/internal/config"
	"drydock/internal/db"
	"drydock/internal/eval"
	"drydock/internal/promptrefine"
	"drydock/internal/reviewengine"
)

func main() {
	cfg := config.FromEnv()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	ctx := context.Background()

	validationResult := cfg.Validate(ctx)
	validationResult.Log(logger)
	if validationResult.HasErrors() {
		logger.Error("configuration validation failed, exiting")
		os.Exit(1)
	}

	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("open db failed", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		logger.Error("migrate failed", "error", err)
		os.Exit(1)
	}

	ds, err := eval.LoadDataset(cfg.EvalDatasetPath)
	if err != nil {
		logger.Error("load eval dataset failed", "path", cfg.EvalDatasetPath, "error", err)
		os.Exit(1)
	}

	engine := reviewengine.New(reviewengine.Config{
		Planner:      reviewengine.ModelEndpoint{BaseURL: cfg.PlannerBaseURL, APIKey: cfg.EffectiveLLMAPIKey(cfg.PlannerAPIKey), Model: cfg.PlannerModel},
		Coder32B:     reviewengine.ModelEndpoint{BaseURL: cfg.Coder32BBaseURL, APIKey: cfg.EffectiveLLMAPIKey(cfg.Coder32BAPIKey), Model: cfg.Coder32BModel},
		LLM70B:       reviewengine.ModelEndpoint{BaseURL: cfg.LLM70BBaseURL, APIKey: cfg.EffectiveLLMAPIKey(cfg.LLM70BAPIKey), Model: cfg.LLM70BModel},
		Coder14B:     reviewengine.ModelEndpoint{BaseURL: cfg.Coder14BBaseURL, APIKey: cfg.EffectiveLLMAPIKey(cfg.Coder14BAPIKey), Model: cfg.Coder14BModel},
		PlannerTemp:  0.1,
		ReviewerTemp: 0.1,
	}, reviewengine.NewOpenAICompatClient(), logger)

	h := eval.Harness{
		Runner:        eval.EngineRunner{Engine: engine},
		Store:         store,
		Logger:        logger,
		LineTolerance: eval.DefaultLineTolerance,
	}
	metrics, err := h.RunMonthly(ctx, ds)
	if err != nil {
		logger.Error("eval run failed", "error", err)
		os.Exit(1)
	}

	out, _ := json.MarshalIndent(metrics, "", "  ")
	_, _ = os.Stdout.Write(out)
	_, _ = os.Stdout.WriteString("\n")

	// --- Prompt refinement eval gate ---
	// After each eval run, check if the active prompt version should be
	// rolled back due to eval score regression.
	prSvc := promptrefine.New(promptrefine.Config{
		EvalScoreTolerance: 0.05,
	}, store, nil, logger) // nil LLM client — rollback doesn't need LLM
	rbResult, err := prSvc.EvalAndMaybeRollback(ctx)
	if err != nil {
		logger.Error("prompt version eval gate failed", "error", err)
		os.Exit(1)
	}
	if rbResult.RolledBack {
		logger.Error("prompt version eval gate failed; prompt rolled back",
			"reason", rbResult.RollbackReason,
		)
		os.Exit(1)
	}
}
