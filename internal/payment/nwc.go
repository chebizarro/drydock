package payment

import (
	"context"
	"errors"
	"time"
)

// NWCConfig configures the NWC invoice provider.
type NWCConfig struct {
	URI        string // NWC connection URI (nostr+walletconnect://...)
	ClientNsec string // Client private key for NWC auth
	Timeout    time.Duration
}

// NWCInvoiceProvider implements InvoiceProvider using Nostr Wallet Connect (NIP-47).
type NWCInvoiceProvider struct {
	cfg NWCConfig
}

// NewNWCInvoiceProvider creates a new NWC invoice provider.
// Note: This is a stub implementation. Full NWC requires:
// - Parsing the connection URI to extract relay and pubkey
// - Creating NIP-47 request events
// - Publishing to the wallet relay
// - Waiting for response events
func NewNWCInvoiceProvider(cfg NWCConfig) (*NWCInvoiceProvider, error) {
	if cfg.URI == "" {
		return nil, errors.New("NWC URI required")
	}
	if cfg.ClientNsec == "" {
		return nil, errors.New("NWC client nsec required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &NWCInvoiceProvider{cfg: cfg}, nil
}

// CreateInvoice creates a Lightning invoice via NWC.
// NIP-47 method: make_invoice
func (p *NWCInvoiceProvider) CreateInvoice(ctx context.Context, sats int64, memo string, expiry time.Duration) (Invoice, error) {
	// TODO: Implement NIP-47 make_invoice
	// 1. Create request event with params: {amount: sats*1000, description: memo, expiry: int}
	// 2. Encrypt and publish to wallet relay
	// 3. Subscribe for response
	// 4. Decrypt and parse response
	// 5. Return invoice ID and BOLT11 string

	return Invoice{}, errors.New("NWC not implemented: make_invoice")
}

// LookupInvoice checks the status of a Lightning invoice via NWC.
// NIP-47 method: lookup_invoice
func (p *NWCInvoiceProvider) LookupInvoice(ctx context.Context, invoiceID string) (InvoiceStatus, error) {
	// TODO: Implement NIP-47 lookup_invoice
	// 1. Create request event with params: {payment_hash: invoiceID}
	// 2. Encrypt and publish to wallet relay
	// 3. Subscribe for response
	// 4. Decrypt and parse response
	// 5. Return settled/expired status

	return InvoiceStatus{}, errors.New("NWC not implemented: lookup_invoice")
}
