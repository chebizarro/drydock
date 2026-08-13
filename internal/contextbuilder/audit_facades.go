package contextbuilder

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"drydock/internal/lspbridge"
)

// ReferencesFacade is the authoritative LSP-backed references analysis used by
// deterministic providers and the code.references agent tool.
type ReferencesFacade struct {
	client *lspbridge.Client
}

type ReferencesRequest struct {
	RepoPath string
	Files    []string
	Symbols  []string
}

func NewReferencesFacade(client *lspbridge.Client) *ReferencesFacade {
	return &ReferencesFacade{client: client}
}

func (f *ReferencesFacade) Analyze(ctx context.Context, req ReferencesRequest) (*lspbridge.AnalyzeResponse, error) {
	if f == nil || f.client == nil {
		return nil, fmt.Errorf("contextbuilder: LSP references are not configured")
	}
	if strings.TrimSpace(req.RepoPath) == "" || len(req.Files) == 0 || len(req.Symbols) == 0 {
		return nil, fmt.Errorf("contextbuilder: repository path, files, and symbols are required")
	}
	files := append([]string(nil), req.Files...)
	symbols := append([]string(nil), req.Symbols...)
	slices.Sort(files)
	files = slices.Compact(files)
	slices.Sort(symbols)
	symbols = slices.Compact(symbols)
	return f.client.Analyze(ctx, lspbridge.AnalyzeRequest{
		RepoPath: req.RepoPath, ChangedFiles: files, Symbols: symbols,
	})
}

// LayerFacade exposes existing context providers through a closed, named set.
type LayerFacade struct {
	providers map[string]Provider
	names     []string
}

type LayerResult struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	Warning string `json:"warning,omitempty"`
}

func NewLayerFacade(opts BuilderOptions) *LayerFacade {
	facade := &LayerFacade{providers: make(map[string]Provider)}
	for _, provider := range DefaultProviders(opts) {
		name := strings.TrimSpace(provider.LayerName())
		if name == "" {
			continue
		}
		if _, exists := facade.providers[name]; exists {
			continue
		}
		facade.providers[name] = provider
		facade.names = append(facade.names, name)
	}
	slices.Sort(facade.names)
	return facade
}

func (f *LayerFacade) Names() []string {
	if f == nil {
		return nil
	}
	return append([]string(nil), f.names...)
}

func (f *LayerFacade) Analyze(ctx context.Context, name string, input BuildInput) (LayerResult, error) {
	if f == nil {
		return LayerResult{}, fmt.Errorf("contextbuilder: context layers are not configured")
	}
	name = strings.TrimSpace(name)
	provider, ok := f.providers[name]
	if !ok {
		return LayerResult{}, fmt.Errorf("contextbuilder: unknown context layer %q", name)
	}
	content, err := provider.Build(ctx, input)
	result := LayerResult{Name: name, Content: content}
	var warning *LayerWarning
	if errors.As(err, &warning) {
		result.Warning = warning.Error()
		return result, nil
	}
	return result, err
}

// SecurityTraceFacade exposes the existing taint and security-surface providers
// as one closed security-only analysis surface.
type SecurityTraceFacade struct {
	taint   Provider
	surface Provider
}

type SecurityTraceRequest struct {
	Kind     string
	RepoPath string
	Patch    string
}

type SecurityTraceResult struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
	Warning string `json:"warning,omitempty"`
}

func NewSecurityTraceFacade(lsp *lspbridge.Client, locator securitySurfaceLocator) *SecurityTraceFacade {
	return &SecurityTraceFacade{
		taint:   taintProvider{lspClient: lsp},
		surface: NewSecuritySurfaceProvider(locator),
	}
}

func (f *SecurityTraceFacade) Analyze(ctx context.Context, req SecurityTraceRequest) (SecurityTraceResult, error) {
	if f == nil {
		return SecurityTraceResult{}, fmt.Errorf("contextbuilder: security trace is not configured")
	}
	var provider Provider
	switch strings.TrimSpace(req.Kind) {
	case "", LayerTaint:
		req.Kind = LayerTaint
		provider = f.taint
	case LayerSecuritySurface:
		provider = f.surface
	default:
		return SecurityTraceResult{}, fmt.Errorf("contextbuilder: unknown security trace %q", req.Kind)
	}
	content, err := provider.Build(ctx, BuildInput{RepoPath: req.RepoPath, PatchEventContent: req.Patch})
	result := SecurityTraceResult{Kind: req.Kind, Content: content}
	var warning *LayerWarning
	if errors.As(err, &warning) {
		result.Warning = warning.Error()
		return result, nil
	}
	return result, err
}
