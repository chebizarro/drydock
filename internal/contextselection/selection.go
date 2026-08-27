// Package contextselection implements deterministic, patch-neutral context assembly.
package contextselection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/contextbuilder"
)

const (
	// VariantWholeFile includes an entire file.
	VariantWholeFile = "whole_file"
	// VariantLineSlice includes one or more one-based inclusive line ranges.
	VariantLineSlice = "line_slice"
	// VariantCodemap includes the canonical symbol structure for a file.
	VariantCodemap = "codemap"
)

var (
	// ErrBudgetExceeded reports that exact token accounting exceeded the effective limit.
	ErrBudgetExceeded = errors.New("context selection: token budget exceeded")
	// ErrInvalidSelection reports malformed roots, entries, ranges, or tokenizer metadata.
	ErrInvalidSelection = errors.New("context selection: invalid selection")
	// ErrSourceDigestMismatch reports that selected content changed before assembly.
	ErrSourceDigestMismatch = errors.New("context selection: source digest mismatch")
)

// Source is the immutable file-reading boundary consumed by the assembler.
type Source interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

// Counter performs exact token accounting for assembled content.
type Counter interface {
	Count(text string) int
}

// Root identifies one immutable source. Order is the stable multi-root ordering key.
type Root struct {
	ID     string
	Order  int
	Digest string
	Source Source
}

// LineRange is a one-based inclusive source range.
type LineRange struct {
	StartLine int
	EndLine   int
}

// Entry selects one representation of a file.
type Entry struct {
	RootID         string
	Path           string
	Variant        string
	Ranges         []LineRange
	ExpectedSHA256 string
}

// Exclusion records omitted content and why it was omitted.
type Exclusion struct {
	RootID  string
	Path    string
	Variant string
	Reason  string
}

// TokenizerIdentity records the exact tokenizer used for accounting.
type TokenizerIdentity struct {
	Name     string
	Version  string
	Encoding string
}

// Included records one deterministically rendered selection entry.
type Included struct {
	RootID         string
	Path           string
	Variant        string
	Ranges         []LineRange
	SourceSHA256   string
	RenderedSHA256 string
}

// RootDigest binds a manifest to one immutable input root.
type RootDigest struct {
	RootID string
	Digest string
}

// Manifest is the digest-bound description of one assembly.
type Manifest struct {
	Version        int
	Roots          []RootDigest
	Included       []Included
	TokenCount     int
	TokenBudget    int
	EffectiveLimit int
	Exclusions     []Exclusion
	Tokenizer      TokenizerIdentity
	ContentSHA256  string
	ManifestSHA256 string
}

// Request is a complete deterministic assembly request.
type Request struct {
	Roots       []Root
	Entries     []Entry
	Exclusions  []Exclusion
	Counter     Counter
	Tokenizer   TokenizerIdentity
	TokenBudget int
	Headroom    float64
}

// Assembly contains immutable content and its matching manifest.
type Assembly struct {
	Content  string
	Manifest Manifest
}

// RenderResult is the patch-neutral rendered selection before token gating.
type RenderResult struct {
	Content    string
	Included   []Included
	Exclusions []Exclusion
}

// Assemble renders, exactly counts, and digest-binds a selection.
func Assemble(ctx context.Context, req Request) (Assembly, error) {
	if req.Counter == nil {
		return Assembly{}, fmt.Errorf("%w: token counter is required", ErrInvalidSelection)
	}
	if strings.TrimSpace(req.Tokenizer.Name) == "" {
		return Assembly{}, fmt.Errorf("%w: tokenizer name is required", ErrInvalidSelection)
	}
	if req.TokenBudget <= 0 {
		req.TokenBudget = contextbuilder.DefaultTokenBudget
	}
	if req.Headroom == 0 {
		req.Headroom = 0.10
	}
	if req.Headroom < 0 || req.Headroom >= 1 {
		return Assembly{}, fmt.Errorf("%w: headroom must be in [0,1)", ErrInvalidSelection)
	}

	rendered, roots, err := render(ctx, req.Roots, req.Entries)
	if err != nil {
		return Assembly{}, err
	}
	for _, root := range roots {
		if strings.TrimSpace(root.Digest) == "" {
			return Assembly{}, fmt.Errorf("%w: root %q digest is required", ErrInvalidSelection, root.ID)
		}
	}
	tokenCount := req.Counter.Count(rendered.Content)
	if tokenCount < 0 {
		return Assembly{}, fmt.Errorf("%w: token counter returned a negative count", ErrInvalidSelection)
	}
	limit := EffectiveTokenLimit(req.TokenBudget, req.Headroom)
	if tokenCount > limit {
		return Assembly{}, fmt.Errorf("%w: exact=%d limit=%d budget=%d headroom=%.2f",
			ErrBudgetExceeded, tokenCount, limit, req.TokenBudget, req.Headroom)
	}

	exclusions := append([]Exclusion(nil), req.Exclusions...)
	knownRoots := rootOrder(roots)
	for i := range exclusions {
		exclusions[i].RootID = strings.TrimSpace(exclusions[i].RootID)
		exclusions[i].Path = strings.TrimSpace(exclusions[i].Path)
		exclusions[i].Reason = strings.TrimSpace(exclusions[i].Reason)
		exclusion := exclusions[i]
		if _, ok := knownRoots[exclusion.RootID]; !ok {
			return Assembly{}, fmt.Errorf("%w: exclusion references unknown root %q", ErrInvalidSelection, exclusion.RootID)
		}
		if exclusion.Path == "" || exclusion.Reason == "" {
			return Assembly{}, fmt.Errorf("%w: exclusion path and reason are required", ErrInvalidSelection)
		}
	}
	exclusions = append(exclusions, rendered.Exclusions...)
	sortExclusions(exclusions, knownRoots)

	manifest := Manifest{
		Version:        1,
		Included:       cloneIncluded(rendered.Included),
		TokenCount:     tokenCount,
		TokenBudget:    req.TokenBudget,
		EffectiveLimit: limit,
		Exclusions:     append([]Exclusion(nil), exclusions...),
		Tokenizer:      req.Tokenizer,
		ContentSHA256:  Hash([]byte(rendered.Content)),
	}
	for _, root := range roots {
		manifest.Roots = append(manifest.Roots, RootDigest{RootID: root.ID, Digest: root.Digest})
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return Assembly{}, fmt.Errorf("context selection: encode manifest: %w", err)
	}
	manifest.ManifestSHA256 = Hash(encoded)
	return Assembly{Content: rendered.Content, Manifest: manifest}, nil
}

// Render produces deterministic selected content without applying a token budget.
func Render(ctx context.Context, roots []Root, entries []Entry) (RenderResult, error) {
	rendered, _, err := render(ctx, roots, entries)
	return rendered, err
}

func render(ctx context.Context, roots []Root, entries []Entry) (RenderResult, []Root, error) {
	orderedRoots := append([]Root(nil), roots...)
	for i := range orderedRoots {
		orderedRoots[i].ID = strings.TrimSpace(orderedRoots[i].ID)
	}
	sort.SliceStable(orderedRoots, func(i, j int) bool {
		if orderedRoots[i].Order != orderedRoots[j].Order {
			return orderedRoots[i].Order < orderedRoots[j].Order
		}
		return orderedRoots[i].ID < orderedRoots[j].ID
	})
	byID := make(map[string]Root, len(orderedRoots))
	for i := range orderedRoots {
		root := orderedRoots[i]
		if root.ID == "" || root.Source == nil {
			return RenderResult{}, nil, fmt.Errorf("%w: every root needs an ID and source", ErrInvalidSelection)
		}
		if _, exists := byID[root.ID]; exists {
			return RenderResult{}, nil, fmt.Errorf("%w: duplicate root %q", ErrInvalidSelection, root.ID)
		}
		byID[root.ID] = root
	}
	if len(orderedRoots) == 0 {
		return RenderResult{}, nil, fmt.Errorf("%w: at least one root is required", ErrInvalidSelection)
	}

	type grouped struct {
		whole         *Entry
		ranges        []LineRange
		rangeExpected string
		codemap       *Entry
	}
	groups := make(map[string]*grouped)
	for i := range entries {
		entry := entries[i]
		entry.RootID = strings.TrimSpace(entry.RootID)
		entry.Path = strings.TrimSpace(entry.Path)
		if _, ok := byID[entry.RootID]; !ok {
			return RenderResult{}, nil, fmt.Errorf("%w: unknown root %q", ErrInvalidSelection, entry.RootID)
		}
		if entry.Path == "" {
			return RenderResult{}, nil, fmt.Errorf("%w: entry path is required", ErrInvalidSelection)
		}
		key := entry.RootID + "\x00" + entry.Path
		group := groups[key]
		if group == nil {
			group = &grouped{}
			groups[key] = group
		}
		switch entry.Variant {
		case VariantWholeFile:
			copy := entry
			group.whole = &copy
		case VariantLineSlice:
			for _, lineRange := range entry.Ranges {
				if lineRange.StartLine <= 0 || lineRange.EndLine < lineRange.StartLine {
					return RenderResult{}, nil, fmt.Errorf("%w: invalid range %d-%d for %s",
						ErrInvalidSelection, lineRange.StartLine, lineRange.EndLine, entry.Path)
				}
				group.ranges = append(group.ranges, lineRange)
			}
			if entry.ExpectedSHA256 != "" {
				if group.rangeExpected != "" && group.rangeExpected != entry.ExpectedSHA256 {
					return RenderResult{}, nil, fmt.Errorf("%w: conflicting digests for %s", ErrInvalidSelection, entry.Path)
				}
				group.rangeExpected = entry.ExpectedSHA256
			}
		case VariantCodemap:
			copy := entry
			group.codemap = &copy
		default:
			return RenderResult{}, nil, fmt.Errorf("%w: unsupported variant %q", ErrInvalidSelection, entry.Variant)
		}
	}

	rootRanks := rootOrder(orderedRoots)
	type renderItem struct {
		key     string
		variant string
	}
	var items []renderItem
	var exclusions []Exclusion
	for key, group := range groups {
		rootID, path := splitKey(key)
		if group.whole != nil {
			items = append(items, renderItem{key: key, variant: VariantWholeFile})
			if len(group.ranges) > 0 {
				exclusions = append(exclusions, Exclusion{RootID: rootID, Path: path, Variant: VariantLineSlice, Reason: "superseded by whole-file selection"})
			}
			if group.codemap != nil {
				exclusions = append(exclusions, Exclusion{RootID: rootID, Path: path, Variant: VariantCodemap, Reason: "superseded by whole-file selection"})
			}
			continue
		}
		if len(group.ranges) > 0 {
			items = append(items, renderItem{key: key, variant: VariantLineSlice})
		}
		if group.codemap != nil {
			items = append(items, renderItem{key: key, variant: VariantCodemap})
		}
	}
	variantRank := map[string]int{VariantWholeFile: 0, VariantLineSlice: 1, VariantCodemap: 2}
	sort.Slice(items, func(i, j int) bool {
		ri, pi := splitKey(items[i].key)
		rj, pj := splitKey(items[j].key)
		if rootRanks[ri] != rootRanks[rj] {
			return rootRanks[ri] < rootRanks[rj]
		}
		if variantRank[items[i].variant] != variantRank[items[j].variant] {
			return variantRank[items[i].variant] < variantRank[items[j].variant]
		}
		return pi < pj
	})

	var sections []string
	var included []Included
	multipleRoots := len(orderedRoots) > 1
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return RenderResult{}, nil, err
		}
		rootID, path := splitKey(item.key)
		root := byID[rootID]
		group := groups[item.key]
		content, err := root.Source.ReadFile(ctx, path)
		if err != nil {
			return RenderResult{}, nil, err
		}
		sourceHash := Hash(content)
		displayPath := path
		if multipleRoots {
			displayPath = rootID + ":" + path
		}

		switch item.variant {
		case VariantWholeFile:
			if err := verifyExpected(group.whole.ExpectedSHA256, sourceHash, rootID, path); err != nil {
				return RenderResult{}, nil, err
			}
			rendered := string(content)
			sections = append(sections, "## file: "+displayPath+"\n"+rendered)
			included = append(included, Included{
				RootID: rootID, Path: path, Variant: VariantWholeFile,
				SourceSHA256: sourceHash, RenderedSHA256: Hash([]byte(rendered)),
			})
		case VariantLineSlice:
			if err := verifyExpected(group.rangeExpected, sourceHash, rootID, path); err != nil {
				return RenderResult{}, nil, err
			}
			lines := strings.Split(string(content), "\n")
			for _, lineRange := range CoalesceRanges(group.ranges) {
				if lineRange.StartLine > len(lines) {
					return RenderResult{}, nil, fmt.Errorf("%w: range starts after end of %s", ErrInvalidSelection, path)
				}
				if lineRange.EndLine > len(lines) {
					lineRange.EndLine = len(lines)
				}
				rendered := strings.Join(lines[lineRange.StartLine-1:lineRange.EndLine], "\n")
				sections = append(sections, fmt.Sprintf("## line-range: %s:%d-%d\n%s",
					displayPath, lineRange.StartLine, lineRange.EndLine, rendered))
				included = append(included, Included{
					RootID: rootID, Path: path, Variant: VariantLineSlice,
					Ranges: []LineRange{lineRange}, SourceSHA256: sourceHash,
					RenderedSHA256: Hash([]byte(rendered)),
				})
			}
		case VariantCodemap:
			if err := verifyExpected(group.codemap.ExpectedSHA256, sourceHash, rootID, path); err != nil {
				return RenderResult{}, nil, err
			}
			structure, err := contextbuilder.NewStructureFacade().Analyze(contextbuilder.StructureRequest{Path: path, Content: content})
			if err != nil {
				return RenderResult{}, nil, fmt.Errorf("context selection: render codemap %s: %w", path, err)
			}
			encoded, err := json.Marshal(structure)
			if err != nil {
				return RenderResult{}, nil, fmt.Errorf("context selection: encode codemap %s: %w", path, err)
			}
			sections = append(sections, "## codemap: "+displayPath+"\n"+string(encoded))
			included = append(included, Included{
				RootID: rootID, Path: path, Variant: VariantCodemap,
				SourceSHA256: sourceHash, RenderedSHA256: Hash(encoded),
			})
		}
	}
	return RenderResult{Content: strings.Join(sections, "\n\n"), Included: included, Exclusions: exclusions}, orderedRoots, nil
}

func verifyExpected(expected, observed, rootID, path string) error {
	if expected != "" && expected != observed {
		return fmt.Errorf("%w: %s:%s", ErrSourceDigestMismatch, rootID, path)
	}
	return nil
}

func splitKey(key string) (string, string) {
	parts := strings.SplitN(key, "\x00", 2)
	return parts[0], parts[1]
}

func rootOrder(roots []Root) map[string]int {
	order := make(map[string]int, len(roots))
	for i, root := range roots {
		order[root.ID] = i
	}
	return order
}

func sortExclusions(exclusions []Exclusion, order map[string]int) {
	sort.SliceStable(exclusions, func(i, j int) bool {
		a, b := exclusions[i], exclusions[j]
		if order[a.RootID] != order[b.RootID] {
			return order[a.RootID] < order[b.RootID]
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Variant != b.Variant {
			return a.Variant < b.Variant
		}
		return a.Reason < b.Reason
	})
}

// CoalesceRanges sorts and merges overlapping or adjacent ranges.
func CoalesceRanges(ranges []LineRange) []LineRange {
	if len(ranges) == 0 {
		return nil
	}
	ranges = append([]LineRange(nil), ranges...)
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].StartLine == ranges[j].StartLine {
			return ranges[i].EndLine < ranges[j].EndLine
		}
		return ranges[i].StartLine < ranges[j].StartLine
	})
	merged := []LineRange{ranges[0]}
	for _, current := range ranges[1:] {
		last := &merged[len(merged)-1]
		if current.StartLine <= last.EndLine+1 {
			if current.EndLine > last.EndLine {
				last.EndLine = current.EndLine
			}
			continue
		}
		merged = append(merged, current)
	}
	return merged
}

// SubtractRange removes one range from a set of coalesced ranges.
func SubtractRange(ranges []LineRange, removed LineRange) []LineRange {
	var out []LineRange
	for _, current := range ranges {
		if removed.EndLine < current.StartLine || removed.StartLine > current.EndLine {
			out = append(out, current)
			continue
		}
		if removed.StartLine > current.StartLine {
			out = append(out, LineRange{StartLine: current.StartLine, EndLine: removed.StartLine - 1})
		}
		if removed.EndLine < current.EndLine {
			out = append(out, LineRange{StartLine: removed.EndLine + 1, EndLine: current.EndLine})
		}
	}
	return out
}

// EffectiveTokenLimit applies deterministic headroom to a token budget.
func EffectiveTokenLimit(budget int, headroom float64) int {
	return int(math.Floor(float64(budget) * (1 - headroom)))
}

// Hash returns the lowercase SHA-256 digest of data.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cloneIncluded(in []Included) []Included {
	out := append([]Included(nil), in...)
	for i := range out {
		out[i].Ranges = append([]LineRange(nil), out[i].Ranges...)
	}
	return out
}
