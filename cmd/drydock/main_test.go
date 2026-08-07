package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"drydock/internal/driftguard"
)

func TestOpenMigratedStoreCreatesDriftGuardSchema(t *testing.T) {
	ctx := context.Background()
	databaseURL := filepath.Join(t.TempDir(), "drydock.db")

	store, err := openMigratedStore(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open and migrate store: %v", err)
	}
	defer store.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := driftguard.NewService(store, logger)

	var output bytes.Buffer
	if _, err := svc.ExportSample(ctx, &output, 20); err != nil {
		t.Fatalf("export from fresh database: %v", err)
	}
	if _, err := svc.ListFlagged(ctx, &output); err != nil {
		t.Fatalf("list flags from fresh database: %v", err)
	}
}
