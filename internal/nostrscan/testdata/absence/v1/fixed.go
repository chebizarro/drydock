package fixture

type Event struct{ pubkey string }

func handleEvent(event Event) { displayDMSender(event) }
func displayDMSender(event Event) string {
	fingerprint := pinnedFingerprint(event.pubkey)
	return fingerprint
}
func pinnedFingerprint(string) string { return "" }
