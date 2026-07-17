// Command lsp-bridge runs the LSP bridge HTTP service.
//
// It manages language server processes (gopls, pyright, tsserver, etc.)
// and exposes a REST API for code analysis.
//
// Usage:
//
//	lsp-bridge [-addr 127.0.0.1:8082] [-auth-token token] [-dev]
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"drydock/internal/lspbridge/server"
)

type bridgeConfig struct {
	addr       string
	authTokens []string
	dev        bool
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := parseConfig(os.Args[1:], os.Getenv)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	mgr := server.NewManager(logger)
	handler := server.NewHandlerWithOptions(mgr, logger, server.HandlerOptions{AuthTokens: cfg.authTokens})

	srv := &http.Server{
		Addr:         cfg.addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("lsp-bridge starting", "addr", cfg.addr, "dev", cfg.dev)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mgr.Shutdown()
	srv.Shutdown(shutdownCtx)
}

func parseConfig(args []string, getenv func(string) string) (bridgeConfig, error) {
	cfg := bridgeConfig{
		addr: firstNonEmpty(getenv("LSP_BRIDGE_ADDR"), "127.0.0.1:8082"),
		dev:  truthy(getenv("LSP_BRIDGE_DEV")),
	}
	fs := flag.NewFlagSet("lsp-bridge", flag.ContinueOnError)
	fs.StringVar(&cfg.addr, "addr", cfg.addr, "listen address")
	authTokens := fs.String("auth-tokens", "", "comma-separated authentication tokens")
	authToken := fs.String("auth-token", "", "authentication token")
	fs.BoolVar(&cfg.dev, "dev", cfg.dev, "allow unauthenticated development mode")
	if err := fs.Parse(args); err != nil {
		return bridgeConfig{}, err
	}

	cfg.authTokens = splitTokens(*authTokens)
	cfg.authTokens = append(cfg.authTokens, splitTokens(*authToken)...)
	if len(cfg.authTokens) == 0 {
		for _, key := range []string{
			"LSP_BRIDGE_AUTH_TOKENS",
			"LSP_BRIDGE_AUTH_TOKEN",
			"DRYDOCK_LSP_BRIDGE_TOKENS",
			"DRYDOCK_LSP_BRIDGE_TOKEN",
		} {
			cfg.authTokens = append(cfg.authTokens, splitTokens(getenv(key))...)
		}
	}
	if len(cfg.authTokens) == 0 && !cfg.dev {
		return bridgeConfig{}, errors.New("at least one authentication token is required (set -auth-token/LSP_BRIDGE_AUTH_TOKEN, or explicitly enable -dev/LSP_BRIDGE_DEV)")
	}
	return cfg, nil
}

func splitTokens(value string) []string {
	var tokens []string
	for _, token := range strings.Split(value, ",") {
		if token = strings.TrimSpace(token); token != "" {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}
