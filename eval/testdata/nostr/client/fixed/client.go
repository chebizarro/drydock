package client

// NIP-01 client fixture with the paper's mitigations applied.
type Event struct {
	ID, PubKey, Content, Sig string
	CreatedAt int64
	Kind int
	Tags [][]string
}

func receiveEvent(event Event) {
	if !verifySignature(event) || !pinnedPubkey(event.PubKey) {
		return
	}
	computedID := event.GetID()
	if computedID != event.ID {
		return
	}
	storeEvent(event)
	cacheLookup(computedID)
}

func decryptDM(event Event) string {
	if !verifyMAC(event.Content) {
		return ""
	}
	return nip44Decrypt(event.Content)
}

func renderDM(Event) {}
func verifySignature(Event) bool { return true }
func pinnedPubkey(string) bool { return true }
func (Event) GetID() string { return "" }
func storeEvent(Event) {}
func cacheLookup(string) {}
func verifyMAC(string) bool { return true }
func nip44Decrypt(string) string { return "" }
