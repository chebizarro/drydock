// Package context assembles deterministic, immutable context from arbitrary roots.
//
// A Selection is patch-neutral: callers identify roots explicitly and may include
// whole files, line slices, or codemaps in any combination. Assemble performs
// exact token accounting with the caller-supplied Tokenizer and returns a
// digest-bound manifest. The package does not persist selections or assemblies.
package context

import (
	stdcontext "context"
	"errors"
	"fmt"

	"git.sharegap.net/cascadia/drydock/internal/contextbuilder"
	"git.sharegap.net/cascadia/drydock/internal/contextselection"
)

var (
	// ErrBudgetExceeded reports that assembled content exceeded the effective token limit.
	ErrBudgetExceeded = errors.New("drydock context: token budget exceeded")
	// ErrInvalidSelection reports malformed roots, entries, ranges, or exclusions.
	ErrInvalidSelection = errors.New("drydock context: invalid selection")
	// ErrSourceChanged reports that source content did not match its selected digest.
	ErrSourceChanged = errors.New("drydock context: source changed")
)

// Variant identifies how a selected file is represented.
type Variant string

const (
	// VariantWholeFile includes the complete file.
	VariantWholeFile Variant = "whole_file"
	// VariantLineSlice includes one or more one-based inclusive line ranges.
	VariantLineSlice Variant = "line_slice"
	// VariantCodemap includes canonical symbol metadata instead of source text.
	VariantCodemap Variant = "codemap"
)

// ContentSource is the narrow immutable file-reading boundary used by assembly.
// repository.Snapshot satisfies this interface.
type ContentSource interface {
	ReadFile(ctx stdcontext.Context, path string) ([]byte, error)
}

// Root identifies one immutable selection root.
type Root struct {
	// ID is the stable root identity recorded in headings and the manifest.
	ID string
	// Order is the deterministic ordering key; ties are resolved by ID.
	Order int
	// Digest binds the root to an immutable repository or content snapshot.
	Digest string
	// Source reads files from that immutable snapshot.
	Source ContentSource
}

// Range is a one-based inclusive source range.
type Range struct {
	// StartLine is the first included line.
	StartLine int
	// EndLine is the last included line.
	EndLine int
}

// Entry selects one representation of a file.
type Entry struct {
	// RootID identifies the selected root.
	RootID string
	// Path is relative to the selected root.
	Path string
	// Variant selects whole-file, line-slice, or codemap rendering.
	Variant Variant
	// Ranges contains the selected ranges for VariantLineSlice.
	Ranges []Range
}

// WholeFile constructs a complete-file selection entry.
func WholeFile(rootID, path string) Entry {
	return Entry{RootID: rootID, Path: path, Variant: VariantWholeFile}
}

// LineSlice constructs a one-based inclusive line-slice selection entry.
// Adjacent and overlapping slices for the same file are coalesced.
func LineSlice(rootID, path string, startLine, endLine int) Entry {
	return Entry{
		RootID: rootID, Path: path, Variant: VariantLineSlice,
		Ranges: []Range{{StartLine: startLine, EndLine: endLine}},
	}
}

// Codemap constructs a symbol/codemap selection entry.
func Codemap(rootID, path string) Entry {
	return Entry{RootID: rootID, Path: path, Variant: VariantCodemap}
}

// Exclusion records omitted content and a stable human-readable reason.
type Exclusion struct {
	// RootID identifies the root containing the omitted content.
	RootID string
	// Path is relative to the root.
	Path string
	// Variant identifies the omitted representation when applicable.
	Variant Variant
	// Reason explains why the content was omitted.
	Reason string
}

// Selection is a complete arbitrary multi-root selection request.
type Selection struct {
	// Roots lists immutable content sources. Root Order defines cross-root order.
	Roots []Root
	// Entries lists selected whole files, slices, and codemaps.
	Entries []Entry
	// Exclusions lists caller-known omissions such as policy or budget decisions.
	Exclusions []Exclusion
}

// TokenizerIdentity identifies the exact tokenizer used for accounting.
type TokenizerIdentity struct {
	// Name identifies the tokenizer implementation.
	Name string
	// Version identifies the tokenizer implementation or vocabulary version.
	Version string
	// Encoding identifies the concrete vocabulary/encoding.
	Encoding string
}

// Tokenizer performs exact token accounting and identifies itself.
type Tokenizer interface {
	Count(text string) int
	Identity() TokenizerIdentity
}

type tiktokenTokenizer struct {
	counter  contextbuilder.TokenCounter
	identity TokenizerIdentity
}

// NewTiktokenTokenizer loads an authoritative tiktoken encoding.
// Unlike drydock's review-oriented fallback counter, this constructor fails
// closed when encoding data is unavailable.
func NewTiktokenTokenizer(encoding string) (Tokenizer, error) {
	if encoding == "" {
		encoding = "cl100k_base"
	}
	counter, err := contextbuilder.NewRequiredTiktokenCounter(encoding)
	if err != nil {
		return nil, err
	}
	return &tiktokenTokenizer{
		counter: counter,
		identity: TokenizerIdentity{
			Name: "tiktoken", Version: "github.com/pkoukk/tiktoken-go", Encoding: encoding,
		},
	}, nil
}

func (t *tiktokenTokenizer) Count(text string) int       { return t.counter.Count(text) }
func (t *tiktokenTokenizer) Identity() TokenizerIdentity { return t.identity }

// Options configures an Assembler.
type Options struct {
	// Tokenizer is required and must perform authoritative token counting.
	Tokenizer Tokenizer
	// TokenBudget is the maximum budget before headroom; non-positive uses 64,000.
	TokenBudget int
	// Headroom reserves a fraction in [0,1); zero uses 10 percent.
	Headroom float64
}

// Assembler deterministically renders selections with exact token accounting.
// An Assembler is immutable after construction and safe for concurrent use when
// its Tokenizer is safe for concurrent use.
type Assembler struct {
	tokenizer Tokenizer
	budget    int
	headroom  float64
}

// NewAssembler constructs an exact-counting context assembler.
func NewAssembler(opts Options) (*Assembler, error) {
	if opts.Tokenizer == nil {
		return nil, fmt.Errorf("%w: tokenizer is required", ErrInvalidSelection)
	}
	if opts.Tokenizer.Identity().Name == "" {
		return nil, fmt.Errorf("%w: tokenizer identity name is required", ErrInvalidSelection)
	}
	return &Assembler{tokenizer: opts.Tokenizer, budget: opts.TokenBudget, headroom: opts.Headroom}, nil
}

// RootDigest binds one manifest root to its immutable source digest.
type RootDigest struct {
	// RootID is the stable root identity.
	RootID string
	// Digest is the caller-supplied immutable snapshot digest.
	Digest string
}

// Inclusion records one rendered selection item.
type Inclusion struct {
	// RootID identifies the source root.
	RootID string
	// Path is relative to the source root.
	Path string
	// Variant identifies the rendered representation.
	Variant Variant
	// Ranges contains included one-based ranges for line slices.
	Ranges []Range
	// SourceSHA256 binds the inclusion to complete source bytes.
	SourceSHA256 string
	// RenderedSHA256 binds the inclusion to its rendered payload.
	RenderedSHA256 string
}

// Manifest is an immutable assembly description returned by Assembly.Manifest.
// Its digest covers every field except ManifestSHA256 itself.
type Manifest struct {
	// Version is the manifest schema version.
	Version int
	// Roots records deterministic root ordering and immutable digests.
	Roots []RootDigest
	// Included records rendered files, ranges, and codemaps.
	Included []Inclusion
	// TokenCount is the exact count of Content.
	TokenCount int
	// TokenBudget is the requested pre-headroom budget.
	TokenBudget int
	// EffectiveLimit is TokenBudget after headroom.
	EffectiveLimit int
	// Exclusions records all caller-supplied and normalization exclusions.
	Exclusions []Exclusion
	// Tokenizer identifies the exact counter used.
	Tokenizer TokenizerIdentity
	// ContentSHA256 is the digest of assembled content.
	ContentSHA256 string
	// ManifestSHA256 is the digest of the manifest with this field empty.
	ManifestSHA256 string
}

// Assembly is an immutable content snapshot.
// Accessors return strings or deep copies so callers cannot mutate its manifest.
type Assembly struct {
	content  string
	manifest Manifest
}

// Content returns the deterministic assembled text.
func (a *Assembly) Content() string {
	if a == nil {
		return ""
	}
	return a.content
}

// Manifest returns a deep copy of the immutable assembly manifest.
func (a *Assembly) Manifest() Manifest {
	if a == nil {
		return Manifest{}
	}
	return cloneManifest(a.manifest)
}

// Assemble renders a selection and returns a digest-bound immutable assembly.
func (a *Assembler) Assemble(ctx stdcontext.Context, selection Selection) (*Assembly, error) {
	if a == nil || a.tokenizer == nil {
		return nil, fmt.Errorf("%w: assembler is not initialized", ErrInvalidSelection)
	}
	roots := make([]contextselection.Root, 0, len(selection.Roots))
	for _, root := range selection.Roots {
		if root.Digest == "" {
			return nil, fmt.Errorf("%w: root %q digest is required", ErrInvalidSelection, root.ID)
		}
		roots = append(roots, contextselection.Root{
			ID: root.ID, Order: root.Order, Digest: root.Digest, Source: root.Source,
		})
	}
	entries := make([]contextselection.Entry, 0, len(selection.Entries))
	for _, entry := range selection.Entries {
		ranges := make([]contextselection.LineRange, 0, len(entry.Ranges))
		for _, lineRange := range entry.Ranges {
			ranges = append(ranges, contextselection.LineRange{
				StartLine: lineRange.StartLine, EndLine: lineRange.EndLine,
			})
		}
		entries = append(entries, contextselection.Entry{
			RootID: entry.RootID, Path: entry.Path, Variant: string(entry.Variant), Ranges: ranges,
		})
	}
	exclusions := make([]contextselection.Exclusion, 0, len(selection.Exclusions))
	for _, exclusion := range selection.Exclusions {
		if exclusion.Reason == "" {
			return nil, fmt.Errorf("%w: exclusion reason is required", ErrInvalidSelection)
		}
		exclusions = append(exclusions, contextselection.Exclusion{
			RootID: exclusion.RootID, Path: exclusion.Path,
			Variant: string(exclusion.Variant), Reason: exclusion.Reason,
		})
	}
	identity := a.tokenizer.Identity()
	internal, err := contextselection.Assemble(ctx, contextselection.Request{
		Roots: roots, Entries: entries, Exclusions: exclusions,
		Counter: a.tokenizer,
		Tokenizer: contextselection.TokenizerIdentity{
			Name: identity.Name, Version: identity.Version, Encoding: identity.Encoding,
		},
		TokenBudget: a.budget, Headroom: a.headroom,
	})
	if err != nil {
		switch {
		case errors.Is(err, contextselection.ErrBudgetExceeded):
			return nil, fmt.Errorf("%w: %v", ErrBudgetExceeded, err)
		case errors.Is(err, contextselection.ErrSourceDigestMismatch):
			return nil, fmt.Errorf("%w: %v", ErrSourceChanged, err)
		case errors.Is(err, contextselection.ErrInvalidSelection):
			return nil, fmt.Errorf("%w: %v", ErrInvalidSelection, err)
		default:
			return nil, err
		}
	}
	return &Assembly{content: internal.Content, manifest: fromInternalManifest(internal.Manifest)}, nil
}

func fromInternalManifest(in contextselection.Manifest) Manifest {
	out := Manifest{
		Version: in.Version, TokenCount: in.TokenCount, TokenBudget: in.TokenBudget,
		EffectiveLimit: in.EffectiveLimit,
		Tokenizer: TokenizerIdentity{
			Name: in.Tokenizer.Name, Version: in.Tokenizer.Version, Encoding: in.Tokenizer.Encoding,
		},
		ContentSHA256: in.ContentSHA256, ManifestSHA256: in.ManifestSHA256,
	}
	for _, root := range in.Roots {
		out.Roots = append(out.Roots, RootDigest{RootID: root.RootID, Digest: root.Digest})
	}
	for _, included := range in.Included {
		item := Inclusion{
			RootID: included.RootID, Path: included.Path, Variant: Variant(included.Variant),
			SourceSHA256: included.SourceSHA256, RenderedSHA256: included.RenderedSHA256,
		}
		for _, lineRange := range included.Ranges {
			item.Ranges = append(item.Ranges, Range{
				StartLine: lineRange.StartLine, EndLine: lineRange.EndLine,
			})
		}
		out.Included = append(out.Included, item)
	}
	for _, exclusion := range in.Exclusions {
		out.Exclusions = append(out.Exclusions, Exclusion{
			RootID: exclusion.RootID, Path: exclusion.Path,
			Variant: Variant(exclusion.Variant), Reason: exclusion.Reason,
		})
	}
	return out
}

func cloneManifest(in Manifest) Manifest {
	out := in
	out.Roots = append([]RootDigest(nil), in.Roots...)
	out.Exclusions = append([]Exclusion(nil), in.Exclusions...)
	out.Included = append([]Inclusion(nil), in.Included...)
	for i := range out.Included {
		out.Included[i].Ranges = append([]Range(nil), in.Included[i].Ranges...)
	}
	return out
}
