package fixture

type Event struct {
	id         string
	created_at int64
}

func (Event) GetID() string { return "" }

var db database

type database struct{}

func (database) Put(Event)   {}
func seenEvents(string) bool { return false }

var lastCreatedAt int64

func handleEVENT(event Event) { persistEvent(event) }
func persistEvent(event Event) {
	computedID := event.GetID()
	if seenEvents(computedID) {
		return
	}
	if event.created_at < lastCreatedAt {
		return
	}
	db.Put(event)
}
