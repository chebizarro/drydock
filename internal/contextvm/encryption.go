package contextvm

import (
	"context"
	"errors"
	"fmt"

	"fiatjaf.com/nostr"
)

// Cipher supports NIP-44 encryption used by NIP-59 gift-wrap flows.
type Cipher interface {
	Encrypt(ctx context.Context, plaintext string, recipient nostr.PubKey) (string, error)
	Decrypt(ctx context.Context, base64ciphertext string, sender nostr.PubKey) (string, error)
}

// EncryptNIP44 encrypts plaintext for a recipient using the configured signer/cipher.
func EncryptNIP44(ctx context.Context, cipher Cipher, plaintext string, recipient nostr.PubKey) (string, error) {
	if cipher == nil {
		return "", errors.New("contextvm cipher is required")
	}
	sealed, err := cipher.Encrypt(ctx, plaintext, recipient)
	if err != nil {
		return "", fmt.Errorf("contextvm nip44 encrypt: %w", err)
	}
	return sealed, nil
}

// DecryptNIP44 decrypts ciphertext from a sender using the configured signer/cipher.
func DecryptNIP44(ctx context.Context, cipher Cipher, ciphertext string, sender nostr.PubKey) (string, error) {
	if cipher == nil {
		return "", errors.New("contextvm cipher is required")
	}
	plaintext, err := cipher.Decrypt(ctx, ciphertext, sender)
	if err != nil {
		return "", fmt.Errorf("contextvm nip44 decrypt: %w", err)
	}
	return plaintext, nil
}

// GiftWrap prepares an encrypted NIP-59 wrapper event. The caller signs the event
// after any additional routing tags are added.
func GiftWrap(ctx context.Context, cipher Cipher, plaintext string, recipient nostr.PubKey) (nostr.Event, error) {
	ciphertext, err := EncryptNIP44(ctx, cipher, plaintext, recipient)
	if err != nil {
		return nostr.Event{}, err
	}
	return nostr.Event{
		CreatedAt: nostr.Now(),
		Kind:      KindGiftWrap,
		Tags:      nostr.Tags{nostr.Tag{"p", recipient.Hex()}},
		Content:   ciphertext,
	}, nil
}

// OpenGiftWrap decrypts a NIP-59 wrapper event.
func OpenGiftWrap(ctx context.Context, cipher Cipher, event nostr.Event) (string, error) {
	if event.Kind != KindGiftWrap {
		return "", fmt.Errorf("contextvm expected gift wrap kind %d, got %d", KindGiftWrap, event.Kind)
	}
	return DecryptNIP44(ctx, cipher, event.Content, event.PubKey)
}
