package fixture

type Event struct{ content []byte }

func handleEvent(event Event) { decryptDM(event.content) }
func decryptDM(ciphertext []byte) []byte {
	if len(ciphertext)%16 != 0 {
		return nil
	}
	return decrypt(ciphertext)
}
func decrypt([]byte) []byte { return nil }
