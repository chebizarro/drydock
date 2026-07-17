package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"drydock/internal/metrics"

	"fiatjaf.com/nostr"
)

type Manager struct {
	baseDir      string
	logger       *slog.Logger
	repoLocks    sync.Map
	evictionMu   sync.Mutex
	maxCount     int   // 0 = unlimited
	maxSizeBytes int64 // 0 = unlimited
}

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithMaxRepoCount sets the maximum number of cached repos before LRU eviction.
func WithMaxRepoCount(n int) ManagerOption {
	return func(m *Manager) { m.maxCount = n }
}

// WithMaxCacheSizeMB sets the maximum total cache size in megabytes.
func WithMaxCacheSizeMB(mb int) ManagerOption {
	return func(m *Manager) { m.maxSizeBytes = int64(mb) * 1024 * 1024 }
}

func NewManager(baseDir string, logger *slog.Logger, opts ...ManagerOption) *Manager {
	m := &Manager{
		baseDir: baseDir,
		logger:  logger,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Git operation timeouts.
const (
	gitCloneTimeout = 5 * time.Minute
	gitFetchTimeout = 2 * time.Minute
	gitApplyTimeout = 1 * time.Minute
)

func (m *Manager) EnsureRepo(ctx context.Context, repoID string, cloneURLs []string) (string, error) {
	return m.ensureRepoAtPath(ctx, m.repoPath(repoID), repoID, cloneURLs)
}

// EnsureCanonicalRepo ensures a clone of the canonical repository under a
// cache entry that cannot be shared with PR/fork clones for the same repo ID.
func (m *Manager) EnsureCanonicalRepo(ctx context.Context, repoID string, cloneURLs []string) (string, error) {
	return m.ensureRepoAtPath(ctx, m.canonicalRepoPath(repoID), repoID, cloneURLs)
}

func (m *Manager) ensureRepoAtPath(ctx context.Context, repoPath, repoID string, cloneURLs []string) (string, error) {
	if len(cloneURLs) == 0 {
		return "", fmt.Errorf("no clone urls for repository %s", repoID)
	}

	validatedPath, err := m.validateRepoPath(repoPath)
	if err != nil {
		return "", err
	}
	repoPath = validatedPath

	mu := m.getRepoLock(repoPath)
	mu.Lock()
	defer mu.Unlock()

	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil {
		fetchCtx, cancel := context.WithTimeout(ctx, gitFetchTimeout)
		defer cancel()
		doneTimer := metrics.TimerVec(metrics.GitOpDuration, "fetch")
		defer doneTimer()
		if _, err := m.runGit(fetchCtx, repoPath, "fetch", "--all", "--prune"); err != nil {
			return "", fmt.Errorf("git fetch: %w", err)
		}
		m.touchAccess(repoPath)
		return repoPath, nil
	}

	// Evict before cloning to make room
	m.evictIfNeeded()

	if err := os.MkdirAll(filepath.Dir(repoPath), 0o755); err != nil {
		return "", fmt.Errorf("create repo cache dir: %w", err)
	}

	// Find first safe clone URL
	var cloneURL string
	for _, u := range cloneURLs {
		if isSafeCloneURL(u) {
			cloneURL = u
			break
		}
		m.logger.Warn("skipping unsafe clone URL", "url", u, "repo_id", repoID)
	}
	if cloneURL == "" {
		return "", fmt.Errorf("no safe clone urls for repository %s", repoID)
	}

	cloneCtx, cloneCancel := context.WithTimeout(ctx, gitCloneTimeout)
	defer cloneCancel()
	doneClone := metrics.TimerVec(metrics.GitOpDuration, "clone")
	defer doneClone()
	validatedPath, err = m.validateRepoPath(repoPath)
	if err != nil {
		return "", err
	}
	if out, err := exec.CommandContext(cloneCtx, "git", "clone", cloneURL, validatedPath).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone %s: %w: %s", cloneURL, err, strings.TrimSpace(string(out)))
	}
	m.touchAccess(repoPath)
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

func (m *Manager) EnsureCommitAvailable(ctx context.Context, repoPath, eventID, commit string, cloneURLs []string) error {
	if strings.TrimSpace(commit) == "" {
		return fmt.Errorf("commit is required")
	}

	validatedPath, err := m.validateRepoPath(repoPath)
	if err != nil {
		return err
	}
	repoPath = validatedPath

	mu := m.getRepoLock(repoPath)
	mu.Lock()
	defer mu.Unlock()

	if _, err := m.runGit(ctx, repoPath, "cat-file", "-e", commit+"^{commit}"); err == nil {
		return nil
	}

	for _, cloneURL := range cloneURLs {
		cloneURL = strings.TrimSpace(cloneURL)
		if cloneURL == "" || !isSafeCloneURL(cloneURL) {
			continue
		}
		if eventID != "" {
			if _, err := m.runGit(ctx, repoPath, "fetch", "--no-tags", cloneURL, "refs/nostr/"+eventID+":refs/drydock/nostr/"+eventID); err == nil {
				if _, err := m.runGit(ctx, repoPath, "cat-file", "-e", commit+"^{commit}"); err == nil {
					return nil
				}
			}
		}
		if _, err := m.runGit(ctx, repoPath, "fetch", "--no-tags", cloneURL, commit); err == nil {
			if _, err := m.runGit(ctx, repoPath, "cat-file", "-e", commit+"^{commit}"); err == nil {
				return nil
			}
		}
	}

	if _, err := m.runGit(ctx, repoPath, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return fmt.Errorf("commit %s not available after fetch attempts", commit)
	}
	return nil
}

func (m *Manager) CheckoutCommitOnBranch(ctx context.Context, repoPath, branch, commit string) error {
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("branch is required")
	}
	if strings.TrimSpace(commit) == "" {
		return fmt.Errorf("commit is required")
	}

	mu := m.getRepoLock(repoPath)
	mu.Lock()
	defer mu.Unlock()

	if _, err := m.runGit(ctx, repoPath, "reset", "--hard", "HEAD"); err != nil {
		return fmt.Errorf("git reset before checkout: %w", err)
	}
	if _, err := m.runGit(ctx, repoPath, "clean", "-fd"); err != nil {
		return fmt.Errorf("git clean before checkout: %w", err)
	}
	if _, err := m.runGit(ctx, repoPath, "fetch", "--all", "--prune"); err != nil {
		return fmt.Errorf("git fetch before checkout: %w", err)
	}
	if _, err := m.runGit(ctx, repoPath, "checkout", "-B", branch, commit); err != nil {
		return fmt.Errorf("checkout branch %s at %s: %w", branch, commit, err)
	}
	return nil
}

// ReadFileAtRef reads a file from the repository at the specified git ref
// without touching the working tree. Returns the file content, or an error.
// ErrNotFound is returned if the file doesn't exist at that ref.
func (m *Manager) ReadFileAtRef(ctx context.Context, repoPath, ref, relPath string) ([]byte, error) {
	mu := m.getRepoLock(repoPath)
	mu.Lock()
	defer mu.Unlock()

	out, err := m.runGit(ctx, repoPath, "show", ref+":"+relPath)
	if err != nil {
		return nil, fmt.Errorf("read %s at %s: %w", relPath, ref, err)
	}
	return []byte(out), nil
}

// ReadFileAtDefaultRef reads a file from the canonical default branch.
// Tries refs/remotes/origin/HEAD first, falls back to HEAD.
// Returns nil, nil if the file doesn't exist.
func (m *Manager) ReadFileAtDefaultRef(ctx context.Context, repoPath, relPath string) ([]byte, error) {
	// Try origin/HEAD first (canonical upstream ref).
	data, err := m.ReadFileAtRef(ctx, repoPath, "refs/remotes/origin/HEAD", relPath)
	if err == nil {
		return data, nil
	}
	// Fallback to HEAD.
	data, err = m.ReadFileAtRef(ctx, repoPath, "HEAD", relPath)
	if err != nil {
		// File doesn't exist at HEAD either — not an error for optional config.
		return nil, nil
	}
	return data, nil
}

func (m *Manager) CleanupReviewBranch(ctx context.Context, repoPath, branch string) error {
	if branch == "" {
		return nil
	}

	validatedPath, err := m.validateRepoPath(repoPath)
	if err != nil {
		return err
	}
	repoPath = validatedPath

	mu := m.getRepoLock(repoPath)
	mu.Lock()
	defer mu.Unlock()

	if _, err := m.runGit(ctx, repoPath, "checkout", "-"); err != nil {
		m.logger.Warn("failed to move off review branch before cleanup", "branch", branch, "error", err)
	}
	if _, err := m.runGit(ctx, repoPath, "branch", "-D", branch); err != nil {
		return fmt.Errorf("delete review branch %s: %w", branch, err)
	}
	return nil
}

func (m *Manager) applySinglePatch(ctx context.Context, repoPath, patchContent string) error {
	validatedPath, err := m.validateRepoPath(repoPath)
	if err != nil {
		return err
	}

	applyCtx, cancel := context.WithTimeout(ctx, gitApplyTimeout)
	defer cancel()
	doneApply := metrics.TimerVec(metrics.GitOpDuration, "apply")
	defer doneApply()
	cmd := exec.CommandContext(applyCtx, "git", "-C", validatedPath, "apply", "--3way", "--index", "-")
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
	validatedPath, err := m.validateRepoPath(repoPath)
	if err != nil {
		return "", err
	}
	fullArgs := append([]string{"-C", validatedPath}, args...)
	out, err := exec.CommandContext(ctx, "git", fullArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (m *Manager) repoPath(repoID string) string {
	return filepath.Join(m.baseDir, safeRepoPathComponent(repoID))
}

func (m *Manager) canonicalRepoPath(repoID string) string {
	return filepath.Join(m.baseDir, hashedRepoPathComponent("canonical\x00"+repoID))
}

func safeRepoPathComponent(repoID string) string {
	return hashedRepoPathComponent(repoID)
}

func hashedRepoPathComponent(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// validateRepoPath resolves symlinks in all existing path components and
// rejects paths that are not strictly beneath the configured cache directory.
func (m *Manager) validateRepoPath(repoPath string) (string, error) {
	basePath, err := resolvePath(m.baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve repo cache directory: %w", err)
	}
	resolvedRepoPath, err := resolvePath(repoPath)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}
	rel, err := filepath.Rel(basePath, resolvedRepoPath)
	if err != nil {
		return "", fmt.Errorf("compare repo path to cache directory: %w", err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("repo path %q is not strictly beneath cache directory %q", repoPath, m.baseDir)
	}
	operationPath, err := filepath.Abs(repoPath)
	if err != nil {
		return "", fmt.Errorf("make repo path absolute: %w", err)
	}
	return filepath.Clean(operationPath), nil
}

// resolvePath evaluates symlinks in the longest existing prefix, allowing it
// to safely resolve a cache path before the cache directory or repo exists.
func resolvePath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absPath)
	var missing []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for %q", path)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
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

const accessMarkerFile = ".drydock-accessed"

// touchAccess updates the access marker for LRU tracking.
func (m *Manager) touchAccess(repoPath string) {
	validatedPath, err := m.validateRepoPath(repoPath)
	if err != nil {
		m.logger.Debug("refusing to touch access marker outside cache", "path", repoPath, "error", err)
		return
	}
	marker := filepath.Join(validatedPath, accessMarkerFile)
	now := time.Now()
	if err := os.WriteFile(marker, []byte(now.Format(time.RFC3339)), 0o644); err != nil {
		m.logger.Debug("failed to touch access marker", "path", marker, "error", err)
	}
}

// repoAccessTime returns the last access time for a cached repo.
func repoAccessTime(repoPath string) time.Time {
	marker := filepath.Join(repoPath, accessMarkerFile)
	info, err := os.Stat(marker)
	if err != nil {
		// Fall back to .git directory mod time
		info, err = os.Stat(filepath.Join(repoPath, ".git"))
		if err != nil {
			return time.Time{} // epoch — oldest possible
		}
	}
	return info.ModTime()
}

type cachedRepo struct {
	path       string
	accessTime time.Time
	sizeBytes  int64
}

// listCachedRepos enumerates repos in the cache directory.
func (m *Manager) listCachedRepos() ([]cachedRepo, error) {
	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var repos []cachedRepo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		repoPath, err := m.validateRepoPath(filepath.Join(m.baseDir, e.Name()))
		if err != nil {
			m.logger.Warn("cache listing: refusing path outside cache", "entry", e.Name(), "error", err)
			continue
		}
		mu := m.getRepoLock(repoPath)
		if !mu.TryLock() {
			continue
		}
		// Only count directories that look like git repos.
		if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
			mu.Unlock()
			continue
		}
		size := dirSize(repoPath)
		accessTime := repoAccessTime(repoPath)
		mu.Unlock()
		repos = append(repos, cachedRepo{
			path:       repoPath,
			accessTime: accessTime,
			sizeBytes:  size,
		})
	}
	return repos, nil
}

// dirSize walks a directory to compute total size in bytes.
func dirSize(path string) int64 {
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// evictIfNeeded removes least-recently-used repos until under configured limits.
func (m *Manager) evictIfNeeded() {
	if m.maxCount <= 0 && m.maxSizeBytes <= 0 {
		return // no limits configured
	}

	m.evictionMu.Lock()
	defer m.evictionMu.Unlock()

	repos, err := m.listCachedRepos()
	if err != nil {
		m.logger.Warn("cache eviction: failed to list repos", "error", err)
		return
	}

	// Sort by access time ascending (oldest first = evict first)
	sort.Slice(repos, func(i, j int) bool {
		return repos[i].accessTime.Before(repos[j].accessTime)
	})

	var totalSize int64
	for _, r := range repos {
		totalSize += r.sizeBytes
	}

	count := len(repos)
	evicted := 0

	for i := 0; i < len(repos); i++ {
		overCount := m.maxCount > 0 && count > m.maxCount
		overSize := m.maxSizeBytes > 0 && totalSize > m.maxSizeBytes
		if !overCount && !overSize {
			break
		}

		r := repos[i]
		mu := m.getRepoLock(r.path)
		if !mu.TryLock() {
			continue
		}
		validatedPath, err := m.validateRepoPath(r.path)
		if err != nil {
			mu.Unlock()
			m.logger.Warn("refusing to evict path outside cache", "path", r.path, "error", err)
			continue
		}

		m.logger.Info("evicting cached repo",
			"path", validatedPath,
			"last_access", r.accessTime.Format(time.RFC3339),
			"size_mb", r.sizeBytes/(1024*1024),
			"reason_count", overCount,
			"reason_size", overSize,
		)
		if err := os.RemoveAll(validatedPath); err != nil {
			mu.Unlock()
			m.logger.Warn("failed to evict repo", "path", validatedPath, "error", err)
			continue
		}
		mu.Unlock()
		totalSize -= r.sizeBytes
		count--
		evicted++
	}

	if evicted > 0 {
		m.logger.Info("cache eviction complete",
			"evicted", evicted,
			"remaining", count,
			"total_size_mb", totalSize/(1024*1024),
		)
	}
}

// buildAutoFixPatch synthesizes a combined patch from eligible suggestions.
//
// Algorithm:
//  1. Commit the current index as a snapshot (so we have a baseline)
//  2. For each suggestion, try applying. If it works, keep it; otherwise skip.
//  3. Generate a combined diff from snapshot to current state.
//  4. Reset to snapshot to restore clean state for branch cleanup.
func (m *Manager) buildAutoFixPatch(ctx context.Context, repoPath string, suggestions []AutoFixSuggestion) (AutoFixResult, error) {
	mu := m.getRepoLock(repoPath)
	mu.Lock()
	defer mu.Unlock()

	// 1. Commit snapshot: the reviewed patch is applied but possibly only staged.
	if err := m.commitSnapshot(ctx, repoPath); err != nil {
		return AutoFixResult{}, fmt.Errorf("autofix snapshot commit: %w", err)
	}

	// 2. Try applying each suggestion sequentially.
	var appliedCount int
	appliedFiles := map[string]struct{}{}

	for _, s := range suggestions {
		patch, err := normalizeSuggestedPatch(s.FilePath, s.SuggestedDiff)
		if err != nil {
			m.logger.Debug("autofix: skipping malformed suggestion",
				"file", s.FilePath, "error", err)
			continue
		}

		// Check first without mutating
		if err := m.checkPatchApplies(ctx, repoPath, patch); err != nil {
			m.logger.Debug("autofix: suggestion does not apply cleanly",
				"file", s.FilePath, "error", err)
			continue
		}

		// Apply for real
		if err := m.applyPatchContent(ctx, repoPath, patch); err != nil {
			m.logger.Warn("autofix: apply failed after check passed",
				"file", s.FilePath, "error", err)
			// Reset to last known good state and continue
			if _, resetErr := m.runGit(ctx, repoPath, "checkout", "--", "."); resetErr != nil {
				return AutoFixResult{}, fmt.Errorf("autofix: reset after failed apply: %w", resetErr)
			}
			continue
		}

		appliedCount++
		appliedFiles[s.FilePath] = struct{}{}
	}

	var result AutoFixResult
	if appliedCount == 0 {
		// Reset to snapshot
		if _, err := m.runGit(ctx, repoPath, "reset", "--hard", "HEAD"); err != nil {
			return AutoFixResult{}, fmt.Errorf("autofix: reset after no applies: %w", err)
		}
		return result, nil
	}

	// 3. Stage all changes and generate combined diff.
	if _, err := m.runGit(ctx, repoPath, "add", "-A"); err != nil {
		return AutoFixResult{}, fmt.Errorf("autofix: stage changes: %w", err)
	}

	diff, err := m.runGit(ctx, repoPath, "diff", "--cached", "HEAD")
	if err != nil {
		return AutoFixResult{}, fmt.Errorf("autofix: generate diff: %w", err)
	}

	// 4. Reset to snapshot so cleanup works normally.
	if _, err := m.runGit(ctx, repoPath, "reset", "--hard", "HEAD"); err != nil {
		return AutoFixResult{}, fmt.Errorf("autofix: final reset: %w", err)
	}

	files := make([]string, 0, len(appliedFiles))
	for f := range appliedFiles {
		files = append(files, f)
	}
	sort.Strings(files)

	result.PatchDiff = diff
	result.AppliedCount = appliedCount
	result.AppliedFiles = files
	return result, nil
}

// commitSnapshot commits the current index state as a temporary snapshot.
// This creates a baseline for diffing auto-fix changes.
func (m *Manager) commitSnapshot(ctx context.Context, repoPath string) error {
	// Stage everything first
	if _, err := m.runGit(ctx, repoPath, "add", "-A"); err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	// Check if there's anything to commit
	status, _ := m.runGit(ctx, repoPath, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		// Nothing to commit — working tree is already clean
		return nil
	}
	if _, err := m.runGit(ctx, repoPath,
		"-c", "user.name=drydock",
		"-c", "user.email=drydock@autofix.local",
		"commit", "-m", "drydock: autofix snapshot", "--no-verify"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// checkPatchApplies tests if a patch applies cleanly without modifying the tree.
func (m *Manager) checkPatchApplies(ctx context.Context, repoPath, patch string) error {
	validatedPath, err := m.validateRepoPath(repoPath)
	if err != nil {
		return err
	}

	applyCtx, cancel := context.WithTimeout(ctx, gitApplyTimeout)
	defer cancel()
	cmd := exec.CommandContext(applyCtx, "git", "-C", validatedPath, "apply", "--check", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	go func() {
		defer stdin.Close()
		io.WriteString(stdin, patch)
	}()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// applyPatchContent applies a patch to the working tree.
func (m *Manager) applyPatchContent(ctx context.Context, repoPath, patch string) error {
	validatedPath, err := m.validateRepoPath(repoPath)
	if err != nil {
		return err
	}

	applyCtx, cancel := context.WithTimeout(ctx, gitApplyTimeout)
	defer cancel()
	cmd := exec.CommandContext(applyCtx, "git", "-C", validatedPath, "apply", "--index", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	go func() {
		defer stdin.Close()
		io.WriteString(stdin, patch)
	}()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// normalizeSuggestedPatch ensures a LLM-generated diff hunk has proper
// unified diff headers so git apply can process it.
func normalizeSuggestedPatch(filePath, suggestedDiff string) (string, error) {
	diff := strings.TrimSpace(suggestedDiff)
	if diff == "" {
		return "", fmt.Errorf("empty suggested diff")
	}

	// If it already starts with "diff --git", assume it's well-formed.
	if strings.HasPrefix(diff, "diff --git") {
		return diff + "\n", nil
	}

	// If it starts with @@ hunk header, add diff/--- /+++ headers.
	if strings.HasPrefix(diff, "@@") {
		var b strings.Builder
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", filePath, filePath)
		fmt.Fprintf(&b, "--- a/%s\n", filePath)
		fmt.Fprintf(&b, "+++ b/%s\n", filePath)
		b.WriteString(diff)
		b.WriteByte('\n')
		return b.String(), nil
	}

	// If it contains +/- lines (hunk content without header), we can't
	// safely reconstruct line numbers. Return error.
	hasHunkContent := false
	for _, line := range strings.Split(diff, "\n") {
		if len(line) > 0 && (line[0] == '+' || line[0] == '-') &&
			!strings.HasPrefix(line, "+++") && !strings.HasPrefix(line, "---") {
			hasHunkContent = true
			break
		}
	}
	if !hasHunkContent {
		return "", fmt.Errorf("does not resemble a unified diff")
	}

	// Has hunk content but no @@ header — can't safely apply.
	return "", fmt.Errorf("missing hunk header (@@)")
}

// isSafeCloneURL validates that a clone URL uses a safe protocol.
// Blocks dangerous git URL schemes that could execute arbitrary commands
// (e.g. ext::, file://, or URLs with embedded credentials/commands).
func isSafeCloneURL(raw string) bool {
	u := strings.TrimSpace(raw)
	if u == "" {
		return false
	}
	lower := strings.ToLower(u)
	// Allow only https:// and git:// protocols
	if strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "git://") {
		return true
	}
	// Allow SSH-style URLs (git@host:path) but only with simple structure
	if strings.Contains(u, "@") && strings.Contains(u, ":") && !strings.Contains(u, "://") {
		// Reject if it contains spaces, semicolons, or backticks (injection attempts)
		for _, c := range u {
			if c == ' ' || c == ';' || c == '`' || c == '$' || c == '|' || c == '&' || c == '\n' || c == '\r' {
				return false
			}
		}
		return true
	}
	return false
}
