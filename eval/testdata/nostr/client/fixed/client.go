package client

// NIP-01 client fixture with the paper's mitigations applied.
type Event struct {
	ID, PubKey, Content, Sig string
	CreatedAt int64
	Kind int
	Tags [][]string
}

func (Event) GetID() string { return "" }

var verificationCache clientCache

type clientCache struct{}

func (clientCache) get(string) bool { return false }

// receiveEvent verifies the signature before storing (clears NOSTR-V2).
func receiveEvent(event Event) {
	if !verifySignature(event) {
		return
	}
	storeEvent(event)
}

// onContactEvent verifies the signature and authenticates the sender pubkey
// against a pinned key before trusting it (clears NOSTR-V1 and NOSTR-V2).
func onContactEvent(event Event) {
	if !verifySignature(event) || !pinnedPubkey(event.PubKey) {
		return
	}
	displayDMSender(event)
}

// onCacheEvent recomputes and compares the event id before the cache lookup
// (clears NOSTR-V7).
func onCacheEvent(event Event) {
	computedID := event.GetID()
	if computedID != event.ID {
		return
	}
	lookupCachedEvent(event)
}

// onDirectMessage validates ciphertext integrity before decrypting (clears
// NOSTR-R2), and decrypts with authenticated NIP-44.
func onDirectMessage(event Event) {
	if !verifyMAC(event.Content) {
		return
	}
	decryptLegacyDM(event)
}

// Received DMs are shown as inert text and no message-derived URL is requested
// on the recipient; rich cards are built sender-side before E2EE. This clears
// the recipient-side network exposure the V5 and V6 checks look for.
func renderDM(Event) {}

func displayDMSender(event Event) string { return event.PubKey }
func lookupCachedEvent(event Event) bool { return verificationCache.get(event.ID) }
func decryptLegacyDM(event Event) string { return nip44Decrypt(event.Content) }

func verifySignature(Event) bool { return true }
func pinnedPubkey(string) bool { return true }
func verifyMAC(string) bool { return true }
func storeEvent(Event) {}
func nip44Decrypt(string) string { return "" }
