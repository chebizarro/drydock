package fixture

type Event struct{ pubkey string }

func handleEvent(event Event)            { displayDMSender(event) }
func displayDMSender(event Event) string { return event.pubkey }
