package fixture

type Event struct{ content []byte }

func handleEvent(event Event)            { decryptDM(event.content) }
func decryptDM(ciphertext []byte) []byte { return decrypt(ciphertext) }
func decrypt([]byte) []byte              { return nil }
