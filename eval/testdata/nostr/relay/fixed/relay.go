package relay

// NIP-01 relay server fixture with replay defenses.
type Event struct {
	ID, PubKey, Content, Sig string
	CreatedAt int64
	Kind int
	Tags [][]string
}

func websocketRelayServer(message string, event Event) {
	if message != "EVENT" || !verifySignature(event) {
		return
	}
	computedID := event.GetID()
	if computedID != event.ID || seenEventsContains(computedID) || event.CreatedAt < freshnessCutoff() {
		return
	}
	storeEvent(event)
	_ = "OK"
}

func verifySignature(Event) bool { return true }
func (Event) GetID() string { return "" }
func seenEventsContains(string) bool { return false }
func freshnessCutoff() int64 { return 0 }
func storeEvent(Event) {}
