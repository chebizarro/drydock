package agenttools

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
	"sync"

	"drydock/internal/contextbuilder"
	"drydock/internal/metrics"
	"drydock/internal/workspacesnapshot"
)

type ArtifactKind string

const (
	ArtifactPatch     ArtifactKind = "patch"
	ArtifactFile      ArtifactKind = "file"
	ArtifactLineRange ArtifactKind = "line_range"
	ArtifactCodemap   ArtifactKind = "codemap"
)

var (
	ErrSelectionFinalized                = errors.New("agent tools: selection is finalized")
	ErrSelectionNotFinalized             = errors.New("agent tools: selection is not finalized")
	ErrMandatoryArtifact                 = errors.New("agent tools: mandatory artifact cannot be removed")
	ErrBudgetExceeded                    = errors.New("agent tools: exact context package exceeds token budget")
	ErrAuthoritativeTokenCounterRequired = errors.New("agent tools: authoritative token counter is required")
)

const DefaultTokenHeadroom = 0.10

type LineRange struct {
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
}

type SelectionArtifact struct {
	Kind      ArtifactKind `json:"kind"`
	Path      string       `json:"path,omitempty"`
	StartLine int          `json:"start_line,omitempty"`
	EndLine   int          `json:"end_line,omitempty"`
	Hash      string       `json:"hash"`
	Mandatory bool         `json:"mandatory,omitempty"`
}

type SelectionConfig struct {
	Snapshot     *workspacesnapshot.Snapshot
	ChangedFiles []string
	Counter      contextbuilder.TokenCounter
	TokenBudget  int
	Headroom     float64
}

type Selection struct {
	mu           sync.RWMutex
	snapshot     *workspacesnapshot.Snapshot
	counter      contextbuilder.TokenCounter
	tokenBudget  int
	headroom     float64
	changedFiles []string
	files        map[string]SelectionArtifact
	ranges       map[string][]LineRange
	rangeHashes  map[string]string
	codemaps     map[string]SelectionArtifact
	patch        SelectionArtifact
	finalized    bool
	bundle       contextbuilder.ContextBundle
}

type SelectionStatus struct {
	Artifacts      []SelectionArtifact `json:"artifacts"`
	Finalized      bool                `json:"finalized"`
	TokenBudget    int                 `json:"token_budget"`
	Headroom       float64             `json:"headroom"`
	EffectiveLimit int                 `json:"effective_limit"`
	TokenCount     int                 `json:"token_count,omitempty"`
}

type FinalizeResult struct {
	TokenCount     int      `json:"token_count"`
	TokenBudget    int      `json:"token_budget"`
	EffectiveLimit int      `json:"effective_limit"`
	ChangedFiles   []string `json:"changed_files"`
	ContextHash    string   `json:"context_hash"`
}

func NewSelection(cfg SelectionConfig) (*Selection, error) {
	if cfg.Snapshot == nil {
		return nil, fmt.Errorf("agent tools: selection snapshot is required")
	}
	if cfg.Counter == nil {
		return nil, contextbuilder.ErrTokenCounterRequired
	}
	switch cfg.Counter.(type) {
	case contextbuilder.ApproxTokenCounter, *contextbuilder.ApproxTokenCounter:
		return nil, ErrAuthoritativeTokenCounterRequired
	}
	if cfg.TokenBudget <= 0 {
		cfg.TokenBudget = contextbuilder.DefaultTokenBudget
	}
	if cfg.Headroom == 0 {
		cfg.Headroom = DefaultTokenHeadroom
	}
	if cfg.Headroom < 0 || cfg.Headroom >= 1 {
		return nil, fmt.Errorf("agent tools: token headroom must be in [0,1)")
	}
	if err := cfg.Snapshot.Verify(); err != nil {
		return nil, fmt.Errorf("agent tools: verify snapshot: %w", err)
	}
	selection := &Selection{
		snapshot: cfg.Snapshot, counter: cfg.Counter, tokenBudget: cfg.TokenBudget,
		headroom: cfg.Headroom, files: make(map[string]SelectionArtifact),
		ranges: make(map[string][]LineRange), rangeHashes: make(map[string]string),
		codemaps: make(map[string]SelectionArtifact),
	}
	selection.patch = SelectionArtifact{
		Kind: ArtifactPatch, Hash: cfg.Snapshot.PatchDigest(), Mandatory: true,
	}
	seen := make(map[string]struct{}, len(cfg.ChangedFiles))
	for _, path := range cfg.ChangedFiles {
		if _, ok := seen[path]; ok {
			continue
		}
		content, err := cfg.Snapshot.ReadFile(context.Background(), path)
		if err != nil && !errors.Is(err, workspacesnapshot.ErrNotFound) {
			return nil, fmt.Errorf("agent tools: seed changed file %s: %w", path, err)
		}
		seen[path] = struct{}{}
		selection.changedFiles = append(selection.changedFiles, path)
		selection.files[path] = SelectionArtifact{
			Kind: ArtifactFile, Path: path, Mandatory: true,
		}
		if err == nil {
			selection.files[path] = SelectionArtifact{
				Kind: ArtifactFile, Path: path, Hash: selectionHash(content), Mandatory: true,
			}
		}
	}
	sort.Strings(selection.changedFiles)
	return selection, nil
}

func (s *Selection) HandleSelectionTool(ctx context.Context, name string, arguments json.RawMessage, _ string) (Result, error) {
	switch name {
	case ToolSelectionAdd:
		var args struct {
			Artifacts []SelectionArtifact `json:"artifacts"`
		}
		if err := decodeArguments(arguments, &args); err != nil {
			return Result{}, err
		}
		if err := s.Add(ctx, args.Artifacts...); err != nil {
			return Result{}, err
		}
		return jsonResult(s.Status())
	case ToolSelectionRemove:
		var args struct {
			Artifacts []SelectionArtifact `json:"artifacts"`
		}
		if err := decodeArguments(arguments, &args); err != nil {
			return Result{}, err
		}
		if err := s.Remove(args.Artifacts...); err != nil {
			return Result{}, err
		}
		return jsonResult(s.Status())
	case ToolSelectionStatus:
		var empty struct{}
		if err := decodeArguments(arguments, &empty); err != nil {
			return Result{}, err
		}
		return jsonResult(s.Status())
	case ToolSelectionFinalize:
		var empty struct{}
		if err := decodeArguments(arguments, &empty); err != nil {
			return Result{}, err
		}
		bundle, err := s.Finalize(ctx)
		if err != nil {
			return Result{}, err
		}
		hash := sha256.Sum256([]byte(bundle.Content))
		return jsonResult(FinalizeResult{
			TokenCount: bundle.TokenCount, TokenBudget: bundle.TokenBudget,
			EffectiveLimit: effectiveTokenLimit(bundle.TokenBudget, s.headroom),
			ChangedFiles:   append([]string(nil), bundle.ChangedFiles...),
			ContextHash:    hex.EncodeToString(hash[:]),
		})
	default:
		return Result{}, ErrUnknownTool
	}
}

func (s *Selection) Add(ctx context.Context, artifacts ...SelectionArtifact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized {
		return ErrSelectionFinalized
	}
	for _, artifact := range artifacts {
		switch artifact.Kind {
		case ArtifactFile:
			content, err := s.snapshot.ReadFile(ctx, artifact.Path)
			if err != nil {
				return err
			}
			s.files[artifact.Path] = SelectionArtifact{
				Kind: ArtifactFile, Path: artifact.Path, Hash: selectionHash(content),
			}
			delete(s.ranges, artifact.Path)
			delete(s.rangeHashes, artifact.Path)
			delete(s.codemaps, artifact.Path)
		case ArtifactLineRange:
			if artifact.StartLine <= 0 || artifact.EndLine < artifact.StartLine {
				return fmt.Errorf("agent tools: invalid line range %d-%d", artifact.StartLine, artifact.EndLine)
			}
			content, err := s.snapshot.ReadFile(ctx, artifact.Path)
			if err != nil {
				return err
			}
			if _, full := s.files[artifact.Path]; full {
				continue
			}
			lines := strings.Split(string(content), "\n")
			if artifact.StartLine > len(lines) {
				return fmt.Errorf("agent tools: line range starts after end of %s", artifact.Path)
			}
			if artifact.EndLine > len(lines) {
				artifact.EndLine = len(lines)
			}
			s.ranges[artifact.Path] = coalesceRanges(append(s.ranges[artifact.Path], LineRange{
				StartLine: artifact.StartLine, EndLine: artifact.EndLine,
			}))
			s.rangeHashes[artifact.Path] = selectionHash(content)
		case ArtifactCodemap:
			content, err := s.snapshot.ReadFile(ctx, artifact.Path)
			if err != nil {
				return err
			}
			if _, full := s.files[artifact.Path]; full {
				continue
			}
			s.codemaps[artifact.Path] = SelectionArtifact{
				Kind: ArtifactCodemap, Path: artifact.Path, Hash: selectionHash(content),
			}
		case ArtifactPatch:
			return fmt.Errorf("agent tools: patch artifact is server-managed")
		default:
			return fmt.Errorf("agent tools: unsupported artifact kind %q", artifact.Kind)
		}
	}
	return nil
}

func (s *Selection) Remove(artifacts ...SelectionArtifact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized {
		return ErrSelectionFinalized
	}
	for _, artifact := range artifacts {
		switch artifact.Kind {
		case ArtifactPatch:
			return ErrMandatoryArtifact
		case ArtifactFile:
			existing, ok := s.files[artifact.Path]
			if ok && existing.Mandatory {
				return fmt.Errorf("%w: %s", ErrMandatoryArtifact, artifact.Path)
			}
			delete(s.files, artifact.Path)
		case ArtifactLineRange:
			if artifact.StartLine <= 0 || artifact.EndLine < artifact.StartLine {
				return fmt.Errorf("agent tools: invalid line range")
			}
			s.ranges[artifact.Path] = subtractRange(s.ranges[artifact.Path], LineRange{
				StartLine: artifact.StartLine, EndLine: artifact.EndLine,
			})
			if len(s.ranges[artifact.Path]) == 0 {
				delete(s.ranges, artifact.Path)
				delete(s.rangeHashes, artifact.Path)
			}
		case ArtifactCodemap:
			delete(s.codemaps, artifact.Path)
		default:
			return fmt.Errorf("agent tools: unsupported artifact kind %q", artifact.Kind)
		}
	}
	return nil
}

func (s *Selection) Status() SelectionStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := SelectionStatus{
		Artifacts: s.artifactsLocked(), Finalized: s.finalized,
		TokenBudget: s.tokenBudget, Headroom: s.headroom,
		EffectiveLimit: effectiveTokenLimit(s.tokenBudget, s.headroom),
	}
	if s.finalized {
		status.TokenCount = s.bundle.TokenCount
	}
	return status
}

func (s *Selection) Finalize(ctx context.Context) (bundle contextbuilder.ContextBundle, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if err != nil {
			metrics.AgenticFinalizationFailures.With(finalizationFailureReason(err)).Inc()
			if errors.Is(err, workspacesnapshot.ErrHashMismatch) {
				metrics.AgenticSnapshotCorruption.Inc()
			}
		}
	}()
	if s.finalized {
		return cloneBundle(s.bundle), nil
	}
	if err = s.snapshot.Verify(); err != nil {
		return contextbuilder.ContextBundle{}, fmt.Errorf("agent tools: verify snapshot before finalization: %w", err)
	}
	content, err := s.renderLocked(ctx)
	if err != nil {
		return contextbuilder.ContextBundle{}, err
	}
	bundle = contextbuilder.ContextBundle{
		Content: content, TokenBudget: s.tokenBudget,
		LayersUsed:   []string{contextbuilder.LayerPatchDiff, contextbuilder.LayerFileContext, "agent-selection"},
		ChangedFiles: append([]string(nil), s.changedFiles...),
	}
	bundle, err = GateBundle(bundle, s.counter, s.tokenBudget, s.headroom)
	if err != nil {
		return contextbuilder.ContextBundle{}, err
	}
	s.bundle = cloneBundle(bundle)
	s.finalized = true
	return cloneBundle(bundle), nil
}

func (s *Selection) Artifacts() ([]SelectionArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.finalized {
		return nil, ErrSelectionNotFinalized
	}
	return append([]SelectionArtifact(nil), s.artifactsLocked()...), nil
}

// RestoreSelection replays a persisted canonical artifact manifest through the
// ordinary renderer and exact-token finalization gate. It never accepts a
// pre-rendered context blob.
func RestoreSelection(ctx context.Context, cfg SelectionConfig, artifacts []SelectionArtifact) (*Selection, error) {
	selection, err := NewSelection(cfg)
	if err != nil {
		return nil, err
	}
	if len(artifacts) == 0 || artifacts[0].Kind != ArtifactPatch || artifacts[0].Hash != cfg.Snapshot.PatchDigest() {
		return nil, fmt.Errorf("agent tools: persisted selection is missing the authoritative patch")
	}
	seeded := selection.artifactsLocked()
	seedByPath := make(map[string]SelectionArtifact)
	for _, artifact := range seeded {
		if artifact.Kind == ArtifactFile && artifact.Mandatory {
			seedByPath[artifact.Path] = artifact
		}
	}
	for _, artifact := range artifacts[1:] {
		if artifact.Kind == ArtifactFile && artifact.Mandatory {
			seed, ok := seedByPath[artifact.Path]
			if !ok || seed.Hash != artifact.Hash {
				return nil, fmt.Errorf("%w: mandatory artifact %s", workspacesnapshot.ErrHashMismatch, artifact.Path)
			}
			continue
		}
		if artifact.Mandatory {
			return nil, fmt.Errorf("agent tools: unsupported mandatory artifact %s", artifact.Path)
		}
		if err := selection.Add(ctx, artifact); err != nil {
			return nil, err
		}
	}
	if _, err := selection.Finalize(ctx); err != nil {
		return nil, err
	}
	restored, _ := selection.Artifacts()
	if !sameSelectionArtifacts(restored, artifacts) {
		return nil, fmt.Errorf("agent tools: restored selection differs from persisted manifest")
	}
	return selection, nil
}

func sameSelectionArtifacts(a, b []SelectionArtifact) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Selection) Bundle() (contextbuilder.ContextBundle, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.finalized {
		return contextbuilder.ContextBundle{}, false
	}
	return cloneBundle(s.bundle), true
}

func (s *Selection) Snapshot() *workspacesnapshot.Snapshot { return s.snapshot }

func GateBundle(bundle contextbuilder.ContextBundle, counter contextbuilder.TokenCounter, tokenBudget int, headroom float64) (contextbuilder.ContextBundle, error) {
	if counter == nil {
		return contextbuilder.ContextBundle{}, contextbuilder.ErrTokenCounterRequired
	}
	if tokenBudget <= 0 {
		tokenBudget = contextbuilder.DefaultTokenBudget
	}
	if headroom == 0 {
		headroom = DefaultTokenHeadroom
	}
	if headroom < 0 || headroom >= 1 {
		return contextbuilder.ContextBundle{}, fmt.Errorf("agent tools: token headroom must be in [0,1)")
	}
	exact := counter.Count(bundle.Content)
	limit := effectiveTokenLimit(tokenBudget, headroom)
	if exact > limit {
		return contextbuilder.ContextBundle{}, fmt.Errorf("%w: exact=%d limit=%d budget=%d headroom=%.2f", ErrBudgetExceeded, exact, limit, tokenBudget, headroom)
	}
	bundle.TokenBudget = tokenBudget
	bundle.TokenCount = exact
	if limit > 0 {
		metrics.AgenticBudgetUtilization.With("context_package").Observe(float64(exact) / float64(limit))
	}
	return bundle, nil
}

func finalizationFailureReason(err error) string {
	switch {
	case errors.Is(err, ErrBudgetExceeded):
		return "budget_exceeded"
	case errors.Is(err, workspacesnapshot.ErrHashMismatch):
		return "snapshot_corruption"
	case errors.Is(err, contextbuilder.ErrTokenCounterRequired),
		errors.Is(err, ErrAuthoritativeTokenCounterRequired):
		return "tokenizer"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	default:
		return "error"
	}
}

func effectiveTokenLimit(budget int, headroom float64) int {
	return int(math.Floor(float64(budget) * (1 - headroom)))
}

func (s *Selection) renderLocked(ctx context.Context) (string, error) {
	var sections []string
	patch := s.snapshot.PatchContent()
	if selectionHash(patch) != s.patch.Hash {
		return "", workspacesnapshot.ErrHashMismatch
	}
	sections = append(sections, "## patch\n"+string(patch))
	sections = append(sections, "## changed-files\n"+strings.Join(s.changedFiles, "\n"))

	filePaths := sortedArtifactPaths(s.files)
	for _, path := range filePaths {
		artifact := s.files[path]
		content, err := s.snapshot.ReadFile(ctx, path)
		if artifact.Mandatory && artifact.Hash == "" && errors.Is(err, workspacesnapshot.ErrNotFound) {
			sections = append(sections, "## file: "+path+"\n[deleted in snapshot]")
			continue
		}
		if err != nil {
			return "", err
		}
		if selectionHash(content) != artifact.Hash {
			return "", fmt.Errorf("%w: %s", workspacesnapshot.ErrHashMismatch, path)
		}
		sections = append(sections, "## file: "+path+"\n"+string(content))
	}

	rangePaths := make([]string, 0, len(s.ranges))
	for path := range s.ranges {
		rangePaths = append(rangePaths, path)
	}
	sort.Strings(rangePaths)
	for _, path := range rangePaths {
		content, err := s.snapshot.ReadFile(ctx, path)
		if err != nil {
			return "", err
		}
		if selectionHash(content) != s.rangeHashes[path] {
			return "", fmt.Errorf("%w: %s", workspacesnapshot.ErrHashMismatch, path)
		}
		lines := strings.Split(string(content), "\n")
		for _, lineRange := range s.ranges[path] {
			sections = append(sections, fmt.Sprintf("## line-range: %s:%d-%d\n%s",
				path, lineRange.StartLine, lineRange.EndLine,
				strings.Join(lines[lineRange.StartLine-1:lineRange.EndLine], "\n")))
		}
	}

	codemapPaths := sortedArtifactPaths(s.codemaps)
	for _, path := range codemapPaths {
		artifact := s.codemaps[path]
		content, err := s.snapshot.ReadFile(ctx, path)
		if err != nil {
			return "", err
		}
		if selectionHash(content) != artifact.Hash {
			return "", fmt.Errorf("%w: %s", workspacesnapshot.ErrHashMismatch, path)
		}
		structure, err := contextbuilder.NewStructureFacade().Analyze(contextbuilder.StructureRequest{Path: path, Content: content})
		if err != nil {
			return "", fmt.Errorf("agent tools: render codemap %s: %w", path, err)
		}
		encoded, err := json.Marshal(structure)
		if err != nil {
			return "", err
		}
		sections = append(sections, "## codemap: "+path+"\n"+string(encoded))
	}
	return strings.Join(sections, "\n\n"), nil
}

func (s *Selection) artifactsLocked() []SelectionArtifact {
	artifacts := []SelectionArtifact{s.patch}
	for _, path := range sortedArtifactPaths(s.files) {
		artifacts = append(artifacts, s.files[path])
	}
	rangePaths := make([]string, 0, len(s.ranges))
	for path := range s.ranges {
		rangePaths = append(rangePaths, path)
	}
	sort.Strings(rangePaths)
	for _, path := range rangePaths {
		for _, lineRange := range s.ranges[path] {
			artifacts = append(artifacts, SelectionArtifact{
				Kind: ArtifactLineRange, Path: path, StartLine: lineRange.StartLine,
				EndLine: lineRange.EndLine, Hash: s.rangeHashes[path],
			})
		}
	}
	for _, path := range sortedArtifactPaths(s.codemaps) {
		artifacts = append(artifacts, s.codemaps[path])
	}
	return artifacts
}

func coalesceRanges(ranges []LineRange) []LineRange {
	if len(ranges) == 0 {
		return nil
	}
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

func subtractRange(ranges []LineRange, removed LineRange) []LineRange {
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

func sortedArtifactPaths(artifacts map[string]SelectionArtifact) []string {
	paths := make([]string, 0, len(artifacts))
	for path := range artifacts {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func selectionHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func cloneBundle(bundle contextbuilder.ContextBundle) contextbuilder.ContextBundle {
	bundle.LayersUsed = append([]string(nil), bundle.LayersUsed...)
	bundle.LayersDropped = append([]string(nil), bundle.LayersDropped...)
	bundle.LayerStatuses = append([]contextbuilder.LayerStatus(nil), bundle.LayerStatuses...)
	bundle.ExcludedFiles = append([]string(nil), bundle.ExcludedFiles...)
	bundle.TestCoverageGaps = append([]string(nil), bundle.TestCoverageGaps...)
	bundle.ChangedFiles = append([]string(nil), bundle.ChangedFiles...)
	return bundle
}
