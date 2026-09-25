package relay

// NIP-01 relay server fixture modeling replay acceptance.
type Event struct {
	ID, PubKey, Content, Sig string
	CreatedAt int64
	Kind int
	Tags [][]string
}

// handleEVENT is the relay's ["EVENT", ...] ingest surface. It persists the
// event with no id recomputation, deduplication, or created_at freshness check,
// so a replayed or forged-id event is accepted (NOSTR-R1).
func handleEVENT(message string, event Event) {
	if message == "EVENT" {
		persistEvent(event)
	}
	_ = "OK"
}

func persistEvent(event Event) { db.Put(event) }

var db database

type database struct{}

func (database) Put(Event) {}
