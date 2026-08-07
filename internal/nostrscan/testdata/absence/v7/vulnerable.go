package fixture

type Event struct{ id string }

var verificationCache cache

type cache struct{}

func (cache) get(string) bool { return false }

func handleEvent(event Event) { lookupCachedEvent(event) }
func lookupCachedEvent(event Event) bool {
	return verificationCache.get(event.id)
}
