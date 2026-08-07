package fixture

type Event struct {
	id         string
	created_at int64
}

var db database

type database struct{}

func (database) Put(Event) {}

func handleEVENT(event Event)  { persistEvent(event) }
func persistEvent(event Event) { db.Put(event) }
