//go:build linux

// NIP-55L DBus signer client for Linux.
//
// Connects to the Signet signer daemon on the session bus at
// org.nostr.Signer and calls GetPublicKey / SignEvent methods.
package signing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"github.com/godbus/dbus/v5"
)

const (
	dbusInterface = "org.nostr.Signer"
	dbusPath      = "/org/nostr/signer"
	dbusDest      = "org.nostr.Signer"
)

// DBusSigner implements nostr.Signer via the NIP-55L DBus interface.
type DBusSigner struct {
	conn   *dbus.Conn
	obj    dbus.BusObject
	appID  string
	pubkey nostr.PubKey
}

// DBusSignerConfig holds configuration for the DBus signer.
type DBusSignerConfig struct {
	// AppID identifies this application to the signer (for ACL prompts).
	// Defaults to "drydock".
	AppID string
}

// NewDBusSigner connects to the session bus and validates the signer is available.
func NewDBusSigner(ctx context.Context, cfg DBusSignerConfig) (*DBusSigner, error) {
	appID := cfg.AppID
	if appID == "" {
		appID = "drydock"
	}

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("dbus signer: connect session bus: %w", err)
	}

	obj := conn.Object(dbusDest, dbus.ObjectPath(dbusPath))

	s := &DBusSigner{
		conn:  conn,
		obj:   obj,
		appID: appID,
	}

	// Validate by fetching the public key.
	pk, err := s.GetPublicKey(ctx)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("dbus signer validation failed: %w", err)
	}
	s.pubkey = pk

	return s, nil
}

// GetPublicKey returns the signer's public key.
func (s *DBusSigner) GetPublicKey(ctx context.Context) (nostr.PubKey, error) {
	// If we already have the pubkey cached, return it.
	if s.pubkey != (nostr.PubKey{}) {
		return s.pubkey, nil
	}

	call := s.obj.CallWithContext(ctx, dbusInterface+".GetPublicKey", 0)
	if call.Err != nil {
		return nostr.PubKey{}, fmt.Errorf("dbus GetPublicKey: %w", call.Err)
	}

	var npubStr string
	if err := call.Store(&npubStr); err != nil {
		return nostr.PubKey{}, fmt.Errorf("dbus GetPublicKey store: %w", err)
	}

	return decodeNpubOrHex(npubStr)
}

// SignEvent signs the event via the DBus signer, setting ID, PubKey, and Sig.
func (s *DBusSigner) SignEvent(ctx context.Context, evt *nostr.Event) error {
	if evt == nil {
		return errors.New("nil event")
	}

	// Serialize a snapshot of the unsigned event to JSON, and verify the signer
	// returns that same requested payload.
	requested := cloneEventPayload(evt)
	evtJSON, err := json.Marshal(&requested)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	expectedPubKey, err := s.GetPublicKey(ctx)
	if err != nil {
		return fmt.Errorf("dbus SignEvent get pubkey: %w", err)
	}
	currentUser := expectedPubKey.Hex()

	call := s.obj.CallWithContext(
		ctx,
		dbusInterface+".SignEvent",
		0,
		string(evtJSON),
		currentUser,
		s.appID,
	)
	if call.Err != nil {
		return fmt.Errorf("dbus SignEvent: %w", call.Err)
	}

	var sigJSON string
	if err := call.Store(&sigJSON); err != nil {
		return fmt.Errorf("dbus SignEvent store: %w", err)
	}

	// The response may be a full signed event JSON or just a signature.
	// Try parsing as a full event first, and verify it before trusting any fields.
	var signed nostr.Event
	if err := json.Unmarshal([]byte(sigJSON), &signed); err == nil && signed.Sig != [64]byte{} {
		if err := verifyRemoteSignedEvent(&requested, &signed, expectedPubKey); err != nil {
			return fmt.Errorf("dbus SignEvent verification failed: %w", err)
		}
		evt.ID = signed.ID
		evt.PubKey = signed.PubKey
		evt.Sig = signed.Sig
		return nil
	}

	// Otherwise treat it as just a hex signature string.
	return fmt.Errorf("dbus SignEvent: unexpected response format: %s", truncateStr(sigJSON, 100))
}

// Close releases the DBus connection.
func (s *DBusSigner) Close() error {
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}

// decodeNpubOrHex parses either an npub bech32 string or raw hex pubkey.
func decodeNpubOrHex(s string) (nostr.PubKey, error) {
	if len(s) > 4 && s[:4] == "npub" {
		prefix, data, err := nip19.Decode(s)
		if err != nil {
			return nostr.PubKey{}, fmt.Errorf("decode npub: %w", err)
		}
		if prefix != "npub" {
			return nostr.PubKey{}, fmt.Errorf("expected npub, got %s", prefix)
		}
		switch v := data.(type) {
		case nostr.PubKey:
			return v, nil
		case [32]byte:
			return nostr.PubKey(v), nil
		case string:
			return nostr.PubKeyFromHex(v)
		default:
			return nostr.PubKey{}, fmt.Errorf("unexpected npub data type: %T", data)
		}
	}
	return nostr.PubKeyFromHex(s)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
