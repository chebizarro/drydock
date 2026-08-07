package fixture

type Event struct{}

func (Event) CheckSignature() bool { return true }

func handleEvent(event Event) { verifyAndStoreEvent(event) }
func verifyAndStoreEvent(event Event) {
	if !event.CheckSignature() {
		return
	}
	storeEvent(event)
}
func storeEvent(Event) {}
