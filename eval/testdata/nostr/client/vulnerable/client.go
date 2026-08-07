package client

// NIP-01 client fixture modeling the paper's vulnerable PoC paths.
type Event struct {
	ID, PubKey, Content, Sig string
	CreatedAt int64
	Kind int
	Tags [][]string
}

func receiveEvent(event Event) {
	storeEvent(event)
	displaySender(event.PubKey)
	cacheLookup(event.ID)
}

func decryptLegacyDM(event Event) string { return nip04Decrypt(event.Content) }
func renderDM(event Event) { fetchPreview(event.Content) }
func fetchMessageURL(event Event) { httpGet(event.Content) }

func storeEvent(Event) {}
func displaySender(string) {}
func cacheLookup(string) {}
func nip04Decrypt(string) string { return "" }
func fetchPreview(string) {}
func httpGet(string) {}
