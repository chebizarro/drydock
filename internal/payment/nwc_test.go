package payment

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func TestParseNWCURI_Valid(t *testing.T) {
	// Valid NWC URI with all required parameters
	uri := "nostr+walletconnect://b889ff5b1513b641e2a139f661a661364979c5beee91842f8f0ef42ab558e9d4?relay=wss://relay.example.com&secret=71a8c14c1407c113601079c4302dab36460f0ccd0ad506f1f2dc73b5100e4f3c"

	conn, err := ParseNWCURI(uri)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if conn.WalletPubkey.Hex() != "b889ff5b1513b641e2a139f661a661364979c5beee91842f8f0ef42ab558e9d4" {
		t.Errorf("wallet pubkey mismatch: got %s", conn.WalletPubkey.Hex())
	}

	if conn.RelayURL != "wss://relay.example.com" {
		t.Errorf("relay URL mismatch: got %s", conn.RelayURL)
	}

	// Secret should be valid (32 bytes)
	if len(conn.Secret) != 32 {
		t.Errorf("secret length mismatch: got %d", len(conn.Secret))
	}
}

func TestParseNWCURI_InvalidPrefix(t *testing.T) {
	uri := "https://example.com"
	_, err := ParseNWCURI(uri)
	if err == nil {
		t.Fatal("expected error for invalid prefix")
	}
	if err.Error() != "invalid NWC URI: must start with nostr+walletconnect://" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseNWCURI_MissingWalletPubkey(t *testing.T) {
	uri := "nostr+walletconnect://?relay=wss://relay.example.com&secret=71a8c14c1407c113601079c4302dab36460f0ccd0ad506f1f2dc73b5100e4f3c"
	_, err := ParseNWCURI(uri)
	if err == nil {
		t.Fatal("expected error for missing wallet pubkey")
	}
}

func TestParseNWCURI_MissingRelay(t *testing.T) {
	uri := "nostr+walletconnect://b889ff5b1513b641e2a139f661a661364979c5beee91842f8f0ef42ab558e9d4?secret=71a8c14c1407c113601079c4302dab36460f0ccd0ad506f1f2dc73b5100e4f3c"
	_, err := ParseNWCURI(uri)
	if err == nil {
		t.Fatal("expected error for missing relay")
	}
	if err.Error() != "invalid NWC URI: missing relay parameter" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseNWCURI_MissingSecret(t *testing.T) {
	uri := "nostr+walletconnect://b889ff5b1513b641e2a139f661a661364979c5beee91842f8f0ef42ab558e9d4?relay=wss://relay.example.com"
	_, err := ParseNWCURI(uri)
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
	if err.Error() != "invalid NWC URI: missing secret parameter" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseNWCURI_InvalidWalletPubkey(t *testing.T) {
	uri := "nostr+walletconnect://invalidpubkey?relay=wss://relay.example.com&secret=71a8c14c1407c113601079c4302dab36460f0ccd0ad506f1f2dc73b5100e4f3c"
	_, err := ParseNWCURI(uri)
	if err == nil {
		t.Fatal("expected error for invalid wallet pubkey")
	}
}

func TestParseNWCURI_InvalidSecret(t *testing.T) {
	uri := "nostr+walletconnect://b889ff5b1513b641e2a139f661a661364979c5beee91842f8f0ef42ab558e9d4?relay=wss://relay.example.com&secret=invalidsecret"
	_, err := ParseNWCURI(uri)
	if err == nil {
		t.Fatal("expected error for invalid secret")
	}
}

func TestNewNWCInvoiceProvider_Valid(t *testing.T) {
	cfg := NWCConfig{
		URI: "nostr+walletconnect://b889ff5b1513b641e2a139f661a661364979c5beee91842f8f0ef42ab558e9d4?relay=wss://relay.example.com&secret=71a8c14c1407c113601079c4302dab36460f0ccd0ad506f1f2dc73b5100e4f3c",
	}

	provider, err := NewNWCInvoiceProvider(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if provider == nil {
		t.Fatal("expected non-nil provider")
	}

	if provider.cfg.Timeout != 30*1e9 { // 30 seconds in nanoseconds
		t.Errorf("expected default timeout of 30s, got %v", provider.cfg.Timeout)
	}
}

func TestNewNWCInvoiceProvider_EmptyURI(t *testing.T) {
	cfg := NWCConfig{}
	_, err := NewNWCInvoiceProvider(cfg)
	if err == nil {
		t.Fatal("expected error for empty URI")
	}
	if err.Error() != "NWC URI required" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewNWCInvoiceProvider_InvalidURI(t *testing.T) {
	cfg := NWCConfig{
		URI: "invalid://uri",
	}
	_, err := NewNWCInvoiceProvider(cfg)
	if err == nil {
		t.Fatal("expected error for invalid URI")
	}
}

func TestValidateCreatedInvoiceRejectsHostileResponses(t *testing.T) {
	now := time.Now().Unix()
	hash := strings.Repeat("11", 32)
	valid := makeInvoiceResult{Type: "incoming", Invoice: "lnbc1valid", PaymentHash: hash, Amount: 100000, CreatedAt: now, ExpiresAt: now + 60}
	cases := map[string]makeInvoiceResult{
		"empty bolt11":    func() makeInvoiceResult { r := valid; r.Invoice = ""; return r }(),
		"malformed hash":  func() makeInvoiceResult { r := valid; r.PaymentHash = "nope"; return r }(),
		"wrong amount":    func() makeInvoiceResult { r := valid; r.Amount++; return r }(),
		"wrong direction": func() makeInvoiceResult { r := valid; r.Type = "outgoing"; return r }(),
		"already expired": func() makeInvoiceResult { r := valid; r.ExpiresAt = now; return r }(),
		"already settled": func() makeInvoiceResult { r := valid; r.SettledAt = now; return r }(),
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := validateCreatedInvoice(result, 100000, now); err == nil {
				t.Fatal("expected hostile make_invoice response to be rejected")
			}
		})
	}
	if invoice, err := validateCreatedInvoice(valid, 100000, now); err != nil || invoice.ID != hash {
		t.Fatalf("valid make_invoice rejected: invoice=%+v err=%v", invoice, err)
	}
}

func TestValidateLookupInvoiceRejectsMismatchesAndRequiresSettlementEvidence(t *testing.T) {
	now := time.Now().Unix()
	preimage := bytesOf(0x42, 32)
	hashBytes := sha256.Sum256(preimage)
	hash := hex.EncodeToString(hashBytes[:])
	expected := Invoice{ID: hash, Request: "lnbc1expected", AmountMSats: 100000, ExpiresAt: now + 60}
	valid := lookupInvoiceResult{Type: "incoming", Invoice: expected.Request, PaymentHash: hash, Amount: expected.AmountMSats, ExpiresAt: expected.ExpiresAt}
	cases := map[string]lookupInvoiceResult{
		"wrong hash":      func() lookupInvoiceResult { r := valid; r.PaymentHash = strings.Repeat("22", 32); return r }(),
		"wrong invoice":   func() lookupInvoiceResult { r := valid; r.Invoice = "lnbc1hostile"; return r }(),
		"wrong amount":    func() lookupInvoiceResult { r := valid; r.Amount = 1; return r }(),
		"wrong direction": func() lookupInvoiceResult { r := valid; r.Type = "outgoing"; return r }(),
		"wrong expiry":    func() lookupInvoiceResult { r := valid; r.ExpiresAt++; return r }(),
		"bad preimage":    func() lookupInvoiceResult { r := valid; r.Preimage = strings.Repeat("00", 32); return r }(),
		"future settled":  func() lookupInvoiceResult { r := valid; r.SettledAt = now + 301; return r }(),
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := validateLookupInvoice(expected, result, now); err == nil {
				t.Fatal("expected hostile lookup_invoice response to be rejected")
			}
		})
	}

	settled := valid
	settled.Preimage = hex.EncodeToString(preimage)
	status, err := validateLookupInvoice(expected, settled, now)
	if err != nil || !status.Settled {
		t.Fatalf("valid preimage settlement rejected: status=%+v err=%v", status, err)
	}
	unsettled, err := validateLookupInvoice(expected, valid, now)
	if err != nil || unsettled.Settled {
		t.Fatalf("valid unsettled invoice rejected: status=%+v err=%v", unsettled, err)
	}
}

func bytesOf(value byte, count int) []byte {
	out := make([]byte, count)
	for i := range out {
		out[i] = value
	}
	return out
}

func TestValidatePayInvoiceResultRequiresPreimage(t *testing.T) {
	now := time.Now().Unix()
	preimage := bytesOf(0x31, 32)
	evidence, err := validatePayInvoiceResult(payInvoiceResult{Preimage: hex.EncodeToString(preimage)}, now)
	if err != nil || !evidence.Settled || evidence.SettledAt != now {
		t.Fatalf("valid pay result rejected: evidence=%+v err=%v", evidence, err)
	}
	hash := sha256.Sum256(preimage)
	if evidence.PaymentHash != hex.EncodeToString(hash[:]) {
		t.Fatalf("derived hash=%q", evidence.PaymentHash)
	}
	if _, err := validatePayInvoiceResult(payInvoiceResult{}, now); err == nil {
		t.Fatal("missing preimage accepted as settlement")
	}
}

func TestValidateOutgoingPaymentSettlementAndAmbiguity(t *testing.T) {
	now := time.Now().Unix()
	preimage := bytesOf(0x44, 32)
	hash := sha256.Sum256(preimage)
	bolt11 := "lnbc1outgoing"
	base := lookupInvoiceResult{
		Type: "outgoing", Invoice: bolt11, Amount: 250000,
		PaymentHash: hex.EncodeToString(hash[:]),
	}
	pending := base
	pending.State = "pending"
	evidence, err := validateOutgoingPayment(bolt11, 250000, pending, now)
	if err != nil || evidence.Settled || evidence.Failed {
		t.Fatalf("pending lookup=%+v err=%v", evidence, err)
	}
	settled := base
	settled.State = "settled"
	settled.Preimage = hex.EncodeToString(preimage)
	settled.SettledAt = now
	evidence, err = validateOutgoingPayment(bolt11, 250000, settled, now)
	if err != nil || !evidence.Settled {
		t.Fatalf("settled lookup=%+v err=%v", evidence, err)
	}
	failed := base
	failed.State = "failed"
	evidence, err = validateOutgoingPayment(bolt11, 250000, failed, now)
	if err != nil || !evidence.Failed {
		t.Fatalf("failed lookup=%+v err=%v", evidence, err)
	}
	hostile := settled
	hostile.Amount++
	if _, err := validateOutgoingPayment(bolt11, 250000, hostile, now); err == nil {
		t.Fatal("mismatched payout amount accepted")
	}
}

func TestHasTag(t *testing.T) {
	tests := []struct {
		name     string
		tags     [][]string
		key      string
		value    string
		expected bool
	}{
		{
			name:     "tag exists",
			tags:     [][]string{{"p", "abc123"}, {"e", "def456"}},
			key:      "p",
			value:    "abc123",
			expected: true,
		},
		{
			name:     "tag does not exist",
			tags:     [][]string{{"p", "abc123"}},
			key:      "e",
			value:    "def456",
			expected: false,
		},
		{
			name:     "key matches but value different",
			tags:     [][]string{{"p", "abc123"}},
			key:      "p",
			value:    "different",
			expected: false,
		},
		{
			name:     "empty tags",
			tags:     [][]string{},
			key:      "p",
			value:    "abc123",
			expected: false,
		},
		{
			name:     "tag with insufficient elements",
			tags:     [][]string{{"p"}},
			key:      "p",
			value:    "abc123",
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Convert to nostr.Tags type
			var tags nostr.Tags
			for _, tag := range tc.tags {
				tags = append(tags, tag)
			}

			result := hasTag(tags, tc.key, tc.value)
			if result != tc.expected {
				t.Errorf("expected %v, got %v", tc.expected, result)
			}
		})
	}
}
