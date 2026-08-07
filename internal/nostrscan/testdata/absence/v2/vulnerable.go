package fixture

type Event struct{}

func (Event) CheckSignature() bool { return true }

func handleEvent(event Event) {
	verifiedPathOne(event)
	verifiedPathTwo(event)
	unsafePathThree(event)
}
func verifiedPathOne(event Event) { event.CheckSignature(); storeEventOne(event) }
func verifiedPathTwo(event Event) { event.CheckSignature(); renderEventTwo(event) }
func unsafePathThree(event Event) { displayEventThree(event) }
func storeEventOne(Event)         {}
func renderEventTwo(Event)        {}
func displayEventThree(Event)     {}
