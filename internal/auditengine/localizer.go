package auditengine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"drydock/internal/codemap"
	"drydock/internal/reviewengine"
)

// Localizer returns repository files likely to contain a vulnerability class.
type Localizer interface {
	Localize(context.Context, string, []codemap.RankedSymbol) ([]string, error)
}

// AntaresLocalizer calls an OpenAI-compatible vulnerability-localization route.
type AntaresLocalizer struct {
	client   reviewengine.LLMClient
	endpoint reviewengine.ModelEndpoint
}

func NewAntaresLocalizer(client reviewengine.LLMClient, endpoint reviewengine.ModelEndpoint) Localizer {
	if client == nil || strings.TrimSpace(endpoint.BaseURL) == "" || strings.TrimSpace(endpoint.Model) == "" {
		return nil
	}
	return &AntaresLocalizer{client: client, endpoint: endpoint}
}

type localizerResponse struct {
	Files          []string `json:"files"`
	CandidateFiles []string `json:"candidate_files"`
}

func (l *AntaresLocalizer) Localize(ctx context.Context, cwe string, repoMap []codemap.RankedSymbol) ([]string, error) {
	if l == nil || l.client == nil {
		return nil, fmt.Errorf("seclocalize is not configured")
	}
	var mapText strings.Builder
	for _, symbol := range repoMap {
		fmt.Fprintf(&mapText, "%s:%d %s %s\n", symbol.Path, symbol.Line, symbol.Kind, symbol.Name)
	}
	response, err := l.client.ChatCompletion(ctx, reviewengine.ChatRequest{
		BaseURL:     l.endpoint.BaseURL,
		APIKey:      l.endpoint.APIKey,
		Model:       l.endpoint.Model,
		JSONMode:    true,
		Temperature: 0,
		System:      "You localize vulnerability classes in source repositories. Return JSON only: {\"files\":[\"relative/path\"]}. Include only paths present in the repository map.",
		User:        fmt.Sprintf("Vulnerability hypothesis: %s\n\nRepository map:\n%s", strings.TrimSpace(cwe), mapText.String()),
	})
	if err != nil {
		return nil, err
	}
	var parsed localizerResponse
	if err := json.Unmarshal([]byte(response.Content), &parsed); err != nil {
		return nil, fmt.Errorf("parse seclocalize response: %w", err)
	}
	files := append(parsed.Files, parsed.CandidateFiles...)
	for i := range files {
		files[i] = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(files[i])), "./")
	}
	slices.Sort(files)
	files = slices.Compact(files)
	return files, nil
}
