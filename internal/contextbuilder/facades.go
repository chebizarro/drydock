package contextbuilder

import (
	"context"
	"strings"

	"drydock/internal/lspbridge"
)

// PatchFacade owns structured patch analysis and deterministic patch/file rendering.
type PatchFacade struct{}

func NewPatchFacade() PatchFacade { return PatchFacade{} }
func (PatchFacade) Analyze(req PatchAnalysisRequest) (PatchAnalysisResult, error) {
	return AnalyzePatchStructure(req)
}
func (PatchFacade) Render(in BuildInput) (string, error)         { return renderPatch(in) }
func (PatchFacade) ModifiedFiles(in BuildInput) (string, error)  { return analyzeModifiedFiles(in) }
func (PatchFacade) ImportsExports(in BuildInput) (string, error) { return analyzeImportsExports(in) }

func AnalyzePatch(in BuildInput) (string, error)          { return NewPatchFacade().Render(in) }
func AnalyzeModifiedFiles(in BuildInput) (string, error)  { return NewPatchFacade().ModifiedFiles(in) }
func AnalyzeImportsExports(in BuildInput) (string, error) { return NewPatchFacade().ImportsExports(in) }

type SymbolAnalysisRequest struct {
	RepoPath       string
	Diff           string
	WorkspaceRoots []string
}
type SymbolAnalysisResult struct {
	Names    []string
	Content  string
	Warnings []error
}
type SymbolsFacade struct {
	Searcher *Searcher
	LSP      *lspbridge.Client
}

func NewSymbolsFacade(searcher *Searcher, lsp *lspbridge.Client) *SymbolsFacade {
	if searcher == nil {
		searcher = NewSearcher()
	}
	return &SymbolsFacade{Searcher: searcher, LSP: lsp}
}
func (f *SymbolsFacade) Analyze(ctx context.Context, req SymbolAnalysisRequest) (SymbolAnalysisResult, error) {
	in := BuildInput{RepoPath: req.RepoPath, PatchEventContent: req.Diff, WorkspaceRoots: req.WorkspaceRoots}
	content, err := analyzeSymbolsContent(ctx, in, f.LSP, f.Searcher)
	result := SymbolAnalysisResult{Content: content}
	if err != nil {
		if warning, ok := err.(*LayerWarning); ok {
			result.Warnings = append(result.Warnings, warning.Err)
		} else {
			return result, err
		}
	}
	if first, _, ok := strings.Cut(content, "\n"); ok && strings.HasPrefix(first, "symbols: ") {
		for _, name := range strings.Split(strings.TrimPrefix(first, "symbols: "), ", ") {
			if name != "" {
				result.Names = append(result.Names, name)
			}
		}
	}
	return result, err
}
func AnalyzeSymbols(ctx context.Context, in BuildInput, lsp *lspbridge.Client, searcher *Searcher) (string, error) {
	result, err := NewSymbolsFacade(searcher, lsp).Analyze(ctx, SymbolAnalysisRequest{RepoPath: in.RepoPath, Diff: in.PatchEventContent, WorkspaceRoots: in.WorkspaceRoots})
	return result.Content, err
}

type TestAnalysisRequest struct {
	RepoPath       string
	Diff           string
	WorkspaceRoots []string
}
type TestAnalysisResult struct {
	Content   string
	Uncovered []string
}
type TestsFacade struct{ Searcher *Searcher }

func NewTestsFacade(searcher *Searcher) *TestsFacade {
	if searcher == nil {
		searcher = NewSearcher()
	}
	return &TestsFacade{Searcher: searcher}
}
func (f *TestsFacade) Analyze(ctx context.Context, req TestAnalysisRequest) (TestAnalysisResult, error) {
	content, err := analyzeTestsContent(ctx, BuildInput{RepoPath: req.RepoPath, PatchEventContent: req.Diff, WorkspaceRoots: req.WorkspaceRoots}, f.Searcher)
	result := TestAnalysisResult{Content: content}
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, TestCoverageGapPrefix) {
			result.Uncovered = append(result.Uncovered, strings.TrimSpace(strings.TrimPrefix(line, TestCoverageGapPrefix)))
		}
	}
	return result, err
}
func AnalyzeTests(ctx context.Context, in BuildInput, searcher *Searcher) (string, error) {
	result, err := NewTestsFacade(searcher).Analyze(ctx, TestAnalysisRequest{RepoPath: in.RepoPath, Diff: in.PatchEventContent, WorkspaceRoots: in.WorkspaceRoots})
	return result.Content, err
}

type HistoryAnalysisRequest struct {
	RepoPath string
	Diff     string
}
type HistoryAnalysisResult struct{ Content string }
type HistoryFacade struct{}

func NewHistoryFacade() HistoryFacade { return HistoryFacade{} }
func (HistoryFacade) Analyze(ctx context.Context, req HistoryAnalysisRequest) (HistoryAnalysisResult, error) {
	content, err := analyzeHistoryContent(ctx, BuildInput{RepoPath: req.RepoPath, PatchEventContent: req.Diff})
	return HistoryAnalysisResult{Content: content}, err
}
func AnalyzeHistory(ctx context.Context, in BuildInput) (string, error) {
	result, err := NewHistoryFacade().Analyze(ctx, HistoryAnalysisRequest{RepoPath: in.RepoPath, Diff: in.PatchEventContent})
	return result.Content, err
}

type DocsAnalysisRequest struct {
	RepoPath       string
	WorkspaceRoots []string
}
type DocsAnalysisResult struct{ Content string }
type DocsFacade struct{}

func NewDocsFacade() DocsFacade { return DocsFacade{} }
func (DocsFacade) Analyze(req DocsAnalysisRequest) (DocsAnalysisResult, error) {
	content, err := analyzeDocsContent(BuildInput{RepoPath: req.RepoPath, WorkspaceRoots: req.WorkspaceRoots})
	return DocsAnalysisResult{Content: content}, err
}
func AnalyzeDocs(in BuildInput) (string, error) {
	result, err := NewDocsFacade().Analyze(DocsAnalysisRequest{RepoPath: in.RepoPath, WorkspaceRoots: in.WorkspaceRoots})
	return result.Content, err
}

type LSPAnalysisRequest struct {
	RepoPath string
	Diff     string
	Symbols  []string
}
type LSPFacade struct{ Client *lspbridge.Client }

func NewLSPFacade(client *lspbridge.Client) *LSPFacade { return &LSPFacade{Client: client} }
func (f *LSPFacade) Analyze(ctx context.Context, req LSPAnalysisRequest) LSPAnalysis {
	return analyzeLSP(ctx, f.Client, BuildInput{RepoPath: req.RepoPath, PatchEventContent: req.Diff}, req.Symbols)
}
func AnalyzeLSP(ctx context.Context, client *lspbridge.Client, in BuildInput, symbols []string) LSPAnalysis {
	return NewLSPFacade(client).Analyze(ctx, LSPAnalysisRequest{RepoPath: in.RepoPath, Diff: in.PatchEventContent, Symbols: symbols})
}
