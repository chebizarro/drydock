package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"drydock/internal/agenttools"
	"drydock/internal/mcpserver"
	"drydock/internal/workspacesnapshot"
)

func main() {
	var (
		target      = flag.String("target", "", "git repository path to freeze before binding stdio")
		ref         = flag.String("ref", "HEAD", "git ref to pin")
		roleName    = flag.String("role", string(agenttools.RoleExternalReadonly), "bound capability role")
		allowPaths  = flag.String("allow-paths", ".", "comma-separated repository-relative path allowlist")
		patchPath   = flag.String("patch-file", "", "optional authoritative patch file")
		storagePath = flag.String("snapshot-storage", filepath.Join(os.TempDir(), "drydock-mcp-snapshots"), "snapshot descriptor storage")
		maxResult   = flag.Int("max-result-bytes", agenttools.DefaultMaxResultBytes, "per-tool result byte limit")
		ttl         = flag.Duration("snapshot-ttl", 24*time.Hour, "frozen snapshot lifetime")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "drydock-mcp: ", log.LstdFlags)
	if strings.TrimSpace(*target) == "" {
		logger.Fatal("-target is required")
	}
	role := agenttools.Role(strings.TrimSpace(*roleName))
	if !role.Valid() {
		logger.Fatalf("unsupported -role %q", *roleName)
	}
	allowed := splitCSV(*allowPaths)
	if len(allowed) == 0 {
		logger.Fatal("-allow-paths must contain at least one repository-relative path")
	}
	var patch []byte
	var err error
	if strings.TrimSpace(*patchPath) != "" {
		patch, err = os.ReadFile(*patchPath)
		if err != nil {
			logger.Fatalf("read patch: %v", err)
		}
	}

	manager, err := workspacesnapshot.NewManager(workspacesnapshot.Config{
		StorageRoot:     *storagePath,
		SnapshotTTL:     *ttl,
		LeaseTTL:        *ttl,
		SessionLifetime: *ttl,
	})
	if err != nil {
		logger.Fatalf("initialize snapshot manager: %v", err)
	}
	snapshot, err := manager.CreatePinned(context.Background(), workspacesnapshot.PinnedGitOptions{
		RepoPath:  *target,
		Ref:       *ref,
		PatchRef:  filepath.Base(*patchPath),
		Patch:     patch,
		Allowlist: allowed,
		TTL:       *ttl,
	})
	if err != nil {
		logger.Fatalf("freeze target: %v", err)
	}

	scope := agenttools.NewScope("stdio:"+snapshot.ID, snapshot, role)
	scope.MaxResultBytes = *maxResult
	bound, err := mcpserver.NewBoundServer(agenttools.NewRegistry(), scope)
	if err != nil {
		logger.Fatalf("bind MCP server: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := bound.RunStdio(ctx); err != nil && ctx.Err() == nil {
		logger.Fatalf("serve stdio: %v", err)
	}
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
