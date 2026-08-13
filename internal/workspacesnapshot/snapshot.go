package workspacesnapshot

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	Path string      `json:"path"`
	Hash string      `json:"hash"`
	Size int64       `json:"size"`
	Mode fs.FileMode `json:"mode"`
}

type Descriptor struct {
	Version      int             `json:"version"`
	SnapshotID   string          `json:"snapshot_id"`
	Kind         Kind            `json:"kind"`
	RepoRoot     string          `json:"repo_root,omitempty"`
	RefName      string          `json:"ref_name,omitempty"`
	Commit       string          `json:"commit,omitempty"`
	PatchRef     string          `json:"patch_ref,omitempty"`
	PatchHash    string          `json:"patch_hash"`
	ManifestHash string          `json:"manifest_hash"`
	Allowlist    []string        `json:"allowlist"`
	Entries      []ManifestEntry `json:"entries"`
	CreatedAt    time.Time       `json:"created_at"`
	ExpiresAt    time.Time       `json:"expires_at"`
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

	manager     *Manager
	repoRoot    string
	filesRoot   string
	refName     string
	storagePath string
	patch       []byte
	entries     map[string]ManifestEntry

	immutableKind         Kind
	immutableCommit       string
	immutablePatchRef     string
	immutablePatchHash    string
	immutableManifestHash string
	immutableAllowlist    []string
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
	snapshotRoot := filepath.Join(m.storageRoot, id)
	if err := os.MkdirAll(snapshotRoot, 0o700); err != nil {
		return nil, fmt.Errorf("workspace snapshot: create descriptor root: %w", err)
	}
	refName := "refs/drydock/snapshots/" + id
	if _, err := runGit(ctx, repoRoot, "update-ref", refName, commit); err != nil {
		return nil, fmt.Errorf("workspace snapshot: create lease ref: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = runGit(context.Background(), repoRoot, "update-ref", "-d", refName)
			_ = os.RemoveAll(snapshotRoot)
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
		ExpiresAt: now.Add(ttl), manager: m, repoRoot: repoRoot, refName: refName, storagePath: snapshotRoot,
		patch: append([]byte(nil), opts.Patch...), entries: entries,
		immutableKind: KindPinnedGit, immutableCommit: commit,
		immutablePatchRef:  strings.TrimSpace(opts.PatchRef),
		immutablePatchHash: hashBytes(opts.Patch),
		immutableAllowlist: append([]string(nil), allowlist...),
	}
	s.ManifestHash = manifestHash(s.Kind, commit, s.PatchRef, s.PatchHash, allowlist, entries)
	s.immutableManifestHash = s.ManifestHash
	if err := persistDescriptor(snapshotRoot, s); err != nil {
		return nil, err
	}
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
		ExpiresAt: now.Add(ttl), manager: m, filesRoot: filesRoot, storagePath: snapshotRoot,
		patch: append([]byte(nil), opts.Patch...), entries: entries,
		immutableKind: KindMutableCopy, immutablePatchRef: strings.TrimSpace(opts.PatchRef),
		immutablePatchHash: hashBytes(opts.Patch),
		immutableAllowlist: append([]string(nil), allowlist...),
	}
	s.ManifestHash = manifestHash(s.Kind, "", s.PatchRef, s.PatchHash, allowlist, entries)
	s.immutableManifestHash = s.ManifestHash
	if err := writeManifest(snapshotRoot, s); err != nil {
		return nil, err
	}
	if err := persistDescriptor(snapshotRoot, s); err != nil {
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

func (m *Manager) Restore(ctx context.Context, storagePath, expectedID, expectedManifestHash, expectedPatchHash string) (*Snapshot, error) {
	root, err := filepath.Abs(storagePath)
	if err != nil || !insideRoot(m.storageRoot, root) || root == m.storageRoot {
		return nil, ErrOutsideScope
	}
	m.mu.RLock()
	existing := m.snapshots[expectedID]
	m.mu.RUnlock()
	if existing != nil {
		if existing.storagePath != root || existing.ManifestDigest() != expectedManifestHash || existing.PatchDigest() != expectedPatchHash {
			return nil, ErrHashMismatch
		}
		return existing, nil
	}
	descriptorData, err := os.ReadFile(filepath.Join(root, "descriptor.json"))
	if err != nil {
		return nil, fmt.Errorf("workspace snapshot: read descriptor: %w", err)
	}
	var descriptor Descriptor
	decoder := json.NewDecoder(bytes.NewReader(descriptorData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&descriptor); err != nil {
		return nil, fmt.Errorf("workspace snapshot: decode descriptor: %w", err)
	}
	if descriptor.Version != 1 || descriptor.SnapshotID != expectedID ||
		descriptor.ManifestHash != expectedManifestHash || descriptor.PatchHash != expectedPatchHash {
		return nil, ErrHashMismatch
	}
	allowlist, err := normalizeAllowlist(descriptor.Allowlist)
	if err != nil {
		return nil, err
	}
	entries := make(map[string]ManifestEntry, len(descriptor.Entries))
	for _, entry := range descriptor.Entries {
		path, err := normalizePath(entry.Path, false)
		if err != nil || path != entry.Path || entry.Hash == "" {
			return nil, ErrHashMismatch
		}
		if _, duplicate := entries[path]; duplicate {
			return nil, ErrHashMismatch
		}
		entries[path] = entry
	}
	patch, err := os.ReadFile(filepath.Join(root, "patch"))
	if err != nil || hashBytes(patch) != descriptor.PatchHash {
		return nil, ErrHashMismatch
	}
	if manifestHash(descriptor.Kind, descriptor.Commit, descriptor.PatchRef, descriptor.PatchHash, allowlist, entries) != descriptor.ManifestHash {
		return nil, ErrHashMismatch
	}
	snapshot := &Snapshot{
		ID: descriptor.SnapshotID, Kind: descriptor.Kind, Commit: descriptor.Commit,
		PatchRef: descriptor.PatchRef, PatchHash: descriptor.PatchHash, ManifestHash: descriptor.ManifestHash,
		Allowlist: allowlist, CreatedAt: descriptor.CreatedAt.UTC(), ExpiresAt: descriptor.ExpiresAt.UTC(),
		manager: m, repoRoot: descriptor.RepoRoot, refName: descriptor.RefName,
		storagePath: root, patch: patch, entries: entries,
		immutableKind: descriptor.Kind, immutableCommit: descriptor.Commit,
		immutablePatchRef: descriptor.PatchRef, immutablePatchHash: descriptor.PatchHash,
		immutableManifestHash: descriptor.ManifestHash, immutableAllowlist: append([]string(nil), allowlist...),
	}
	switch descriptor.Kind {
	case KindPinnedGit:
		if descriptor.RepoRoot == "" || descriptor.RefName == "" {
			return nil, ErrHashMismatch
		}
		resolved, err := runGit(ctx, descriptor.RepoRoot, "rev-parse", "--verify", descriptor.RefName+"^{commit}")
		if err != nil || strings.TrimSpace(resolved) != descriptor.Commit {
			return nil, ErrHashMismatch
		}
	case KindMutableCopy:
		snapshot.filesRoot = filepath.Join(root, "files")
	default:
		return nil, ErrHashMismatch
	}
	if err := snapshot.Verify(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if prior := m.snapshots[expectedID]; prior != nil {
		m.mu.Unlock()
		if prior.ManifestDigest() != expectedManifestHash || prior.PatchDigest() != expectedPatchHash {
			return nil, ErrHashMismatch
		}
		return prior, nil
	}
	m.snapshots[expectedID] = snapshot
	m.mu.Unlock()
	return snapshot, nil
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
		switch s.immutableKind {
		case KindPinnedGit:
			if _, err := runGit(ctx, s.repoRoot, "update-ref", "-d", s.refName); err != nil {
				return removed, fmt.Errorf("workspace snapshot: delete lease ref: %w", err)
			}
			if err := os.RemoveAll(s.storagePath); err != nil {
				return removed, fmt.Errorf("workspace snapshot: remove descriptor: %w", err)
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
	if !pathAllowed(normalized, s.immutableAllowlist) {
		return nil, ErrOutsideScope
	}
	entry, ok := s.entries[normalized]
	if !ok {
		return nil, ErrNotFound
	}
	switch s.immutableKind {
	case KindPinnedGit:
		out, err := runGitBytes(ctx, s.repoRoot, "show", s.immutableCommit+":"+normalized)
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
		return nil, fmt.Errorf("workspace snapshot: unsupported kind %q", s.immutableKind)
	}
}

func (s *Snapshot) Resolve(path string) (string, error) {
	if s.immutableKind != KindMutableCopy {
		return "", ErrNotMaterialized
	}
	normalized, err := normalizePath(path, false)
	if err != nil {
		return "", err
	}
	if !pathAllowed(normalized, s.immutableAllowlist) {
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
	if normalized != "." && !pathAllowed(normalized, s.immutableAllowlist) && !allowlistBelow(normalized, s.immutableAllowlist) {
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

func (s *Snapshot) StoragePath() string { return s.storagePath }

func (s *Snapshot) PatchDigest() string    { return s.immutablePatchHash }
func (s *Snapshot) ManifestDigest() string { return s.immutableManifestHash }
func (s *Snapshot) SnapshotKind() Kind     { return s.immutableKind }
func (s *Snapshot) PinnedCommit() string   { return s.immutableCommit }
func (s *Snapshot) AllowedPaths() []string {
	return append([]string(nil), s.immutableAllowlist...)
}

// Materialize writes verified snapshot files into an empty destination. It is
// used for deterministic analysis that requires a filesystem root and never
// reads from the live workspace.
func (s *Snapshot) Materialize(ctx context.Context, destination string) error {
	if strings.TrimSpace(destination) == "" {
		return fmt.Errorf("workspace snapshot: materialization destination is required")
	}
	root, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("workspace snapshot: materialization destination: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("workspace snapshot: inspect materialization destination: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace snapshot: materialization destination is not a directory")
	}
	existing, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(existing) != 0 {
		return fmt.Errorf("workspace snapshot: materialization destination must be empty")
	}
	paths := make([]string, 0, len(s.entries))
	for path := range s.entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		content, err := s.ReadFile(ctx, path)
		if err != nil {
			return err
		}
		entry := s.entries[path]
		target := filepath.Join(root, filepath.FromSlash(path))
		if !insideRoot(root, target) {
			return ErrOutsideScope
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, entry.Mode.Perm()&0o700); err != nil {
			return err
		}
	}
	return nil
}

// GitReadRequest is the closed, read-only action set exposed to agent tools.
type GitReadRequest struct {
	Action    string
	Path      string
	StartLine int
	EndLine   int
	Limit     int
}

// GitRead executes a read-only operation against the pinned commit. Mutable
// copies intentionally have no live-repository fallback.
func (s *Snapshot) GitRead(ctx context.Context, req GitReadRequest) ([]byte, error) {
	if s.immutableKind != KindPinnedGit {
		return nil, fmt.Errorf("workspace snapshot: git.read requires a pinned git snapshot")
	}
	path := ""
	if strings.TrimSpace(req.Path) != "" {
		var err error
		path, err = normalizePath(req.Path, false)
		if err != nil {
			return nil, err
		}
		if !pathAllowed(path, s.immutableAllowlist) {
			return nil, ErrOutsideScope
		}
	}
	switch req.Action {
	case "diff":
		if path != "" {
			return nil, fmt.Errorf("workspace snapshot: authoritative diff does not support path filtering")
		}
		return append([]byte(nil), s.patch...), nil
	case "show":
		if path != "" {
			return s.ReadFile(ctx, path)
		}
		return runGitBytes(ctx, s.repoRoot, "show", "--no-ext-diff", "--no-textconv", "--stat", "--oneline", s.immutableCommit)
	case "log":
		limit := req.Limit
		if limit <= 0 {
			limit = 20
		}
		if limit > 100 {
			limit = 100
		}
		args := []string{"log", "-n", strconv.Itoa(limit), "--format=%H%x09%an%x09%aI%x09%s", s.immutableCommit, "--"}
		if path != "" {
			args = append(args, path)
		} else {
			for _, allowed := range s.immutableAllowlist {
				if allowed != "." {
					args = append(args, allowed)
				}
			}
		}
		return runGitBytes(ctx, s.repoRoot, args...)
	case "blame":
		if path == "" {
			return nil, fmt.Errorf("workspace snapshot: blame path is required")
		}
		args := []string{"blame", "--porcelain"}
		if req.StartLine > 0 {
			end := req.EndLine
			if end < req.StartLine {
				end = req.StartLine
			}
			args = append(args, "-L", fmt.Sprintf("%d,%d", req.StartLine, end))
		}
		args = append(args, s.immutableCommit, "--", path)
		return runGitBytes(ctx, s.repoRoot, args...)
	default:
		return nil, fmt.Errorf("workspace snapshot: unsupported git action %q", req.Action)
	}
}

func (s *Snapshot) Verify() error {
	if hashBytes(s.patch) != s.immutablePatchHash {
		return ErrHashMismatch
	}
	if manifestHash(s.immutableKind, s.immutableCommit, s.immutablePatchRef, s.immutablePatchHash, s.immutableAllowlist, s.entries) != s.immutableManifestHash {
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
	var collapsed []string
	for _, candidate := range out {
		if pathAllowed(candidate, collapsed) {
			continue
		}
		collapsed = append(collapsed, candidate)
	}
	return collapsed, nil
}

func normalizePath(path string, allowDot bool) (string, error) {
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	windowsAbsolute := len(path) >= 3 &&
		((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')) &&
		path[1] == ':' && path[2] == '/'
	if path == "" || strings.ContainsRune(path, 0) || windowsAbsolute || filepath.IsAbs(path) || filepath.VolumeName(path) != "" {
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
		entry, err := copyFile(path, dst, rel, info)
		if err != nil {
			return err
		}
		entries[rel] = entry
		return nil
	})
}

func copyFile(source, target, rel string, expected fs.FileInfo) (ManifestEntry, error) {
	if expected == nil || !expected.Mode().IsRegular() {
		return ManifestEntry{}, fmt.Errorf("workspace snapshot: invalid source file %s", rel)
	}
	in, err := os.Open(source)
	if err != nil {
		return ManifestEntry{}, err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil {
		return ManifestEntry{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return ManifestEntry{}, fmt.Errorf("%w: source changed while copying %s", ErrSymlink, rel)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return ManifestEntry{}, err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, expected.Mode().Perm()&0o700)
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
	return ManifestEntry{Path: rel, Hash: hex.EncodeToString(h.Sum(nil)), Size: size, Mode: expected.Mode().Perm()}, nil
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

func persistDescriptor(root string, s *Snapshot) error {
	paths := make([]string, 0, len(s.entries))
	for path := range s.entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	entries := make([]ManifestEntry, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, s.entries[path])
	}
	descriptor := Descriptor{
		Version: 1, SnapshotID: s.ID, Kind: s.immutableKind, RepoRoot: s.repoRoot,
		RefName: s.refName, Commit: s.immutableCommit, PatchRef: s.immutablePatchRef,
		PatchHash: s.immutablePatchHash, ManifestHash: s.immutableManifestHash,
		Allowlist: append([]string(nil), s.immutableAllowlist...), Entries: entries,
		CreatedAt: s.CreatedAt.UTC(), ExpiresAt: s.ExpiresAt.UTC(),
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		return fmt.Errorf("workspace snapshot: encode descriptor: %w", err)
	}
	if err := atomicWrite(filepath.Join(root, "patch"), s.patch); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(root, "descriptor.json"), encoded); err != nil {
		return err
	}
	return nil
}

func atomicWrite(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return nil
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
