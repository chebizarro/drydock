package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"drydock/internal/agenttools"
	"drydock/internal/driftguard"
	"drydock/internal/reviewsession"
	"drydock/internal/workspacesnapshot"
)

func TestResolveMCPHTTPScopeBindsServerSessionReadonly(t *testing.T) {
	ctx := context.Background()
	store, err := openMigratedStore(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sessions, err := reviewsession.NewSQLiteStore(store.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, snapshot := testReviewSnapshot(t, time.Hour)
	chatID := createReviewSession(t, sessions, manager, snapshot, time.Now().Add(time.Hour))

	scope, err := resolveMCPHTTPScope(ctx, chatID, sessions, manager)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Role != agenttools.RoleExternalReadonly || scope.Snapshot.ID != snapshot.ID ||
		scope.ID == "" {
		t.Fatalf("unexpected MCP scope: %#v", scope)
	}
}

func TestRunReviewLifecycleExpiresSessionBeforeSnapshotGC(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := openMigratedStore(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	past := now.Add(-2 * time.Hour)
	sessions, err := reviewsession.NewSQLiteStore(store.DB(), func() time.Time { return past })
	if err != nil {
		t.Fatal(err)
	}
	manager, snapshot := testReviewSnapshot(t, time.Hour)
	chatID := createReviewSession(t, sessions, manager, snapshot, now.Add(-time.Hour))
	snapshot.ExpiresAt = now.Add(-time.Hour)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan struct{})
	go func() {
		runReviewLifecycle(ctx, 5*time.Millisecond, sessions, manager, logger)
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		loaded, loadErr := sessions.LoadForContinuation(context.Background(), chatID)
		_, snapshotErr := manager.Get(snapshot.ID)
		if loadErr == nil && loaded.Session.State == reviewsession.StateExpired &&
			errors.Is(snapshotErr, workspacesnapshot.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lifecycle did not expire and collect binding: state=%v load_err=%v snapshot_err=%v",
				loaded.Session.State, loadErr, snapshotErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func testReviewSnapshot(t *testing.T, ttl time.Duration) (*workspacesnapshot.Manager, *workspacesnapshot.Snapshot) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := workspacesnapshot.NewManager(workspacesnapshot.Config{
		StorageRoot: t.TempDir(), SnapshotTTL: ttl, LeaseTTL: ttl, SessionLifetime: ttl,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.CreateMutable(context.Background(), workspacesnapshot.MutableCopyOptions{
		WorkspacePath: workspace, Allowlist: []string{"."}, TTL: ttl,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, snapshot
}

func createReviewSession(t *testing.T, sessions *reviewsession.SQLStore, manager *workspacesnapshot.Manager, snapshot *workspacesnapshot.Snapshot, expires time.Time) string {
	t.Helper()
	lease, err := manager.Acquire(snapshot.ID, "test-session", time.Until(expires))
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := sessions.Create(context.Background(), reviewsession.CreateParams{
		Owner: reviewsession.Owner{Kind: "test", ID: "owner"}, Mode: reviewsession.ModePatch,
		Snapshot: reviewsession.Snapshot{
			ID: snapshot.ID, Kind: string(snapshot.SnapshotKind()), StoragePath: snapshot.StoragePath(),
			ManifestHash: snapshot.ManifestDigest(), DiffHash: snapshot.PatchDigest(), ExpiresAt: snapshot.ExpiresAt,
		},
		TargetEnvelope: []byte(`{}`), BundleHash: "bundle", LeaseID: lease.ID,
		RequestID: "request-1", ExpiresAt: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.CompleteTurn(context.Background(), reservation.Session.ChatID, "request-1", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	return reservation.Session.ChatID
}

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
