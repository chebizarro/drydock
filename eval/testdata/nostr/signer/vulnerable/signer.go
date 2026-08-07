package signer

// NIP-46 signer fixture reusing a raw ECDH secret across protocols.
func signEvent(event string) string { return event }

func nostrConnect(peer string) {
	sharedSecret := computeSharedSecret(peer)
	nip04Encrypt(sharedSecret)
	bunkerEncrypt(sharedSecret)
}

func computeSharedSecret(string) string { return "" }
func nip04Encrypt(string) {}
func bunkerEncrypt(string) {}
