package signing

import (
	"fmt"
	"reflect"

	"fiatjaf.com/nostr"
)

// verifyRemoteSignedEvent validates a full signed event returned by an external
// signer before trusting any signature fields from it. Remote signers must sign
// exactly the payload we requested and must use their advertised public key.
func verifyRemoteSignedEvent(unsigned, signed *nostr.Event, expectedPubKey nostr.PubKey) error {
	if unsigned == nil {
		return fmt.Errorf("signed event verification: nil unsigned event")
	}
	if signed == nil {
		return fmt.Errorf("signed event verification: nil signed event")
	}

	if !signed.CheckID() {
		return fmt.Errorf("signed event verification: event id does not match signed payload")
	}
	if !signed.VerifySignature() {
		return fmt.Errorf("signed event verification: invalid event signature")
	}
	if signed.PubKey != expectedPubKey {
		return fmt.Errorf("signed event verification: pubkey mismatch: got %s, want %s", signed.PubKey.Hex(), expectedPubKey.Hex())
	}

	if signed.Kind != unsigned.Kind {
		return fmt.Errorf("signed event verification: kind mismatch: got %d, want %d", signed.Kind, unsigned.Kind)
	}
	if signed.Content != unsigned.Content {
		return fmt.Errorf("signed event verification: content mismatch")
	}
	if signed.CreatedAt != unsigned.CreatedAt {
		return fmt.Errorf("signed event verification: created_at mismatch: got %d, want %d", signed.CreatedAt, unsigned.CreatedAt)
	}
	if !tagsEqual(signed.Tags, unsigned.Tags) {
		return fmt.Errorf("signed event verification: tags mismatch")
	}

	return nil
}

func cloneEventPayload(evt *nostr.Event) nostr.Event {
	cloned := *evt
	if evt.Tags != nil {
		cloned.Tags = make(nostr.Tags, len(evt.Tags))
		for i := range evt.Tags {
			cloned.Tags[i] = append(cloned.Tags[i], evt.Tags[i]...)
		}
	}
	return cloned
}

func tagsEqual(a, b nostr.Tags) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}
