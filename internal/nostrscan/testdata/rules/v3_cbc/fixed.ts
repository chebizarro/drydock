const cipher = createCipheriv("aes-256-cbc", sharedSecret, iv); const mac = hmac(authKey, ciphertext);
