// Package codeindex provides semantic code indexing for retrieval-augmented
// code review. It indexes source code symbols (functions, methods, types) into
// a Qdrant vector collection, enabling the review pipeline to surface
// semantically related code that may be affected by a patch.
//
// Indexing is incremental: only files whose git hash changed since the last
// index are re-processed. State is stored in a repo-local file under .git/.
package codeindex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"drydock/internal/embedding"
	"drydock/internal/symbols"
	"drydock/internal/vectorstore"
)

const (
	stateFileName = "drydock-codeindex-state.json"
	maxFileBytes  = 64 * 1024
	maxChunkBytes = 8 * 1024
	upsertBatch   = 50
	contextLines  = 2 // lines of context before/after each symbol
)

// indexState tracks the last indexed commit per repo.
type indexState struct {
	Commit    string `json:"commit"`
	UpdatedAt int64  `json:"updated_at"`
}

// Indexer manages semantic code indexing into Qdrant.
// It is safe for concurrent use; per-repo serialisation is enforced internally.
type Indexer struct {
	qdrant    *vectorstore.Client
	embedder  *embedding.Client
	logger    *slog.Logger
	vectorDim int

	repoLocks sync.Map // keyed by repoID → *sync.Mutex
}

// New creates a code indexer.
func New(qdrant *vectorstore.Client, embedder *embedding.Client, logger *slog.Logger, vectorDims ...int) *Indexer {
	if logger == nil {
		logger = slog.Default()
	}
	vectorDim := 768
	if len(vectorDims) > 0 && vectorDims[0] > 0 {
		vectorDim = vectorDims[0]
	}
	return &Indexer{
		qdrant:    qdrant,
		embedder:  embedder,
		logger:    logger,
		vectorDim: vectorDim,
	}
}

// IndexRepo indexes (or incrementally re-indexes) source code in the given
// repository into the code_chunks Qdrant collection. It reads from the
// canonical default ref (origin/HEAD or HEAD) rather than the mutable working
// tree, so review-branch checkouts do not pollute the persistent index.
//
// On first call for a repo, all supported source files are indexed.
// Subsequent calls compare against the last indexed commit and only
// re-process files that changed, were renamed, or were deleted.
func (idx *Indexer) IndexRepo(ctx context.Context, repoPath, repoID string) error {
	if repoPath == "" || repoID == "" {
		return fmt.Errorf("codeindex: repoPath and repoID are required")
	}

	// Serialise per-repo to avoid concurrent indexing of the same repo.
	mu := idx.repoMutex(repoID)
	mu.Lock()
	defer mu.Unlock()

	// Ensure collection exists.
	if err := idx.qdrant.EnsureCollection(ctx, vectorstore.CollectionCodeChunks, idx.vectorDim); err != nil {
		return fmt.Errorf("ensure code_chunks collection: %w", err)
	}

	// Resolve canonical ref and current commit.
	currentCommit, err := resolveCanonicalCommit(ctx, repoPath)
	if err != nil {
		return fmt.Errorf("resolve canonical commit: %w", err)
	}

	// Read prior state.
	state := readState(repoPath)

	// Check Qdrant/state divergence: if state says current but collection is
	// empty, force a full rebuild.
	forceRebuild := false
	if state.Commit == currentCommit {
		count, err := idx.countRepoPoints(ctx, repoID)
		if err != nil {
			idx.logger.Warn("could not count indexed points, will re-index",
				"repo_id", repoID, "error", err)
			forceRebuild = true
		} else if count == 0 {
			idx.logger.Info("state file current but no indexed points, forcing rebuild",
				"repo_id", repoID, "commit", currentCommit)
			forceRebuild = true
		} else {
			idx.logger.Debug("code index up to date",
				"repo_id", repoID, "commit", currentCommit, "points", count)
			return nil
		}
	}

	// Create per-call extractor (not shared — tree-sitter parser is mutable).
	extractor := symbols.New()
	defer extractor.Close()

	var totalUpserted int
	var hadErrors bool

	// Determine whether to do a full rebuild or incremental update.
	fullRebuild := state.Commit == "" || forceRebuild

	if !fullRebuild {
		// Try incremental update first.
		idx.logger.Info("incremental code index update",
			"repo_id", repoID,
			"from", state.Commit[:min(8, len(state.Commit))],
			"to", currentCommit[:min(8, len(currentCommit))])

		changes, err := gitDiffNameStatus(ctx, repoPath, state.Commit, currentCommit)
		if err != nil {
			// Fallback to full rebuild if diff fails (e.g. force-pushed history).
			idx.logger.Warn("incremental diff failed, falling back to full rebuild",
				"repo_id", repoID, "error", err)
			fullRebuild = true
		} else {
			for _, change := range changes {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}

				switch {
				case change.status == "D":
					if err := idx.deleteFilePoints(ctx, repoID, change.oldPath); err != nil {
						idx.logger.Warn("failed to delete chunks for removed file",
							"file", change.oldPath, "error", err)
						hadErrors = true
					}

				case strings.HasPrefix(change.status, "R"):
					if err := idx.deleteFilePoints(ctx, repoID, change.oldPath); err != nil {
						idx.logger.Warn("failed to delete chunks for renamed file",
							"old_path", change.oldPath, "error", err)
						hadErrors = true
					}
					n, err := idx.indexFile(ctx, extractor, repoPath, repoID, currentCommit, change.newPath)
					if err != nil {
						idx.logger.Warn("skip renamed file", "file", change.newPath, "error", err)
						hadErrors = true
					} else {
						totalUpserted += n
					}

				case change.status == "A" || change.status == "M" ||
					change.status == "T" || change.status == "C":
					if change.status != "A" {
						if err := idx.deleteFilePoints(ctx, repoID, change.effectivePath()); err != nil {
							idx.logger.Warn("failed to delete old chunks",
								"file", change.effectivePath(), "error", err)
							hadErrors = true
						}
					}
					n, err := idx.indexFile(ctx, extractor, repoPath, repoID, currentCommit, change.effectivePath())
					if err != nil {
						idx.logger.Warn("skip file", "file", change.effectivePath(), "error", err)
						hadErrors = true
					} else {
						totalUpserted += n
					}

				default:
					idx.logger.Warn("unknown git diff status, skipping",
						"status", change.status, "file", change.effectivePath())
				}
			}
		}
	}

	if fullRebuild {
		idx.logger.Info("full code index build",
			"repo_id", repoID, "commit", currentCommit)

		// Delete any stale points for this repo.
		if err := idx.deleteRepoPoints(ctx, repoID); err != nil {
			idx.logger.Warn("failed to clean stale points", "repo_id", repoID, "error", err)
		}

		files, err := gitListFiles(ctx, repoPath, currentCommit)
		if err != nil {
			return fmt.Errorf("git ls-tree: %w", err)
		}

		for _, filePath := range files {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			n, err := idx.indexFile(ctx, extractor, repoPath, repoID, currentCommit, filePath)
			if err != nil {
				idx.logger.Warn("skip file during index",
					"file", filePath, "error", err)
				hadErrors = true
				continue
			}
			totalUpserted += n
		}
	}

	// Only persist state if indexing completed without errors.
	// Partial indexes leave the state unchanged so the next run retries.
	if hadErrors {
		idx.logger.Warn("code index completed with errors, state not advanced",
			"repo_id", repoID, "chunks_upserted", totalUpserted)
	} else {
		if err := writeState(repoPath, indexState{
			Commit:    currentCommit,
			UpdatedAt: time.Now().Unix(),
		}); err != nil {
			idx.logger.Warn("failed to write index state", "repo_id", repoID, "error", err)
		}
	}

	idx.logger.Info("code index complete",
		"repo_id", repoID, "commit", currentCommit[:min(8, len(currentCommit))],
		"chunks_upserted", totalUpserted)
	return nil
}

// indexFile reads a single file from the canonical ref, extracts symbols,
// embeds each chunk, and upserts to Qdrant. Returns the number of points
// upserted. Returns a non-nil error only for file-level issues that the
// caller should log and skip.
func (idx *Indexer) indexFile(
	ctx context.Context,
	extractor *symbols.Extractor,
	repoPath, repoID, commit, filePath string,
) (int, error) {
	// Check language support.
	ext := filepath.Ext(filePath)
	lang := symbols.LangFromExt(ext)
	if lang == "" {
		return 0, nil // unsupported language, silently skip
	}

	// Read file from canonical ref.
	source, err := gitShowFile(ctx, repoPath, commit, filePath)
	if err != nil {
		return 0, fmt.Errorf("git show %s: %w", filePath, err)
	}

	// Skip large or binary files.
	if len(source) > maxFileBytes {
		return 0, nil
	}
	if !isProbablyText(source) {
		return 0, nil
	}

	// Extract symbols.
	syms, err := extractor.Extract(lang, source)
	if err != nil {
		return 0, fmt.Errorf("extract symbols from %s: %w", filePath, err)
	}
	if len(syms) == 0 {
		return 0, nil
	}

	// Build chunks from symbols.
	lines := strings.Split(string(source), "\n")
	var points []vectorstore.Point
	upserted := 0

	for _, sym := range syms {
		chunk := extractChunk(lines, sym)
		if len(chunk) == 0 {
			continue
		}

		content := string(chunk)
		if len(content) > maxChunkBytes {
			content = content[:maxChunkBytes]
		}

		vec, err := idx.embedder.Embed(ctx, content)
		if err != nil {
			idx.logger.Warn("embed failed, skip chunk",
				"file", filePath, "symbol", sym.Name, "error", err)
			continue
		}

		points = append(points, vectorstore.Point{
			ID:     chunkPointID(repoID, filePath, sym),
			Vector: vec,
			Payload: map[string]any{
				"repo_id":        repoID,
				"file_path":      filePath,
				"symbol_name":    sym.Name,
				"symbol_kind":    string(sym.Kind),
				"parent_symbol":  sym.Parent,
				"start_line":     int(sym.StartLine) + 1, // 1-based
				"end_line":       int(sym.EndLine) + 1,   // 1-based
				"language":       lang,
				"content":        content,
				"content_hash":   contentHash(content),
				"indexed_commit": commit,
			},
		})

		// Batch upsert when buffer is full.
		if len(points) >= upsertBatch {
			if err := idx.qdrant.Upsert(ctx, vectorstore.CollectionCodeChunks, points); err != nil {
				return upserted, fmt.Errorf("upsert batch: %w", err)
			}
			upserted += len(points)
			points = points[:0]
		}
	}

	// Flush remaining points.
	if len(points) > 0 {
		if err := idx.qdrant.Upsert(ctx, vectorstore.CollectionCodeChunks, points); err != nil {
			return upserted, fmt.Errorf("upsert remaining: %w", err)
		}
		upserted += len(points)
	}

	return upserted, nil
}

// extractChunk slices the source lines for a symbol with a small context window.
func extractChunk(lines []string, sym symbols.Symbol) []byte {
	start := int(sym.StartLine)
	end := int(sym.EndLine)

	// Add context lines (bounded).
	ctxStart := start - contextLines
	if ctxStart < 0 {
		ctxStart = 0
	}
	ctxEnd := end + contextLines
	if ctxEnd >= len(lines) {
		ctxEnd = len(lines) - 1
	}

	if ctxStart > ctxEnd || ctxStart >= len(lines) {
		return nil
	}

	return []byte(strings.Join(lines[ctxStart:ctxEnd+1], "\n"))
}

// chunkPointID generates a stable deterministic ID for a code chunk.
func chunkPointID(repoID, filePath string, sym symbols.Symbol) string {
	key := fmt.Sprintf("%s::%s::%s::%s::%d::%d",
		repoID, filePath, sym.Name, sym.Kind, sym.StartLine, sym.EndLine)
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h[:16])
}

// contentHash returns the SHA-256 hex digest of content.
func contentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h)
}

// repoMutex returns a per-repo mutex, creating one if needed.
func (idx *Indexer) repoMutex(repoID string) *sync.Mutex {
	v, _ := idx.repoLocks.LoadOrStore(repoID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// countRepoPoints returns the number of code chunks for a repo in Qdrant.
func (idx *Indexer) countRepoPoints(ctx context.Context, repoID string) (int64, error) {
	return idx.qdrant.Count(ctx, vectorstore.CollectionCodeChunks, map[string]any{
		"must": []map[string]any{
			{"key": "repo_id", "match": map[string]any{"value": repoID}},
		},
	})
}

// deleteRepoPoints removes all code chunks for a repo from Qdrant.
func (idx *Indexer) deleteRepoPoints(ctx context.Context, repoID string) error {
	return idx.scrollAndDelete(ctx, map[string]any{
		"must": []map[string]any{
			{"key": "repo_id", "match": map[string]any{"value": repoID}},
		},
	})
}

// deleteFilePoints removes all code chunks for a specific file in a repo.
func (idx *Indexer) deleteFilePoints(ctx context.Context, repoID, filePath string) error {
	return idx.scrollAndDelete(ctx, map[string]any{
		"must": []map[string]any{
			{"key": "repo_id", "match": map[string]any{"value": repoID}},
			{"key": "file_path", "match": map[string]any{"value": filePath}},
		},
	})
}

// scrollAndDelete scrolls matching points and deletes them in batches.
func (idx *Indexer) scrollAndDelete(ctx context.Context, filter map[string]any) error {
	var offset *string
	for {
		points, next, err := idx.qdrant.Scroll(ctx, vectorstore.CollectionCodeChunks, 100, offset, filter)
		if err != nil {
			return err
		}
		if len(points) == 0 {
			return nil
		}
		ids := make([]string, len(points))
		for i, p := range points {
			ids[i] = p.ID
		}
		if err := idx.qdrant.Delete(ctx, vectorstore.CollectionCodeChunks, ids); err != nil {
			return err
		}
		if next == nil {
			return nil
		}
		offset = next
	}
}

// --- Git helpers ---

// resolveCanonicalCommit finds the canonical commit to index.
// Prefers origin/HEAD, falls back to HEAD.
func resolveCanonicalCommit(ctx context.Context, repoPath string) (string, error) {
	// Try origin/HEAD first.
	commit, err := gitRevParse(ctx, repoPath, "refs/remotes/origin/HEAD")
	if err == nil && commit != "" {
		return commit, nil
	}
	// Fallback to HEAD.
	commit, err = gitRevParse(ctx, repoPath, "HEAD")
	if err != nil {
		return "", fmt.Errorf("could not resolve HEAD: %w", err)
	}
	if commit == "" {
		return "", fmt.Errorf("empty HEAD commit")
	}
	return commit, nil
}

func gitRevParse(ctx context.Context, repoPath, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", ref)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitListFiles returns all tracked file paths at the given commit.
func gitListFiles(ctx context.Context, repoPath, commit string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "ls-tree", "-r", "--name-only", commit)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, nil
	}
	return strings.Split(raw, "\n"), nil
}

// fileChange represents a single entry from git diff --name-status.
type fileChange struct {
	status  string // A, M, D, T, C, or R<score>
	oldPath string // for renames/copies, the source path
	newPath string // for renames/copies, the dest path; otherwise same as oldPath
}

func (fc fileChange) effectivePath() string {
	if fc.newPath != "" {
		return fc.newPath
	}
	return fc.oldPath
}

// gitDiffNameStatus returns changed files between two commits.
func gitDiffNameStatus(ctx context.Context, repoPath, from, to string) ([]fileChange, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath,
		"diff", "--name-status", "--find-renames", from, to)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, nil
	}

	var changes []fileChange
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		status := parts[0]
		fc := fileChange{status: status, oldPath: parts[1]}
		if len(parts) >= 3 {
			// Rename or copy: old\tnew
			fc.newPath = parts[2]
		}
		changes = append(changes, fc)
	}
	return changes, nil
}

// gitShowFile reads a file's contents from a specific commit.
func gitShowFile(ctx context.Context, repoPath, commit, filePath string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "show", commit+":"+filePath)
	return cmd.Output()
}

// --- State file helpers ---

func stateFilePath(repoPath string) string {
	return filepath.Join(repoPath, ".git", stateFileName)
}

func readState(repoPath string) indexState {
	data, err := os.ReadFile(stateFilePath(repoPath))
	if err != nil {
		return indexState{}
	}
	var s indexState
	if err := json.Unmarshal(data, &s); err != nil {
		return indexState{}
	}
	return s
}

func writeState(repoPath string, s indexState) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(stateFilePath(repoPath), data, 0o644)
}

// isProbablyText returns true if data looks like text (no NUL bytes in first 512 bytes).
func isProbablyText(data []byte) bool {
	check := data
	if len(check) > 512 {
		check = check[:512]
	}
	for _, b := range check {
		if b == 0 {
			return false
		}
	}
	return true
}
