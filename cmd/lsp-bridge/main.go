// Command lsp-bridge runs the LSP bridge HTTP service.
//
// It manages language server processes (gopls, pyright, tsserver, etc.)
// and exposes a REST API for code analysis.
//
// Usage:
//
//	lsp-bridge [-addr :8082]
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"drydock/internal/lspbridge/server"
)

func main() {
	addr := flag.String("addr", ":8082", "listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mgr := server.NewManager(logger)
	handler := server.NewHandler(mgr, logger)

	srv := &http.Server{
		Addr:         *addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("lsp-bridge starting", "addr", *addr)
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
