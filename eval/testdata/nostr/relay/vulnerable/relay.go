package relay

// NIP-01 relay server fixture modeling replay acceptance.
type Event struct {
	ID, PubKey, Content, Sig string
	CreatedAt int64
	Kind int
	Tags [][]string
}

func websocketRelayServer(message string, event Event) {
	if message == "EVENT" {
		storeEvent(event)
	}
	_ = "OK"
}

func storeEvent(Event) {}
