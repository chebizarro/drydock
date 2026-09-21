package signing

import (
	"context"

	"fiatjaf.com/nostr"
)

// Signer is the minimal signing capability shared by every Drydock component
// that publishes Nostr events: resolve the service public key and sign events.
// It is the single canonical declaration; consumers import it rather than
// re-declaring an identical interface. Encryption-capable consumers embed it
// (see codechat.Keyer).
type Signer interface {
	GetPublicKey(ctx context.Context) (nostr.PubKey, error)
	SignEvent(ctx context.Context, evt *nostr.Event) error
}
