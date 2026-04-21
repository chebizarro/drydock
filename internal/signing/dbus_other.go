//go:build !linux

package signing

import (
	"context"
	"errors"

	"fiatjaf.com/nostr"
)

// DBusSignerConfig holds configuration for the DBus signer.
// On non-Linux platforms this is a no-op stub.
type DBusSignerConfig struct {
	AppID string
}

// DBusSigner is a stub on non-Linux platforms.
type DBusSigner struct{}

// NewDBusSigner always returns an error on non-Linux platforms.
// NIP-55L DBus signing requires a Linux session bus.
func NewDBusSigner(_ context.Context, _ DBusSignerConfig) (*DBusSigner, error) {
	return nil, errors.New("dbus signer is only available on Linux")
}

// GetPublicKey is a stub — never called on non-Linux.
func (s *DBusSigner) GetPublicKey(_ context.Context) (nostr.PubKey, error) {
	return nostr.PubKey{}, errors.New("dbus signer is only available on Linux")
}

// SignEvent is a stub — never called on non-Linux.
func (s *DBusSigner) SignEvent(_ context.Context, _ *nostr.Event) error {
	return errors.New("dbus signer is only available on Linux")
}

// Close is a stub — never called on non-Linux.
func (s *DBusSigner) Close() error { return nil }
