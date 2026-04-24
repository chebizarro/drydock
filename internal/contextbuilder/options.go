package contextbuilder

import (
	"drydock/internal/embedding"
	"drydock/internal/lspbridge"
	"drydock/internal/vectorstore"
)

// WithQdrant configures Qdrant-based retrieval for the context builder.
// Both qdrant and embed clients must be non-nil for the provider to activate.
func WithQdrant(qdrant *vectorstore.Client, embed *embedding.Client) func(*BuilderOptions) {
	return func(opts *BuilderOptions) {
		p := NewQdrantProvider(qdrant, embed)
		if p != nil {
			opts.qdrantProvider = p
		}
	}
}

// WithLSPBridge configures LSP bridge integration for enhanced symbol analysis.
func WithLSPBridge(client *lspbridge.Client) func(*BuilderOptions) {
	return func(opts *BuilderOptions) {
		if client != nil {
			opts.lspClient = client
		}
	}
}

// WithExtraProviders adds additional context providers (e.g. security scanner).
// They are appended after the default providers.
func WithExtraProviders(providers ...Provider) func(*BuilderOptions) {
	return func(opts *BuilderOptions) {
		opts.extraProviders = append(opts.extraProviders, providers...)
	}
}

// NewBuilderOptions creates BuilderOptions from a list of option functions.
func NewBuilderOptions(fns ...func(*BuilderOptions)) BuilderOptions {
	var opts BuilderOptions
	for _, fn := range fns {
		fn(&opts)
	}
	return opts
}
