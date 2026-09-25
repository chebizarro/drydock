package client

// NIP-01 client fixture modeling the paper's vulnerable PoC paths.
type Event struct {
	ID, PubKey, Content, Sig string
	CreatedAt int64
	Kind int
	Tags [][]string
}

var verificationCache clientCache

type clientCache struct{}

func (clientCache) get(string) bool { return false }

// receiveEvent stores an event without verifying its signature (NOSTR-V2).
func receiveEvent(event Event) {
	storeEvent(event)
}

// onContactEvent uses a sender pubkey as a trust anchor without authenticating
// it against a pinned key or key-transparency lookup (NOSTR-V1).
func onContactEvent(event Event) { displayDMSender(event) }

// onCacheEvent looks a wire-supplied event id up in a cache without recomputing
// and comparing it first (NOSTR-V7).
func onCacheEvent(event Event) { lookupCachedEvent(event) }

// onDirectMessage decrypts a DM without any length or integrity check (NOSTR-R2).
func onDirectMessage(event Event) { decryptLegacyDM(event) }

// renderDM auto-unfurls a received DM, triggering a network request (NOSTR-V6).
func renderDM(event Event) { fetchPreview(event.Content) }

// previewLink fetches a message-derived URL on the recipient (NOSTR-V5).
func previewLink(event Event) { http.Get(event.Content) }

func displayDMSender(event Event) string { return event.PubKey }
func lookupCachedEvent(event Event) bool { return verificationCache.get(event.ID) }
func decryptLegacyDM(event Event) string { return nip04Decrypt(event.Content) }

func storeEvent(Event) {}
func nip04Decrypt(string) string { return "" }
func fetchPreview(string) {}
