package contextbuilder

import (
	"context"

	"drydock/internal/nostrscan/knowledge"
)

const LayerNostrProtocol = "nostr-protocol"

// NostrProtocolProvider supplies the embedded knowledge pack when the Nostr lens registers it.
type NostrProtocolProvider struct{}

func NewNostrProtocolProvider() NostrProtocolProvider { return NostrProtocolProvider{} }

func (NostrProtocolProvider) LayerName() string { return LayerNostrProtocol }
func (NostrProtocolProvider) Priority() int     { return 2 }
func (NostrProtocolProvider) Build(context.Context, BuildInput) (string, error) {
	return knowledge.Context()
}

// ReviewerSystemPreamble supplies the pack's system instructions to the Nostr lens.
func (NostrProtocolProvider) ReviewerSystemPreamble() (string, error) {
	return knowledge.ReviewerSystemPreamble()
}
