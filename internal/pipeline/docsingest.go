package pipeline

import (
	"context"

	"drydock/internal/docsingest"
)

// DocsIngesterAdapter adapts docsingest.Ingester to the DocIngester interface.
type DocsIngesterAdapter struct {
	ingester *docsingest.Ingester
}

// NewDocsIngesterAdapter wraps a docsingest.Ingester for use with the pipeline.
// Returns nil if the ingester is nil (graceful degradation).
func NewDocsIngesterAdapter(ing *docsingest.Ingester) *DocsIngesterAdapter {
	if ing == nil {
		return nil
	}
	return &DocsIngesterAdapter{ingester: ing}
}

// IngestRepoDocs indexes project documentation from the given repo path.
func (a *DocsIngesterAdapter) IngestRepoDocs(ctx context.Context, repoPath, repoID string) error {
	_, err := a.ingester.Run(ctx, docsingest.Config{
		RepoPath: repoPath,
		RepoID:   repoID,
	})
	return err
}
