// Package docsingest provides a pipeline to ingest project documentation
// from cloned repositories into the Qdrant project_docs collection for
// retrieval-augmented review.
//
// Scans repos for CONTRIBUTING.md, README.md, docs/ folder, and API specs,
// chunks markdown by ## headings, embeds each chunk, and upserts with
// metadata (repo_id, file path, section title).
// Re-ingestion is idempotent via content-hash deduplication.
package docsingest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"drydock/internal/embedding"
	"drydock/internal/vectorstore"
)

// Chunk represents a single section extracted from a project documentation file.
type Chunk struct {
	RepoID       string // unique repository identifier
	FilePath     string // relative path in repo (e.g. "CONTRIBUTING.md")
	SectionTitle string // e.g. "Code Style", "Pull Requests"
	Content      string // full text of the section
	ContentHash  string // sha256 hex of content for dedup
}

// Ingester orchestrates project documentation ingestion into Qdrant.
type Ingester struct {
	qdrant   *vectorstore.Client
	embedder *embedding.Client
	logger   *slog.Logger
}

// Config holds ingestion configuration.
type Config struct {
	// RepoPath is the path to the cloned repository.
	RepoPath string
	// RepoID is the unique repository identifier (used in payload and chunk IDs).
	RepoID string
	// WorkspaceRoots are optional monorepo workspace roots (relative to RepoPath).
	WorkspaceRoots []string
	// VectorDim is the embedding vector dimensionality (default: 768).
	VectorDim int
}

// NewIngester creates a project documentation ingester.
func NewIngester(qdrant *vectorstore.Client, embedder *embedding.Client, logger *slog.Logger) *Ingester {
	if logger == nil {
		logger = slog.Default()
	}
	return &Ingester{
		qdrant:   qdrant,
		embedder: embedder,
		logger:   logger,
	}
}

// maxFileSize is the per-file read limit to avoid embedding huge files.
const maxFileSize = 32 * 1024 // 32 KiB

// docCandidates are well-known documentation file paths to scan in a repo root.
var docCandidates = []string{
	"CONTRIBUTING.md",
	"README.md",
	"CODE_OF_CONDUCT.md",
	"STYLEGUIDE.md",
	"ARCHITECTURE.md",
	"docs/CONTRIBUTING.md",
	"docs/style-guide.md",
	"docs/STYLEGUIDE.md",
	"docs/architecture.md",
	"docs/ARCHITECTURE.md",
}

// docExtensions are file extensions treated as documentation.
var docExtensions = map[string]bool{
	".md":       true,
	".markdown": true,
	".rst":      true,
	".txt":      true,
	".yaml":     true,
	".yml":      true,
	".json":     true,
}

// Run ingests documentation from the given repository.
// Returns the number of chunks upserted and any error.
func (ing *Ingester) Run(ctx context.Context, cfg Config) (int, error) {
	if cfg.RepoPath == "" {
		return 0, fmt.Errorf("docsingest: RepoPath is required")
	}
	if cfg.RepoID == "" {
		return 0, fmt.Errorf("docsingest: RepoID is required")
	}
	if cfg.VectorDim <= 0 {
		cfg.VectorDim = 768
	}

	// Ensure collection exists.
	if err := ing.qdrant.EnsureCollection(ctx, vectorstore.CollectionProjectDocs, cfg.VectorDim); err != nil {
		return 0, fmt.Errorf("ensure project_docs collection: %w", err)
	}

	// Discover documentation files.
	files := ing.discoverDocFiles(cfg.RepoPath, cfg.WorkspaceRoots)
	if len(files) == 0 {
		ing.logger.Info("no documentation files found", "repo_id", cfg.RepoID, "path", cfg.RepoPath)
		return 0, nil
	}

	ing.logger.Info("found documentation files", "count", len(files), "repo_id", cfg.RepoID)

	// Fetch existing content hashes for this repo.
	existingHashes, err := ing.fetchExistingHashes(ctx, cfg.RepoID)
	if err != nil {
		ing.logger.Warn("could not fetch existing hashes, will re-ingest all", "error", err)
		existingHashes = make(map[string]string)
	}

	var totalUpserted int

	for _, relPath := range files {
		select {
		case <-ctx.Done():
			return totalUpserted, ctx.Err()
		default:
		}

		absPath := filepath.Join(cfg.RepoPath, relPath)
		data, err := os.ReadFile(absPath)
		if err != nil {
			ing.logger.Warn("skip file", "path", relPath, "error", err)
			continue
		}
		if len(data) > maxFileSize {
			data = data[:maxFileSize]
		}
		if !isProbablyText(data) {
			continue
		}

		chunks := ChunkDocument(cfg.RepoID, relPath, string(data))
		if len(chunks) == 0 {
			continue
		}

		// Filter to only changed chunks.
		var toUpsert []Chunk
		for _, c := range chunks {
			id := chunkID(c)
			if existing, ok := existingHashes[id]; ok && existing == c.ContentHash {
				continue
			}
			toUpsert = append(toUpsert, c)
		}

		if len(toUpsert) == 0 {
			ing.logger.Debug("no changes", "file", relPath)
			continue
		}

		// Embed and upsert.
		points := make([]vectorstore.Point, 0, len(toUpsert))
		for _, c := range toUpsert {
			vec, err := ing.embedder.Embed(ctx, c.Content)
			if err != nil {
				ing.logger.Warn("embed failed, skip chunk",
					"file", c.FilePath, "section", c.SectionTitle, "error", err)
				continue
			}

			points = append(points, vectorstore.Point{
				ID:     chunkID(c),
				Vector: vec,
				Payload: map[string]any{
					"repo_id":       c.RepoID,
					"file_path":     c.FilePath,
					"section_title": c.SectionTitle,
					"content":       c.Content,
					"content_hash":  c.ContentHash,
				},
			})
		}

		if len(points) > 0 {
			if err := ing.qdrant.Upsert(ctx, vectorstore.CollectionProjectDocs, points); err != nil {
				ing.logger.Warn("upsert failed", "file", relPath, "error", err)
				continue
			}
			totalUpserted += len(points)
			ing.logger.Info("ingested doc chunks", "file", relPath, "chunks", len(points))
		}
	}

	ing.logger.Info("project docs ingestion complete",
		"repo_id", cfg.RepoID, "total_upserted", totalUpserted)
	return totalUpserted, nil
}

// ChunkDocument splits a documentation file into sections.
// Markdown files are chunked by ## headings; other files become a single chunk.
// Exported for testing.
func ChunkDocument(repoID, filePath, content string) []Chunk {
	ext := strings.ToLower(filepath.Ext(filePath))
	if ext == ".md" || ext == ".markdown" || ext == ".rst" {
		return chunkMarkdown(repoID, filePath, content)
	}
	// Non-markdown files (YAML specs, JSON schemas, etc.) → single chunk.
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	return []Chunk{{
		RepoID:       repoID,
		FilePath:     filePath,
		SectionTitle: filepath.Base(filePath),
		Content:      content,
		ContentHash:  contentHash(content),
	}}
}

// chunkMarkdown splits markdown content into sections by ## headings.
func chunkMarkdown(repoID, filePath, content string) []Chunk {
	lines := strings.Split(content, "\n")
	var chunks []Chunk
	var current *Chunk

	for _, line := range lines {
		if title, ok := isHeading(line); ok {
			if current != nil && strings.TrimSpace(current.Content) != "" {
				current.Content = strings.TrimSpace(current.Content)
				current.ContentHash = contentHash(current.Content)
				chunks = append(chunks, *current)
			}
			current = &Chunk{
				RepoID:       repoID,
				FilePath:     filePath,
				SectionTitle: title,
			}
			continue
		}

		if current != nil {
			current.Content += line + "\n"
		} else {
			// Content before first heading → "Introduction" chunk.
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			current = &Chunk{
				RepoID:       repoID,
				FilePath:     filePath,
				SectionTitle: "Introduction",
				Content:      line + "\n",
			}
		}
	}

	if current != nil && strings.TrimSpace(current.Content) != "" {
		current.Content = strings.TrimSpace(current.Content)
		current.ContentHash = contentHash(current.Content)
		chunks = append(chunks, *current)
	}

	return chunks
}

// discoverDocFiles returns relative paths to documentation files in the repo.
func (ing *Ingester) discoverDocFiles(repoPath string, workspaceRoots []string) []string {
	seen := make(map[string]bool)
	var files []string

	// Check well-known candidates.
	for _, rel := range docCandidates {
		abs := filepath.Join(repoPath, rel)
		if info, err := os.Stat(abs); err == nil && !info.IsDir() {
			if !seen[rel] {
				seen[rel] = true
				files = append(files, rel)
			}
		}
	}

	// Check workspace-root candidates for monorepos.
	for _, root := range workspaceRoots {
		for _, name := range []string{"README.md", "CONTRIBUTING.md"} {
			rel := filepath.Join(root, name)
			abs := filepath.Join(repoPath, rel)
			if info, err := os.Stat(abs); err == nil && !info.IsDir() {
				if !seen[rel] {
					seen[rel] = true
					files = append(files, rel)
				}
			}
		}
	}

	// Scan docs/ directory recursively for additional documentation.
	docsDir := filepath.Join(repoPath, "docs")
	if info, err := os.Stat(docsDir); err == nil && info.IsDir() {
		filepath.WalkDir(docsDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			ext := strings.ToLower(filepath.Ext(d.Name()))
			if !docExtensions[ext] {
				return nil
			}
			rel, err := filepath.Rel(repoPath, path)
			if err != nil {
				return nil
			}
			if !seen[rel] {
				seen[rel] = true
				files = append(files, rel)
			}
			return nil
		})
	}

	// Scan for API specs at repo root.
	for _, name := range []string{
		"openapi.yaml", "openapi.yml", "openapi.json",
		"swagger.yaml", "swagger.yml", "swagger.json",
		"schema.graphql", "schema.gql",
	} {
		abs := filepath.Join(repoPath, name)
		if info, err := os.Stat(abs); err == nil && !info.IsDir() {
			if !seen[name] {
				seen[name] = true
				files = append(files, name)
			}
		}
	}

	return files
}

// fetchExistingHashes retrieves point IDs and content_hash for this repo from Qdrant.
func (ing *Ingester) fetchExistingHashes(ctx context.Context, repoID string) (map[string]string, error) {
	hashes := make(map[string]string)
	var offset *string

	filter := map[string]any{
		"must": []map[string]any{
			{"key": "repo_id", "match": map[string]any{"value": repoID}},
		},
	}

	for {
		points, next, err := ing.qdrant.Scroll(ctx, vectorstore.CollectionProjectDocs, 100, offset, filter)
		if err != nil {
			return hashes, err
		}
		for _, p := range points {
			if h, ok := p.Payload["content_hash"].(string); ok {
				hashes[p.ID] = h
			}
		}
		if next == nil {
			break
		}
		offset = next
	}
	return hashes, nil
}

// isHeading returns the heading title if the line is a ## heading, else false.
func isHeading(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "## ") {
		return strings.TrimSpace(trimmed[3:]), true
	}
	return "", false
}

// chunkID generates a stable deterministic ID for a chunk.
func chunkID(c Chunk) string {
	h := sha256.Sum256([]byte(c.RepoID + "::" + c.FilePath + "::" + c.SectionTitle))
	return fmt.Sprintf("%x", h[:16])
}

// contentHash returns the SHA-256 hex digest of content.
func contentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h)
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
