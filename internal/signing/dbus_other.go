//go:build !linux

package signing

import (
	"context"
	"errors"
)

// DBusSignerConfig holds configuration for the DBus signer.
// On non-Linux platforms this is a no-op stub.
type DBusSignerConfig struct {
	AppID string
}

// NewDBusSigner always returns an error on non-Linux platforms.
// NIP-55L DBus signing requires a Linux session bus.
func NewDBusSigner(_ context.Context, _ DBusSignerConfig) (interface{}, error) {
	return nil, errors.New("dbus signer is only available on Linux")
}
