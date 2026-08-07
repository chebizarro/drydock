package fixture

type Event struct{ id string }

func (Event) GetID() string { return "" }

var verificationCache cache

type cache struct{}

func (cache) get(string) bool { return false }

func handleEvent(event Event) { lookupCachedEvent(event) }
func lookupCachedEvent(event Event) bool {
	computedID := event.GetID()
	if computedID != event.id {
		return false
	}
	return verificationCache.get(computedID)
}
