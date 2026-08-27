// Package repository opens local or managed Git roots and exposes immutable
// snapshots for deterministic tree and search operations.
package repository

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/contextbuilder"
	internalrepo "git.sharegap.net/cascadia/drydock/internal/repo"
	"git.sharegap.net/cascadia/drydock/internal/workspacesnapshot"
)

var (
	// ErrInvalidRoot reports a malformed or inaccessible root specification.
	ErrInvalidRoot = errors.New("drydock repository: invalid root")
	// ErrNotFound reports a path or snapshot that does not exist.
	ErrNotFound = errors.New("drydock repository: not found")
	// ErrOutsideScope reports a path outside the snapshot allowlist.
	ErrOutsideScope = errors.New("drydock repository: path outside snapshot scope")
	// ErrUnsafePath reports an invalid path or disallowed symlink.
	ErrUnsafePath = errors.New("drydock repository: unsafe path")
	// ErrSnapshotExpired reports an expired immutable snapshot.
	ErrSnapshotExpired = errors.New("drydock repository: snapshot expired")
	// ErrSnapshotChanged reports failed immutable digest verification.
	ErrSnapshotChanged = errors.New("drydock repository: snapshot changed")
)

// RootKind identifies how a root is sourced.
type RootKind string

const (
	// RootLocal snapshots an allowlisted local directory as an immutable copy.
	RootLocal RootKind = "local"
	// RootManagedGit snapshots a managed checkout at a pinned Git commit.
	RootManagedGit RootKind = "managed_git"
)

// ManagedGitSpec configures a checkout owned by drydock's repository cache.
type ManagedGitSpec struct {
	// RepositoryID is the stable logical repository identity used for cache placement.
	RepositoryID string
	// CloneURLs are tried in order after unsafe URL schemes are rejected.
	CloneURLs []string
	// Ref is the Git ref pinned by Snapshot; empty means HEAD.
	Ref string
	// CacheDir stores managed repository clones.
	CacheDir string
	// MaxRepoCount limits cached repositories; zero is unlimited.
	MaxRepoCount int
	// MaxCacheSizeMB limits cache size in MiB; zero is unlimited.
	MaxCacheSizeMB int
}

// RootSpec describes one local or managed Git root.
// Exactly one of LocalPath and ManagedGit must be set.
type RootSpec struct {
	// ID is the stable caller-owned root identity.
	ID string
	// LocalPath selects an existing local directory.
	LocalPath string
	// ManagedGit selects a drydock-managed Git checkout.
	ManagedGit *ManagedGitSpec
	// SnapshotStore stores private immutable snapshot data and descriptors.
	SnapshotStore string
	// Allowlist contains root-relative files or directories; empty means the whole root.
	Allowlist []string
	// SnapshotTTL controls snapshot expiry; zero uses the internal default.
	SnapshotTTL time.Duration
	// LeaseTTL controls snapshot leases; zero uses the internal default.
	LeaseTTL time.Duration
	// SessionLifetime is the minimum lease duration; zero uses 24 hours.
	SessionLifetime time.Duration
	// Logger receives managed-checkout diagnostics; nil uses slog.Default.
	Logger *slog.Logger
}

// Root is an opaque opened root. Its filesystem/cache location is not part of
// the public contract.
type Root struct {
	id        string
	kind      RootKind
	path      string
	ref       string
	allowlist []string
	snapshots *workspacesnapshot.Manager
}

// RootDescriptor describes an opened root without exposing private cache paths.
type RootDescriptor struct {
	// ID is the caller-owned root identity.
	ID string
	// Kind identifies local-copy or managed-Git semantics.
	Kind RootKind
	// Ref is the configured managed Git ref, or empty for local roots.
	Ref string
}

// OpenRoot validates and opens a local root or ensures a managed Git checkout.
func OpenRoot(ctx context.Context, spec RootSpec) (*Root, error) {
	id := strings.TrimSpace(spec.ID)
	if id == "" {
		return nil, fmt.Errorf("%w: ID is required", ErrInvalidRoot)
	}
	hasLocal := strings.TrimSpace(spec.LocalPath) != ""
	hasManaged := spec.ManagedGit != nil
	if hasLocal == hasManaged {
		return nil, fmt.Errorf("%w: exactly one of LocalPath and ManagedGit is required", ErrInvalidRoot)
	}
	if strings.TrimSpace(spec.SnapshotStore) == "" {
		return nil, fmt.Errorf("%w: SnapshotStore is required", ErrInvalidRoot)
	}
	snapshotManager, err := workspacesnapshot.NewManager(workspacesnapshot.Config{
		StorageRoot: spec.SnapshotStore, SnapshotTTL: spec.SnapshotTTL,
		LeaseTTL: spec.LeaseTTL, SessionLifetime: spec.SessionLifetime,
	})
	if err != nil {
		return nil, err
	}
	allowlist := append([]string(nil), spec.Allowlist...)
	if len(allowlist) == 0 {
		allowlist = []string{"."}
	}

	if hasLocal {
		path, err := canonicalDirectory(spec.LocalPath)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidRoot, err)
		}
		return &Root{
			id: id, kind: RootLocal, path: path,
			allowlist: allowlist, snapshots: snapshotManager,
		}, nil
	}

	managed := spec.ManagedGit
	if strings.TrimSpace(managed.RepositoryID) == "" {
		return nil, fmt.Errorf("%w: managed RepositoryID is required", ErrInvalidRoot)
	}
	if strings.TrimSpace(managed.CacheDir) == "" {
		return nil, fmt.Errorf("%w: managed CacheDir is required", ErrInvalidRoot)
	}
	logger := spec.Logger
	if logger == nil {
		logger = slog.Default()
	}
	options := []internalrepo.ManagerOption{
		internalrepo.WithMaxRepoCount(managed.MaxRepoCount),
		internalrepo.WithMaxCacheSizeMB(managed.MaxCacheSizeMB),
	}
	manager := internalrepo.NewManager(managed.CacheDir, logger, options...)
	path, err := manager.EnsureRepo(ctx, managed.RepositoryID, append([]string(nil), managed.CloneURLs...))
	if err != nil {
		return nil, err
	}
	ref := strings.TrimSpace(managed.Ref)
	if ref == "" {
		ref = "HEAD"
	}
	return &Root{
		id: id, kind: RootManagedGit, path: path, ref: ref,
		allowlist: allowlist, snapshots: snapshotManager,
	}, nil
}

// Descriptor returns an owned description of the opened root.
func (r *Root) Descriptor() RootDescriptor {
	if r == nil {
		return RootDescriptor{}
	}
	return RootDescriptor{ID: r.id, Kind: r.kind, Ref: r.ref}
}

// SnapshotOptions configures one immutable root snapshot.
type SnapshotOptions struct {
	// Allowlist overrides the root allowlist when non-empty.
	Allowlist []string
	// TTL overrides the root snapshot TTL when positive.
	TTL time.Duration
}

// Snapshot freezes the opened root.
// Local roots are copied; managed Git roots are pinned without mutating the checkout.
func (r *Root) Snapshot(ctx context.Context, opts SnapshotOptions) (*Snapshot, error) {
	if r == nil || r.snapshots == nil {
		return nil, fmt.Errorf("%w: root is not initialized", ErrInvalidRoot)
	}
	allowlist := append([]string(nil), opts.Allowlist...)
	if len(allowlist) == 0 {
		allowlist = append([]string(nil), r.allowlist...)
	}
	var inner *workspacesnapshot.Snapshot
	var err error
	switch r.kind {
	case RootLocal:
		inner, err = r.snapshots.CreateMutable(ctx, workspacesnapshot.MutableCopyOptions{
			WorkspacePath: r.path, Allowlist: allowlist, TTL: opts.TTL,
		})
	case RootManagedGit:
		inner, err = r.snapshots.CreatePinned(ctx, workspacesnapshot.PinnedGitOptions{
			RepoPath: r.path, Ref: r.ref, Allowlist: allowlist, TTL: opts.TTL,
		})
	default:
		err = fmt.Errorf("%w: unsupported root kind %q", ErrInvalidRoot, r.kind)
	}
	if err != nil {
		return nil, translateError(err)
	}
	return &Snapshot{rootID: r.id, inner: inner}, nil
}

// Snapshot is an opaque immutable repository view.
type Snapshot struct {
	rootID string
	inner  *workspacesnapshot.Snapshot
}

// SnapshotKind identifies immutable copy or pinned-Git snapshot storage.
type SnapshotKind string

const (
	// SnapshotMutableCopy is an immutable copy of a local root.
	SnapshotMutableCopy SnapshotKind = "mutable_copy"
	// SnapshotPinnedGit is a Git commit pinned by a private ref.
	SnapshotPinnedGit SnapshotKind = "pinned_git"
)

// SnapshotDescriptor describes an immutable snapshot with owned fields.
type SnapshotDescriptor struct {
	// ID is the opaque snapshot identity.
	ID string
	// RootID is the caller-owned root identity.
	RootID string
	// Kind identifies copy or pinned-Git storage.
	Kind SnapshotKind
	// Commit is set for pinned Git snapshots.
	Commit string
	// Digest is the immutable manifest SHA-256.
	Digest string
	// Allowlist is the normalized snapshot scope.
	Allowlist []string
	// CreatedAt is the snapshot creation time.
	CreatedAt time.Time
	// ExpiresAt is the current snapshot expiry.
	ExpiresAt time.Time
}

// Descriptor returns an owned immutable snapshot description.
func (s *Snapshot) Descriptor() SnapshotDescriptor {
	if s == nil || s.inner == nil {
		return SnapshotDescriptor{}
	}
	return SnapshotDescriptor{
		ID: s.inner.ID, RootID: s.rootID, Kind: SnapshotKind(s.inner.SnapshotKind()),
		Commit: s.inner.PinnedCommit(), Digest: s.inner.ManifestDigest(),
		Allowlist: s.inner.AllowedPaths(), CreatedAt: s.inner.CreatedAt, ExpiresAt: s.inner.ExpiresAt,
	}
}

// Digest returns the immutable snapshot manifest SHA-256.
func (s *Snapshot) Digest() string {
	if s == nil || s.inner == nil {
		return ""
	}
	return s.inner.ManifestDigest()
}

// ReadFile reads a verified root-relative file from the immutable snapshot.
func (s *Snapshot) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if s == nil || s.inner == nil {
		return nil, fmt.Errorf("%w: snapshot is not initialized", ErrInvalidRoot)
	}
	content, err := s.inner.ReadFile(ctx, path)
	if err != nil {
		return nil, translateError(err)
	}
	return content, nil
}

// TreeOptions configures a deterministic flattened file tree.
type TreeOptions struct {
	// Path limits results to a root-relative file or directory; empty means ".".
	Path string
	// MaxDepth limits descendants relative to Path; zero means unlimited.
	MaxDepth int
}

// TreeEntry describes one immutable file.
type TreeEntry struct {
	// Path is root-relative and slash-separated.
	Path string
	// SHA256 is the immutable file content digest.
	SHA256 string
	// Size is the file size in bytes.
	Size int64
	// Mode is the captured file mode.
	Mode fs.FileMode
}

// Tree returns sorted immutable file entries.
// Directory nodes are implicit; only files are returned.
func (s *Snapshot) Tree(ctx context.Context, opts TreeOptions) ([]TreeEntry, error) {
	if s == nil || s.inner == nil {
		return nil, fmt.Errorf("%w: snapshot is not initialized", ErrInvalidRoot)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prefix := strings.TrimSpace(opts.Path)
	if prefix == "" {
		prefix = "."
	}
	entries, err := s.inner.List(prefix)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]TreeEntry, 0, len(entries))
	for _, entry := range entries {
		if opts.MaxDepth > 0 {
			relative := entry.Path
			if prefix != "." {
				relative = strings.TrimPrefix(strings.TrimPrefix(entry.Path, prefix), "/")
			}
			if strings.Count(relative, "/") > opts.MaxDepth {
				continue
			}
		}
		out = append(out, TreeEntry{
			Path: entry.Path, SHA256: entry.Hash, Size: entry.Size, Mode: entry.Mode,
		})
	}
	return out, nil
}

// SearchOptions configures deterministic snapshot content search.
type SearchOptions struct {
	// Query is a literal string unless Regex is true.
	Query string
	// Path limits search to a root-relative path; empty means ".".
	Path string
	// Regex interprets Query as a Go regular expression.
	Regex bool
	// TestsOnly restricts matches to recognized test paths.
	TestsOnly bool
	// MaxResults defaults to 100 and is capped at 1,000.
	MaxResults int
}

// SearchHit is one line-oriented immutable content match.
type SearchHit struct {
	// Path is the matching root-relative file.
	Path string
	// Line is the one-based line number.
	Line int
	// Text is the complete matching line.
	Text string
}

// Search returns deterministic path-then-line ordered matches.
func (s *Snapshot) Search(ctx context.Context, opts SearchOptions) ([]SearchHit, error) {
	if s == nil || s.inner == nil {
		return nil, fmt.Errorf("%w: snapshot is not initialized", ErrInvalidRoot)
	}
	hits, err := contextbuilder.NewContentSearchFacade().Search(ctx, snapshotSource{s.inner}, contextbuilder.ContentSearchRequest{
		Query: opts.Query, Path: opts.Path, Regex: opts.Regex,
		TestsOnly: opts.TestsOnly, MaxResults: opts.MaxResults,
	})
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]SearchHit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, SearchHit{Path: hit.Path, Line: hit.Line, Text: hit.Text})
	}
	return out, nil
}

type snapshotSource struct {
	snapshot *workspacesnapshot.Snapshot
}

func (s snapshotSource) ListPaths(_ context.Context, prefix string) ([]string, error) {
	entries, err := s.snapshot.List(prefix)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	return paths, nil
}

func (s snapshotSource) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return s.snapshot.ReadFile(ctx, path)
}

func translateError(err error) error {
	switch {
	case errors.Is(err, workspacesnapshot.ErrNotFound):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, workspacesnapshot.ErrOutsideScope):
		return fmt.Errorf("%w: %v", ErrOutsideScope, err)
	case errors.Is(err, workspacesnapshot.ErrInvalidPath),
		errors.Is(err, workspacesnapshot.ErrSymlink):
		return fmt.Errorf("%w: %v", ErrUnsafePath, err)
	case errors.Is(err, workspacesnapshot.ErrExpired):
		return fmt.Errorf("%w: %v", ErrSnapshotExpired, err)
	case errors.Is(err, workspacesnapshot.ErrHashMismatch):
		return fmt.Errorf("%w: %v", ErrSnapshotChanged, err)
	default:
		return err
	}
}

func canonicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return resolved, nil
}
