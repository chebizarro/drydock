package repo

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"fiatjaf.com/nostr"
)

type Manager struct {
	baseDir   string
	logger    *slog.Logger
	repoLocks sync.Map
}

func NewManager(baseDir string, logger *slog.Logger) *Manager {
	return &Manager{
		baseDir: baseDir,
		logger:  logger,
	}
}

func (m *Manager) EnsureRepo(ctx context.Context, repoID string, cloneURLs []string) (string, error) {
	if len(cloneURLs) == 0 {
		return "", fmt.Errorf("no clone urls for repository %s", repoID)
	}
	repoPath := m.repoPath(repoID)

	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil {
		if _, err := m.runGit(ctx, repoPath, "fetch", "--all", "--prune"); err != nil {
			return "", fmt.Errorf("git fetch: %w", err)
		}
		return repoPath, nil
	}

	if err := os.MkdirAll(filepath.Dir(repoPath), 0o755); err != nil {
		return "", fmt.Errorf("create repo cache dir: %w", err)
	}
	// Safe: len(cloneURLs) > 0 checked above
	cloneURL := cloneURLs[0]
	if out, err := exec.CommandContext(ctx, "git", "clone", cloneURL, repoPath).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone %s: %w: %s", cloneURL, err, strings.TrimSpace(string(out)))
	}
	return repoPath, nil
}

func (m *Manager) ApplyPatchSeries(ctx context.Context, repoPath, patchEventID string, events []nostr.Event) (string, error) {
	if len(events) == 0 {
		return "", fmt.Errorf("empty patch series")
	}
	branch := "review/" + shortID(patchEventID)

	mu := m.getRepoLock(repoPath)
	mu.Lock()
	defer mu.Unlock()

	if _, err := m.runGit(ctx, repoPath, "reset", "--hard", "HEAD"); err != nil {
		return branch, fmt.Errorf("git reset before apply: %w", err)
	}
	if _, err := m.runGit(ctx, repoPath, "clean", "-fd"); err != nil {
		return branch, fmt.Errorf("git clean before apply: %w", err)
	}

	if _, err := m.runGit(ctx, repoPath, "checkout", "-B", branch); err != nil {
		return branch, fmt.Errorf("checkout throwaway branch: %w", err)
	}

	for _, event := range events {
		if err := m.applySinglePatch(ctx, repoPath, event.Content); err != nil {
			return branch, fmt.Errorf("patch %s does not apply cleanly: %w", event.ID.Hex(), err)
		}
	}

	return branch, nil
}

func (m *Manager) CleanupReviewBranch(ctx context.Context, repoPath, branch string) error {
	if branch == "" {
		return nil
	}
	if _, err := m.runGit(ctx, repoPath, "checkout", "-"); err != nil {
		m.logger.Warn("failed to move off review branch before cleanup", "branch", branch, "error", err)
	}
	if _, err := m.runGit(ctx, repoPath, "branch", "-D", branch); err != nil {
		return fmt.Errorf("delete review branch %s: %w", branch, err)
	}
	return nil
}

func (m *Manager) applySinglePatch(ctx context.Context, repoPath, patchContent string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "apply", "--3way", "--index", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open stdin: %w", err)
	}
	
	errCh := make(chan error, 1)
	go func() {
		defer stdin.Close()
		select {
		case <-ctx.Done():
			errCh <- ctx.Err()
			return
		default:
		}
		_, writeErr := io.WriteString(stdin, patchContent)
		errCh <- writeErr
	}()
	
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	
	if writeErr := <-errCh; writeErr != nil && writeErr != io.ErrClosedPipe {
		return fmt.Errorf("write patch content: %w", writeErr)
	}
	return nil
}

func (m *Manager) runGit(ctx context.Context, repoPath string, args ...string) (string, error) {
	fullArgs := append([]string{"-C", repoPath}, args...)
	out, err := exec.CommandContext(ctx, "git", fullArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (m *Manager) repoPath(repoID string) string {
	safe := strings.NewReplacer("/", "_", "\\", "_", ":", "__", " ", "_").Replace(repoID)
	return filepath.Join(m.baseDir, safe)
}

func (m *Manager) getRepoLock(repoPath string) *sync.Mutex {
	mu, _ := m.repoLocks.LoadOrStore(repoPath, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func shortID(v string) string {
	if len(v) <= 12 {
		return v
	}
	return v[:12]
}

