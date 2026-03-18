package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"drydock/internal/config"
	"drydock/internal/db"
	"drydock/internal/ingest"
	"drydock/internal/listener"
)

func main() {
	cfg := config.FromEnv()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		logger.Error("failed to migrate database", "error", err)
		os.Exit(1)
	}

	processor := ingest.NewProcessor(store, logger)
	svc := listener.New(listener.Config{
		Relays:          cfg.Relays,
		LookbackMinutes: cfg.ListenerLookbackMin,
	}, processor, logger)

	if err := svc.Run(ctx); err != nil {
		logger.Error("listener exited with error", "error", err)
		os.Exit(1)
	}
}

