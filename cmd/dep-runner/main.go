// Command dep-runner runs the dependency-upgrade sandbox as an HTTP sidecar.
//
// It runs package-manager toolchains (go, npm, cargo, pip) against untrusted
// repository manifests in an isolated temp root and returns the resulting
// manifest/lockfile edits. It mirrors cmd/lsp-bridge: a lean, separately-built
// binary that ships in its own toolchain image behind a Docker Compose profile,
// with a bearer token and a /healthz probe. drydock reaches it over HTTP and
// gains no toolchains or host privilege of its own.
//
// Usage:
//
//	dep-runner [-addr 0.0.0.0:8083] [-auth-token token] [-dev]
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/deprunner/server"
)

type runnerConfig struct {
	addr       string
	authTokens []string
	dev        bool
	opts       server.Options
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := parseConfig(os.Args[1:], os.Getenv)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	sandbox := server.NewSandbox(cfg.opts, logger)
	handler := server.NewHandler(sandbox, logger, server.HandlerOptions{AuthTokens: cfg.authTokens})

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		// No WriteTimeout: a lockfile solve can legitimately run for minutes; the
		// sandbox enforces its own per-request wall-clock ceiling instead.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("dep-runner starting", "addr", cfg.addr, "dev", cfg.dev, "allow_scripts", cfg.opts.AllowScripts)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func parseConfig(args []string, getenv func(string) string) (runnerConfig, error) {
	cfg := runnerConfig{
		addr: firstNonEmpty(getenv("DEP_RUNNER_ADDR"), "0.0.0.0:8083"),
		dev:  truthy(getenv("DEP_RUNNER_DEV")),
		opts: server.Options{
			WorkRoot:      firstNonEmpty(getenv("DEP_RUNNER_WORK_ROOT"), ""),
			AllowScripts:  truthy(getenv("DEP_RUNNER_ALLOW_SCRIPTS")),
			GoProxy:       firstNonEmpty(getenv("DEP_RUNNER_GOPROXY"), getenv("GOPROXY")),
			GoSumDB:       getenv("DEP_RUNNER_GOSUMDB"),
			NPMRegistry:   getenv("DEP_RUNNER_NPM_REGISTRY"),
			CargoRegistry: getenv("DEP_RUNNER_CARGO_REGISTRY"),
			PipIndexURL:   getenv("DEP_RUNNER_PIP_INDEX_URL"),
			ToolchainPath: getenv("DEP_RUNNER_TOOLCHAIN_PATH"),
		},
	}

	fs := flag.NewFlagSet("dep-runner", flag.ContinueOnError)
	fs.StringVar(&cfg.addr, "addr", cfg.addr, "listen address")
	authToken := fs.String("auth-token", "", "authentication token")
	authTokens := fs.String("auth-tokens", "", "comma-separated authentication tokens")
	fs.BoolVar(&cfg.dev, "dev", cfg.dev, "allow unauthenticated development mode")
	allowScripts := fs.Bool("allow-scripts", cfg.opts.AllowScripts, "permit package lifecycle scripts (dangerous; operator-only)")
	if err := fs.Parse(args); err != nil {
		return runnerConfig{}, err
	}
	cfg.opts.AllowScripts = *allowScripts

	if to := strings.TrimSpace(getenv("DEP_RUNNER_TIMEOUT")); to != "" {
		d, err := time.ParseDuration(to)
		if err != nil {
			return runnerConfig{}, errors.New("DEP_RUNNER_TIMEOUT: " + err.Error())
		}
		cfg.opts.Timeout = d
	}
	if mb := strings.TrimSpace(getenv("DEP_RUNNER_MAX_MANIFEST_BYTES")); mb != "" {
		n, err := strconv.Atoi(mb)
		if err != nil {
			return runnerConfig{}, errors.New("DEP_RUNNER_MAX_MANIFEST_BYTES: " + err.Error())
		}
		cfg.opts.MaxManifestBytes = n
	}

	cfg.authTokens = splitTokens(*authTokens)
	cfg.authTokens = append(cfg.authTokens, splitTokens(*authToken)...)
	if len(cfg.authTokens) == 0 {
		for _, key := range []string{"DEP_RUNNER_AUTH_TOKENS", "DEP_RUNNER_AUTH_TOKEN"} {
			cfg.authTokens = append(cfg.authTokens, splitTokens(getenv(key))...)
		}
	}
	if len(cfg.authTokens) == 0 && !cfg.dev {
		return runnerConfig{}, errors.New("at least one authentication token is required (set -auth-token/DEP_RUNNER_AUTH_TOKEN, or explicitly enable -dev/DEP_RUNNER_DEV)")
	}
	return cfg, nil
}

// splitTokens mirrors the lean helper in cmd/lsp-bridge: this sidecar avoids
// linking internal/config's transitive deps (the SQLite driver, vectorstore).
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
