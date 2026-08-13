package contextbuilder

import (
	"bufio"
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"drydock/internal/symbols"
)

// ContentSource is the narrow immutable repository view used by tool-facing
// analysis facades.
type ContentSource interface {
	ListPaths(ctx context.Context, prefix string) ([]string, error)
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

type ContentSearchRequest struct {
	Query      string
	Path       string
	Regex      bool
	TestsOnly  bool
	MaxResults int
}

type ContentSearchHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// ContentSearchFacade searches an immutable ContentSource. Keeping this logic
// here lets deterministic providers and tool handlers share analysis behavior.
type ContentSearchFacade struct{}

func NewContentSearchFacade() ContentSearchFacade { return ContentSearchFacade{} }

func (ContentSearchFacade) Search(ctx context.Context, source ContentSource, req ContentSearchRequest) ([]ContentSearchHit, error) {
	if source == nil {
		return nil, fmt.Errorf("contextbuilder: content source is required")
	}
	if strings.TrimSpace(req.Query) == "" {
		return nil, fmt.Errorf("contextbuilder: search query is required")
	}
	pattern := req.Query
	if !req.Regex {
		pattern = regexp.QuoteMeta(pattern)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("contextbuilder: compile search pattern: %w", err)
	}
	limit := req.MaxResults
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	prefix := strings.TrimSpace(req.Path)
	if prefix == "" {
		prefix = "."
	}
	paths, err := source.ListPaths(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var hits []ContentSearchHit
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if req.TestsOnly && !isTestPath(path) {
			continue
		}
		content, err := source.ReadFile(ctx, path)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(strings.NewReader(string(content)))
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			if re.MatchString(scanner.Text()) {
				hits = append(hits, ContentSearchHit{Path: path, Line: line, Text: scanner.Text()})
				if len(hits) >= limit {
					return hits, nil
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("contextbuilder: scan %s: %w", path, err)
		}
	}
	return hits, nil
}

func isTestPath(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(lower)
	return strings.Contains(lower, "/test/") ||
		strings.Contains(lower, "/tests/") ||
		strings.Contains(base, "_test.") ||
		strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") ||
		strings.HasPrefix(base, "test_")
}

type StructureRequest struct {
	Path    string
	Content []byte
}

type StructureResult struct {
	Path      string           `json:"path"`
	Language  string           `json:"language"`
	Symbols   []symbols.Symbol `json:"symbols"`
	Available bool             `json:"available"`
}

// StructureFacade provides the canonical tree-sitter structure analysis used
// by code.structure.
type StructureFacade struct{}

func NewStructureFacade() StructureFacade { return StructureFacade{} }

func (StructureFacade) Analyze(req StructureRequest) (StructureResult, error) {
	lang := symbols.LangFromExt(strings.ToLower(filepath.Ext(req.Path)))
	if lang == "" {
		return StructureResult{}, fmt.Errorf("contextbuilder: unsupported source extension for %s", req.Path)
	}
	if !symbols.TreeSitterAvailable() {
		return StructureResult{Path: req.Path, Language: lang, Available: false},
			fmt.Errorf("contextbuilder: tree-sitter is unavailable")
	}
	extractor := symbols.New()
	defer extractor.Close()
	found, err := extractor.Extract(lang, req.Content)
	if err != nil {
		return StructureResult{}, err
	}
	return StructureResult{Path: req.Path, Language: lang, Symbols: found, Available: true}, nil
}
