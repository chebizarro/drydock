const sharedSecret = hkdf(getSharedSecret(privateKey, peerKey), "nip44-v2");
