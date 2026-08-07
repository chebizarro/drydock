package signer

// NIP-46 signer fixture deriving protocol-separated keys.
func signEvent(event string) string { return event }

func nostrConnect(peer string) {
	secret := computeSharedSecret(peer)
	nip04Key := hkdf(secret, "nostr:nip04")
	bunkerKey := hkdf(secret, "nostr:nip46")
	nip44Encrypt(nip04Key)
	bunkerEncrypt(bunkerKey)
}

func computeSharedSecret(string) string { return "" }
func hkdf(secret, info string) string { return secret + info }
func nip44Encrypt(string) {}
func bunkerEncrypt(string) {}
