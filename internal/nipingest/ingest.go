// Package nipingest provides a pipeline to ingest NIP markdown files into
// the Qdrant nip_specs collection for retrieval-augmented review.
//
// Chunks NIP specs by ## headings, embeds each chunk, and upserts with
// metadata (nip number, section title, event kinds mentioned).
// Re-ingestion is idempotent via content-hash deduplication.
package nipingest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"drydock/internal/embedding"
	"drydock/internal/vectorstore"
)

// Chunk represents a single section extracted from a NIP markdown file.
type Chunk struct {
	NIPID        string // e.g. "01", "46", "5F"
	SectionTitle string // e.g. "Protocol Overview", "Event Kinds"
	Content      string // full text of the section
	EventKinds   []int  // event kinds mentioned (e.g. 1617, 30617)
	ContentHash  string // sha256 hex of content for dedup
}

// Ingester orchestrates NIP spec ingestion into Qdrant.
type Ingester struct {
	qdrant    *vectorstore.Client
	embedder  *embedding.Client
	logger    *slog.Logger
	vectorDim int
}

// Config holds ingestion configuration.
type Config struct {
	// NIPsDir is the directory containing NIP markdown files.
	NIPsDir string
	// VectorDim is the embedding vector dimensionality (e.g. 768).
	VectorDim int
}

// NewIngester creates a NIP spec ingester.
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

// Run ingests all NIP markdown files from the given directory.
// Returns the number of chunks upserted and any error.
func (ing *Ingester) Run(ctx context.Context, cfg Config) (int, error) {
	if cfg.VectorDim <= 0 {
		cfg.VectorDim = embedding.DefaultDimension
	}

	// Ensure collection exists.
	if err := ing.qdrant.EnsureCollection(ctx, ing.qdrant.CollectionNames().NIPSpecs, cfg.VectorDim); err != nil {
		return 0, fmt.Errorf("ensure nip_specs collection: %w", err)
	}

	// Find NIP markdown files.
	files, err := findNIPFiles(cfg.NIPsDir)
	if err != nil {
		return 0, fmt.Errorf("find NIP files: %w", err)
	}
	if len(files) == 0 {
		ing.logger.Warn("no NIP files found", "dir", cfg.NIPsDir)
		return 0, nil
	}

	ing.logger.Info("found NIP files", "count", len(files), "dir", cfg.NIPsDir)

	// Fetch existing content hashes for dedup.
	existingHashes, err := ing.fetchExistingHashes(ctx)
	if err != nil {
		ing.logger.Warn("could not fetch existing hashes, will re-ingest all", "error", err)
		existingHashes = make(map[string]string)
	}

	var totalIntended int
	var totalUpserted int
	var totalFailed int
	var chunkErrors []error

	for _, file := range files {
		select {
		case <-ctx.Done():
			return totalUpserted, ctx.Err()
		default:
		}

		data, err := os.ReadFile(file)
		if err != nil {
			ing.logger.Warn("skip file", "path", file, "error", err)
			continue
		}

		nipID := extractNIPID(filepath.Base(file))
		chunks := ChunkMarkdown(nipID, string(data))

		if len(chunks) == 0 {
			continue
		}

		// Filter to only changed chunks.
		var toUpsert []Chunk
		for _, c := range chunks {
			id := chunkID(c)
			if existing, ok := existingHashes[id]; ok && existing == c.ContentHash {
				continue // unchanged
			}
			toUpsert = append(toUpsert, c)
		}

		if len(toUpsert) == 0 {
			ing.logger.Debug("no changes", "nip", nipID)
			continue
		}
		totalIntended += len(toUpsert)

		// Embed and upsert.
		points := make([]vectorstore.Point, 0, len(toUpsert))
		for _, c := range toUpsert {
			vec, err := ing.embedder.Embed(ctx, c.Content)
			if err != nil {
				totalFailed++
				chunkErrors = append(chunkErrors, fmt.Errorf("embed NIP-%s section %q: %w", c.NIPID, c.SectionTitle, err))
				ing.logger.Warn("embed failed, skip chunk", "nip", c.NIPID, "section", c.SectionTitle, "error", err)
				continue
			}

			points = append(points, vectorstore.Point{
				ID:     chunkID(c),
				Vector: vec,
				Payload: map[string]any{
					"nip_id":        c.NIPID,
					"section_title": c.SectionTitle,
					"content":       c.Content,
					"event_kinds":   c.EventKinds,
					"content_hash":  c.ContentHash,
				},
			})
		}

		if len(points) > 0 {
			if err := ing.qdrant.Upsert(ctx, ing.qdrant.CollectionNames().NIPSpecs, points); err != nil {
				totalFailed += len(points)
				chunkErrors = append(chunkErrors, fmt.Errorf("upsert NIP-%s (%d chunks): %w", nipID, len(points), err))
				ing.logger.Warn("upsert failed", "nip", nipID, "error", err)
				continue
			}
			totalUpserted += len(points)
			ing.logger.Info("ingested NIP chunks", "nip", nipID, "chunks", len(points))
		}
	}

	ing.logger.Info("NIP ingestion complete",
		"chunks_intended", totalIntended,
		"chunks_upserted", totalUpserted,
		"chunks_failed", totalFailed)
	if totalFailed > 0 {
		return totalUpserted, fmt.Errorf(
			"NIP ingestion incomplete: intended_chunks=%d upserted_chunks=%d failed_chunks=%d: %w",
			totalIntended, totalUpserted, totalFailed, errors.Join(chunkErrors...),
		)
	}
	return totalUpserted, nil
}

// ChunkMarkdown splits NIP markdown content into sections by ## headings.
// Exported for testing.
func ChunkMarkdown(nipID, content string) []Chunk {
	lines := strings.Split(content, "\n")
	var chunks []Chunk
	var current *Chunk

	for _, line := range lines {
		if title, ok := isHeading(line); ok {
			// Flush previous chunk.
			if current != nil && strings.TrimSpace(current.Content) != "" {
				current.Content = strings.TrimSpace(current.Content)
				current.ContentHash = contentHash(current.Content)
				current.EventKinds = extractEventKinds(current.Content)
				chunks = append(chunks, *current)
			}
			current = &Chunk{
				NIPID:        nipID,
				SectionTitle: title,
			}
			continue
		}

		if current != nil {
			current.Content += line + "\n"
		} else {
			// Content before first heading — create an "Introduction" chunk.
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue // skip top-level # heading
			}
			current = &Chunk{
				NIPID:        nipID,
				SectionTitle: "Introduction",
				Content:      line + "\n",
			}
		}
	}

	// Flush final chunk.
	if current != nil && strings.TrimSpace(current.Content) != "" {
		current.Content = strings.TrimSpace(current.Content)
		current.ContentHash = contentHash(current.Content)
		current.EventKinds = extractEventKinds(current.Content)
		chunks = append(chunks, *current)
	}

	return chunks
}

// fetchExistingHashes retrieves all point IDs and their content_hash from Qdrant.
func (ing *Ingester) fetchExistingHashes(ctx context.Context) (map[string]string, error) {
	hashes := make(map[string]string)
	var offset *string

	for {
		points, next, err := ing.qdrant.Scroll(ctx, ing.qdrant.CollectionNames().NIPSpecs, 100, offset, nil)
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

// findNIPFiles returns all .md files in the directory matching NIP naming patterns.
func findNIPFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(strings.ToLower(name), ".md") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	return files, nil
}

// isHeading returns the heading title if the line is a ## heading, else false.
func isHeading(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "## ") {
		return strings.TrimSpace(trimmed[3:]), true
	}
	return "", false
}

// extractNIPID extracts the NIP identifier from a filename.
// e.g. "01.md" → "01", "NIP-46.md" → "46", "5F.md" → "5F"
func extractNIPID(filename string) string {
	name := strings.TrimSuffix(filename, filepath.Ext(filename))
	// Remove common prefixes.
	for _, prefix := range []string{"NIP-", "nip-", "NIP", "nip"} {
		name = strings.TrimPrefix(name, prefix)
	}
	return strings.TrimSpace(name)
}

// chunkID generates a stable deterministic ID for a chunk.
func chunkID(c Chunk) string {
	h := sha256.Sum256([]byte(c.NIPID + "::" + c.SectionTitle))
	return fmt.Sprintf("%x", h[:16]) // 32 hex chars
}

// contentHash returns the SHA-256 hex digest of content.
func contentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h)
}

// eventKindRe matches kind numbers (3-5 digits) that look like Nostr event kinds.
var eventKindRe = regexp.MustCompile(`\bkind[:\s]+(\d{3,5})\b|\b(\d{4,5})\b`)

// knownKindRanges are Nostr event kind ranges worth extracting.
var knownKinds = map[int]bool{
	0: true, 1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true,
	1111: true, 1617: true, 1618: true, 1619: true, 1621: true, 1622: true,
	1630: true, 1631: true, 1632: true, 1633: true, 1985: true,
	5900: true, 6900: true, 7000: true,
	10002: true, 30017: true, 30023: true, 30617: true, 30618: true, 30818: true,
	30819: true,
}

// extractEventKinds finds Nostr event kind numbers mentioned in the text.
func extractEventKinds(text string) []int {
	matches := eventKindRe.FindAllStringSubmatch(text, -1)
	seen := make(map[int]bool)
	var kinds []int

	for _, m := range matches {
		numStr := m[1]
		if numStr == "" {
			numStr = m[2]
		}
		n, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		if knownKinds[n] && !seen[n] {
			seen[n] = true
			kinds = append(kinds, n)
		}
	}
	return kinds
}
