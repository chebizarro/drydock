package workspacesnapshot

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Kind string

const (
	KindPinnedGit   Kind = "pinned_git"
	KindMutableCopy Kind = "mutable_copy"
)

var (
	ErrInvalidPath     = errors.New("workspace snapshot: invalid path")
	ErrOutsideScope    = errors.New("workspace snapshot: path outside snapshot scope")
	ErrSymlink         = errors.New("workspace snapshot: symlinks are not allowed")
	ErrHashMismatch    = errors.New("workspace snapshot: artifact hash mismatch")
	ErrNotFound        = errors.New("workspace snapshot: not found")
	ErrExpired         = errors.New("workspace snapshot: expired")
	ErrNotMaterialized = errors.New("workspace snapshot: pinned git snapshots are not materialized")
)

type Clock func() time.Time

type Config struct {
	StorageRoot     string
	SnapshotTTL     time.Duration
	LeaseTTL        time.Duration
	SessionLifetime time.Duration
	Clock           Clock
}

type Manager struct {
	mu              sync.RWMutex
	storageRoot     string
	snapshotTTL     time.Duration
	leaseTTL        time.Duration
	sessionLifetime time.Duration
	now             Clock
	snapshots       map[string]*Snapshot
	leases          map[string]Lease
}

type ManifestEntry struct {
	Path string
	Hash string
	Size int64
	Mode fs.FileMode
}

type Snapshot struct {
	ID           string
	Kind         Kind
	Commit       string
	PatchRef     string
	PatchHash    string
	ManifestHash string
	Allowlist    []string
	CreatedAt    time.Time
	ExpiresAt    time.Time

	manager   *Manager
	repoRoot  string
	filesRoot string
	refName   string
	patch     []byte
	entries   map[string]ManifestEntry
}

type PinnedGitOptions struct {
	RepoPath  string
	Ref       string
	PatchRef  string
	Patch     []byte
	Allowlist []string
	TTL       time.Duration
}

type MutableCopyOptions struct {
	WorkspacePath string
	PatchRef      string
	Patch         []byte
	Allowlist     []string
	TTL           time.Duration
}

type Lease struct {
	ID         string
	SnapshotID string
	SessionID  string
	ExpiresAt  time.Time
}

func NewManager(cfg Config) (*Manager, error) {
	if strings.TrimSpace(cfg.StorageRoot) == "" {
		return nil, fmt.Errorf("workspace snapshot: storage root is required")
	}
	root, err := filepath.Abs(cfg.StorageRoot)
	if err != nil {
		return nil, fmt.Errorf("workspace snapshot: storage root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("workspace snapshot: create storage root: %w", err)
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.SessionLifetime <= 0 {
		cfg.SessionLifetime = 24 * time.Hour
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = cfg.SessionLifetime
	}
	if cfg.LeaseTTL < cfg.SessionLifetime {
		return nil, fmt.Errorf("workspace snapshot: lease TTL %s is shorter than session lifetime %s", cfg.LeaseTTL, cfg.SessionLifetime)
	}
	if cfg.SnapshotTTL <= 0 {
		cfg.SnapshotTTL = cfg.LeaseTTL
	}
	return &Manager{
		storageRoot: root, snapshotTTL: cfg.SnapshotTTL, leaseTTL: cfg.LeaseTTL,
		sessionLifetime: cfg.SessionLifetime, now: cfg.Clock,
		snapshots: make(map[string]*Snapshot), leases: make(map[string]Lease),
	}, nil
}

func (m *Manager) CreatePinned(ctx context.Context, opts PinnedGitOptions) (_ *Snapshot, err error) {
	if strings.TrimSpace(opts.RepoPath) == "" {
		return nil, fmt.Errorf("workspace snapshot: repository path is required")
	}
	repoRoot, err := filepath.Abs(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("workspace snapshot: repository path: %w", err)
	}
	repoRoot, err = filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("workspace snapshot: resolve repository: %w", err)
	}
	allowlist, err := normalizeAllowlist(opts.Allowlist)
	if err != nil {
		return nil, err
	}
	ref := strings.TrimSpace(opts.Ref)
	if ref == "" {
		return nil, fmt.Errorf("workspace snapshot: git ref is required")
	}
	commit, err := runGit(ctx, repoRoot, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("workspace snapshot: pin ref: %w", err)
	}
	commit = strings.TrimSpace(commit)
	entries, err := gitManifest(ctx, repoRoot, commit, allowlist)
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	refName := "refs/drydock/snapshots/" + id
	if _, err := runGit(ctx, repoRoot, "update-ref", refName, commit); err != nil {
		return nil, fmt.Errorf("workspace snapshot: create lease ref: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = runGit(context.Background(), repoRoot, "update-ref", "-d", refName)
		}
	}()

	now := m.now().UTC()
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = m.snapshotTTL
	}
	s := &Snapshot{
		ID: id, Kind: KindPinnedGit, Commit: commit, PatchRef: strings.TrimSpace(opts.PatchRef),
		PatchHash: hashBytes(opts.Patch), Allowlist: allowlist, CreatedAt: now,
		ExpiresAt: now.Add(ttl), manager: m, repoRoot: repoRoot, refName: refName,
		patch: append([]byte(nil), opts.Patch...), entries: entries,
	}
	s.ManifestHash = manifestHash(s.Kind, commit, s.PatchRef, s.PatchHash, allowlist, entries)
	m.mu.Lock()
	m.snapshots[id] = s
	m.mu.Unlock()
	committed = true
	return s, nil
}

func (m *Manager) CreateMutable(ctx context.Context, opts MutableCopyOptions) (_ *Snapshot, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.WorkspacePath) == "" {
		return nil, fmt.Errorf("workspace snapshot: workspace path is required")
	}
	source, err := filepath.Abs(opts.WorkspacePath)
	if err != nil {
		return nil, fmt.Errorf("workspace snapshot: workspace path: %w", err)
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return nil, fmt.Errorf("workspace snapshot: resolve workspace: %w", err)
	}
	info, err := os.Stat(source)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("workspace snapshot: workspace is not a directory")
	}
	allowlist, err := normalizeAllowlist(opts.Allowlist)
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	snapshotRoot := filepath.Join(m.storageRoot, id)
	filesRoot := filepath.Join(snapshotRoot, "files")
	if err := os.MkdirAll(filesRoot, 0o700); err != nil {
		return nil, fmt.Errorf("workspace snapshot: create copy root: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(snapshotRoot)
		}
	}()

	entries := make(map[string]ManifestEntry)
	for _, allowed := range allowlist {
		if err := copyAllowed(ctx, source, filesRoot, allowed, entries); err != nil {
			return nil, err
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("workspace snapshot: allowlist matched no files")
	}
	now := m.now().UTC()
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = m.snapshotTTL
	}
	s := &Snapshot{
		ID: id, Kind: KindMutableCopy, PatchRef: strings.TrimSpace(opts.PatchRef),
		PatchHash: hashBytes(opts.Patch), Allowlist: allowlist, CreatedAt: now,
		ExpiresAt: now.Add(ttl), manager: m, filesRoot: filesRoot,
		patch: append([]byte(nil), opts.Patch...), entries: entries,
	}
	s.ManifestHash = manifestHash(s.Kind, "", s.PatchRef, s.PatchHash, allowlist, entries)
	if err := writeManifest(snapshotRoot, s); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.snapshots[id] = s
	m.mu.Unlock()
	committed = true
	return s, nil
}

func (m *Manager) Get(id string) (*Snapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.snapshots[id]
	if !ok {
		return nil, ErrNotFound
	}
	if !m.now().Before(s.ExpiresAt) && !m.hasActiveLeaseLocked(id, m.now()) {
		return nil, ErrExpired
	}
	return s, nil
}

func (m *Manager) Acquire(snapshotID, sessionID string, ttl time.Duration) (Lease, error) {
	if strings.TrimSpace(sessionID) == "" {
		return Lease{}, fmt.Errorf("workspace snapshot: session ID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.snapshots[snapshotID]
	if !ok {
		return Lease{}, ErrNotFound
	}
	now := m.now().UTC()
	if !now.Before(s.ExpiresAt) && !m.hasActiveLeaseLocked(snapshotID, now) {
		return Lease{}, ErrExpired
	}
	if ttl <= 0 {
		ttl = m.leaseTTL
	}
	if ttl < m.sessionLifetime {
		ttl = m.sessionLifetime
	}
	id, err := newID()
	if err != nil {
		return Lease{}, err
	}
	lease := Lease{ID: id, SnapshotID: snapshotID, SessionID: sessionID, ExpiresAt: now.Add(ttl)}
	m.leases[id] = lease
	if s.ExpiresAt.Before(lease.ExpiresAt) {
		s.ExpiresAt = lease.ExpiresAt
	}
	return lease, nil
}

func (m *Manager) Renew(leaseID string, ttl time.Duration) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, ok := m.leases[leaseID]
	if !ok {
		return Lease{}, ErrNotFound
	}
	if ttl <= 0 {
		ttl = m.leaseTTL
	}
	if ttl < m.sessionLifetime {
		ttl = m.sessionLifetime
	}
	lease.ExpiresAt = m.now().UTC().Add(ttl)
	m.leases[leaseID] = lease
	if s := m.snapshots[lease.SnapshotID]; s != nil && s.ExpiresAt.Before(lease.ExpiresAt) {
		s.ExpiresAt = lease.ExpiresAt
	}
	return lease, nil
}

func (m *Manager) Release(leaseID string) {
	m.mu.Lock()
	delete(m.leases, leaseID)
	m.mu.Unlock()
}

func (m *Manager) GC(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	for id, lease := range m.leases {
		if !now.Before(lease.ExpiresAt) {
			delete(m.leases, id)
		}
	}
	var removed []string
	for id, s := range m.snapshots {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if now.Before(s.ExpiresAt) || m.hasActiveLeaseLocked(id, now) {
			continue
		}
		switch s.Kind {
		case KindPinnedGit:
			if _, err := runGit(ctx, s.repoRoot, "update-ref", "-d", s.refName); err != nil {
				return removed, fmt.Errorf("workspace snapshot: delete lease ref: %w", err)
			}
		case KindMutableCopy:
			if err := os.RemoveAll(filepath.Dir(s.filesRoot)); err != nil {
				return removed, fmt.Errorf("workspace snapshot: remove copy: %w", err)
			}
		}
		delete(m.snapshots, id)
		removed = append(removed, id)
	}
	sort.Strings(removed)
	return removed, nil
}

func (m *Manager) hasActiveLeaseLocked(snapshotID string, now time.Time) bool {
	for _, lease := range m.leases {
		if lease.SnapshotID == snapshotID && now.Before(lease.ExpiresAt) {
			return true
		}
	}
	return false
}

func (s *Snapshot) ReadFile(ctx context.Context, path string) ([]byte, error) {
	normalized, err := normalizePath(path, false)
	if err != nil {
		return nil, err
	}
	if !pathAllowed(normalized, s.Allowlist) {
		return nil, ErrOutsideScope
	}
	entry, ok := s.entries[normalized]
	if !ok {
		return nil, ErrNotFound
	}
	switch s.Kind {
	case KindPinnedGit:
		out, err := runGitBytes(ctx, s.repoRoot, "show", s.Commit+":"+normalized)
		if err != nil {
			return nil, fmt.Errorf("workspace snapshot: read pinned file: %w", err)
		}
		if hashBytes(out) != entry.Hash {
			return nil, ErrHashMismatch
		}
		return out, nil
	case KindMutableCopy:
		resolved, err := s.Resolve(path)
		if err != nil {
			return nil, err
		}
		out, err := os.ReadFile(resolved)
		if err != nil {
			return nil, fmt.Errorf("workspace snapshot: read copied file: %w", err)
		}
		if hashBytes(out) != entry.Hash {
			return nil, ErrHashMismatch
		}
		return out, nil
	default:
		return nil, fmt.Errorf("workspace snapshot: unsupported kind %q", s.Kind)
	}
}

func (s *Snapshot) Resolve(path string) (string, error) {
	if s.Kind != KindMutableCopy {
		return "", ErrNotMaterialized
	}
	normalized, err := normalizePath(path, false)
	if err != nil {
		return "", err
	}
	if !pathAllowed(normalized, s.Allowlist) {
		return "", ErrOutsideScope
	}
	entry, ok := s.entries[normalized]
	if !ok {
		return "", ErrNotFound
	}
	resolved := filepath.Join(s.filesRoot, filepath.FromSlash(normalized))
	if !insideRoot(s.filesRoot, resolved) {
		return "", ErrOutsideScope
	}
	if err := rejectSymlinkComponents(s.filesRoot, resolved); err != nil {
		return "", err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", err
	}
	if hashBytes(data) != entry.Hash {
		return "", ErrHashMismatch
	}
	return resolved, nil
}

func (s *Snapshot) List(prefix string) ([]ManifestEntry, error) {
	normalized, err := normalizePath(prefix, true)
	if err != nil {
		return nil, err
	}
	if normalized != "." && !pathAllowed(normalized, s.Allowlist) && !allowlistBelow(normalized, s.Allowlist) {
		return nil, ErrOutsideScope
	}
	var entries []ManifestEntry
	for path, entry := range s.entries {
		if normalized == "." || path == normalized || strings.HasPrefix(path, normalized+"/") {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (s *Snapshot) PatchContent() []byte { return append([]byte(nil), s.patch...) }

func (s *Snapshot) Verify() error {
	if hashBytes(s.patch) != s.PatchHash {
		return ErrHashMismatch
	}
	if manifestHash(s.Kind, s.Commit, s.PatchRef, s.PatchHash, s.Allowlist, s.entries) != s.ManifestHash {
		return ErrHashMismatch
	}
	for path := range s.entries {
		if _, err := s.ReadFile(context.Background(), path); err != nil {
			return err
		}
	}
	return nil
}

func normalizeAllowlist(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("workspace snapshot: path allowlist is required")
	}
	seen := make(map[string]struct{}, len(paths))
	var out []string
	for _, path := range paths {
		normalized, err := normalizePath(path, true)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out, nil
}

func normalizePath(path string, allowDot bool) (string, error) {
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	if path == "" || strings.ContainsRune(path, 0) || filepath.IsAbs(path) || filepath.VolumeName(path) != "" {
		return "", ErrInvalidPath
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return "", ErrInvalidPath
		}
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", ErrInvalidPath
	}
	if clean == "." && !allowDot {
		return "", ErrInvalidPath
	}
	return clean, nil
}

func pathAllowed(path string, allowlist []string) bool {
	for _, allowed := range allowlist {
		if allowed == "." || path == allowed || strings.HasPrefix(path, allowed+"/") {
			return true
		}
	}
	return false
}

func allowlistBelow(prefix string, allowlist []string) bool {
	for _, allowed := range allowlist {
		if allowed == "." || strings.HasPrefix(allowed, prefix+"/") {
			return true
		}
	}
	return false
}

func copyAllowed(ctx context.Context, source, target, allowed string, entries map[string]ManifestEntry) error {
	sourcePath := source
	if allowed != "." {
		sourcePath = filepath.Join(source, filepath.FromSlash(allowed))
	}
	if !insideRoot(source, sourcePath) {
		return ErrOutsideScope
	}
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return fmt.Errorf("workspace snapshot: inspect allowed path %q: %w", allowed, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlink, allowed)
	}
	walkRoot := sourcePath
	return filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			if d.IsDir() {
				return nil
			}
			rel = allowed
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlink, rel)
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(target, filepath.FromSlash(rel)), 0o700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("workspace snapshot: unsupported file type %s", rel)
		}
		dst := filepath.Join(target, filepath.FromSlash(rel))
		entry, err := copyFile(path, dst, rel, info.Mode())
		if err != nil {
			return err
		}
		entries[rel] = entry
		return nil
	})
}

func copyFile(source, target, rel string, mode fs.FileMode) (ManifestEntry, error) {
	in, err := os.Open(source)
	if err != nil {
		return ManifestEntry{}, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return ManifestEntry{}, err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm()&0o700)
	if err != nil {
		return ManifestEntry{}, err
	}
	h := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(out, h), in)
	closeErr := out.Close()
	if copyErr != nil {
		return ManifestEntry{}, copyErr
	}
	if closeErr != nil {
		return ManifestEntry{}, closeErr
	}
	return ManifestEntry{Path: rel, Hash: hex.EncodeToString(h.Sum(nil)), Size: size, Mode: mode.Perm()}, nil
}

func gitManifest(ctx context.Context, repoRoot, commit string, allowlist []string) (map[string]ManifestEntry, error) {
	args := []string{"ls-tree", "-r", "-z", "--full-tree", commit, "--"}
	for _, allowed := range allowlist {
		if allowed != "." {
			args = append(args, allowed)
		}
	}
	out, err := runGitBytes(ctx, repoRoot, args...)
	if err != nil {
		return nil, fmt.Errorf("workspace snapshot: read git tree: %w", err)
	}
	entries := make(map[string]ManifestEntry)
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		meta, pathBytes, ok := bytes.Cut(raw, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("workspace snapshot: malformed git tree entry")
		}
		fields := strings.Fields(string(meta))
		if len(fields) != 3 {
			return nil, fmt.Errorf("workspace snapshot: malformed git tree metadata")
		}
		path, err := normalizePath(string(pathBytes), false)
		if err != nil {
			return nil, err
		}
		if !pathAllowed(path, allowlist) {
			continue
		}
		if fields[0] == "120000" {
			return nil, fmt.Errorf("%w: %s", ErrSymlink, path)
		}
		if fields[1] != "blob" {
			continue
		}
		data, err := runGitBytes(ctx, repoRoot, "show", commit+":"+path)
		if err != nil {
			return nil, err
		}
		modeValue, _ := strconv.ParseUint(fields[0], 8, 32)
		entries[path] = ManifestEntry{Path: path, Hash: hashBytes(data), Size: int64(len(data)), Mode: fs.FileMode(modeValue)}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("workspace snapshot: allowlist matched no files")
	}
	return entries, nil
}

func rejectSymlinkComponents(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ErrOutsideScope
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlink, rel)
		}
	}
	return nil
}

func insideRoot(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func manifestHash(kind Kind, commit, patchRef, patchHash string, allowlist []string, entries map[string]ManifestEntry) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", kind, commit, patchRef, patchHash)
	for _, allowed := range allowlist {
		fmt.Fprintf(h, "allow:%s\x00", allowed)
	}
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		entry := entries[path]
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%o\x00", path, entry.Hash, entry.Size, entry.Mode)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeManifest(root string, s *Snapshot) error {
	var b strings.Builder
	fmt.Fprintf(&b, "snapshot=%s\nkind=%s\nmanifest=%s\npatch=%s\n", s.ID, s.Kind, s.ManifestHash, s.PatchHash)
	paths := make([]string, 0, len(s.entries))
	for path := range s.entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		entry := s.entries[path]
		fmt.Fprintf(&b, "%s\t%s\t%d\t%o\n", entry.Path, entry.Hash, entry.Size, entry.Mode)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest"), []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("workspace snapshot: write manifest: %w", err)
	}
	return nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("workspace snapshot: generate ID: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func runGit(ctx context.Context, repo string, args ...string) (string, error) {
	out, err := runGitBytes(ctx, repo, args...)
	return string(out), err
}

func runGitBytes(ctx context.Context, repo string, args ...string) ([]byte, error) {
	full := append([]string{"-C", repo}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	return out, nil
}
