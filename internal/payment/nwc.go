package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
)

// NIP-47 event kinds.
const (
	KindNWCRequest  nostr.Kind = 23194
	KindNWCResponse nostr.Kind = 23195
)

// NWCConfig configures the NWC invoice provider.
type NWCConfig struct {
	URI     string        // NWC connection URI (nostr+walletconnect://...)
	Timeout time.Duration // Request timeout (default: 30s)
}

// NWCConnection holds parsed NWC connection details.
type NWCConnection struct {
	WalletPubkey nostr.PubKey    // Wallet service pubkey
	RelayURL     string          // Relay URL for NWC communication
	Secret       nostr.SecretKey // Client secret for NIP-44 encryption
}

// NWCInvoiceProvider implements InvoiceProvider using Nostr Wallet Connect (NIP-47).
type NWCInvoiceProvider struct {
	cfg   NWCConfig
	conn  NWCConnection
	pool  *nostr.Pool
	keyer nostr.Keyer // handles signing and encryption
}

// nwcRequest represents a NIP-47 request payload.
type nwcRequest struct {
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

// nwcResponse represents a NIP-47 response payload.
type nwcResponse struct {
	ResultType string          `json:"result_type"`
	Error      *nwcError       `json:"error,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
}

type nwcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// makeInvoiceParams are the params for make_invoice method.
type makeInvoiceParams struct {
	Amount      int64  `json:"amount"`                // millisats
	Description string `json:"description,omitempty"` // memo
	Expiry      int64  `json:"expiry,omitempty"`      // seconds
}

// makeInvoiceResult is the result of make_invoice.
type makeInvoiceResult struct {
	Type            string `json:"type"` // "incoming"
	Invoice         string `json:"invoice"`
	PaymentHash     string `json:"payment_hash"`
	Preimage        string `json:"preimage,omitempty"`
	Amount          int64  `json:"amount"` // millisats
	CreatedAt       int64  `json:"created_at"`
	ExpiresAt       int64  `json:"expires_at,omitempty"`
	SettledAt       int64  `json:"settled_at,omitempty"`
	Description     string `json:"description,omitempty"`
	DescriptionHash string `json:"description_hash,omitempty"`
}

// lookupInvoiceParams are the params for lookup_invoice method.
type lookupInvoiceParams struct {
	PaymentHash string `json:"payment_hash,omitempty"`
	Invoice     string `json:"invoice,omitempty"`
}

// lookupInvoiceResult is the result of lookup_invoice.
type lookupInvoiceResult struct {
	Type        string `json:"type"` // "incoming" or "outgoing"
	Invoice     string `json:"invoice,omitempty"`
	PaymentHash string `json:"payment_hash"`
	Preimage    string `json:"preimage,omitempty"`
	Amount      int64  `json:"amount"` // millisats
	CreatedAt   int64  `json:"created_at"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
	SettledAt   int64  `json:"settled_at,omitempty"`
}

// ParseNWCURI parses a nostr+walletconnect:// URI into connection details.
func ParseNWCURI(uri string) (NWCConnection, error) {
	// Format: nostr+walletconnect://<pubkey>?relay=<url>&secret=<hex>
	if !strings.HasPrefix(uri, "nostr+walletconnect://") {
		return NWCConnection{}, errors.New("invalid NWC URI: must start with nostr+walletconnect://")
	}

	// Parse as URL
	parsed, err := url.Parse(uri)
	if err != nil {
		return NWCConnection{}, fmt.Errorf("invalid NWC URI: %w", err)
	}

	// Extract wallet pubkey from host
	walletPubkeyHex := parsed.Host
	if walletPubkeyHex == "" {
		return NWCConnection{}, errors.New("invalid NWC URI: missing wallet pubkey")
	}
	walletPubkey, err := nostr.PubKeyFromHex(walletPubkeyHex)
	if err != nil {
		return NWCConnection{}, fmt.Errorf("invalid wallet pubkey: %w", err)
	}

	// Extract relay URL
	relayURL := parsed.Query().Get("relay")
	if relayURL == "" {
		return NWCConnection{}, errors.New("invalid NWC URI: missing relay parameter")
	}

	// Extract secret
	secretHex := parsed.Query().Get("secret")
	if secretHex == "" {
		return NWCConnection{}, errors.New("invalid NWC URI: missing secret parameter")
	}
	secret, err := nostr.SecretKeyFromHex(secretHex)
	if err != nil {
		return NWCConnection{}, fmt.Errorf("invalid secret: %w", err)
	}

	return NWCConnection{
		WalletPubkey: walletPubkey,
		RelayURL:     relayURL,
		Secret:       secret,
	}, nil
}

// NewNWCInvoiceProvider creates a new NWC invoice provider.
func NewNWCInvoiceProvider(cfg NWCConfig) (*NWCInvoiceProvider, error) {
	if cfg.URI == "" {
		return nil, errors.New("NWC URI required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}

	conn, err := ParseNWCURI(cfg.URI)
	if err != nil {
		return nil, err
	}

	// Create keyer from secret for signing and NIP-44 encryption
	k := keyer.NewPlainKeySigner(conn.Secret)

	return &NWCInvoiceProvider{
		cfg:   cfg,
		conn:  conn,
		pool:  nostr.NewPool(nostr.PoolOptions{}),
		keyer: k,
	}, nil
}

// CreateInvoice creates a Lightning invoice via NWC.
// NIP-47 method: make_invoice
func (p *NWCInvoiceProvider) CreateInvoice(ctx context.Context, sats int64, memo string, expiry time.Duration) (Invoice, error) {
	params := makeInvoiceParams{
		Amount:      sats * 1000, // convert to millisats
		Description: memo,
		Expiry:      int64(expiry.Seconds()),
	}

	result, err := p.sendRequest(ctx, "make_invoice", params)
	if err != nil {
		return Invoice{}, fmt.Errorf("make_invoice failed: %w", err)
	}

	var invoiceResult makeInvoiceResult
	if err := json.Unmarshal(result, &invoiceResult); err != nil {
		return Invoice{}, fmt.Errorf("failed to parse make_invoice result: %w", err)
	}

	return Invoice{
		ID:      invoiceResult.PaymentHash,
		Request: invoiceResult.Invoice,
	}, nil
}

// LookupInvoice checks the status of a Lightning invoice via NWC.
// NIP-47 method: lookup_invoice
func (p *NWCInvoiceProvider) LookupInvoice(ctx context.Context, invoiceID string) (InvoiceStatus, error) {
	params := lookupInvoiceParams{
		PaymentHash: invoiceID,
	}

	result, err := p.sendRequest(ctx, "lookup_invoice", params)
	if err != nil {
		return InvoiceStatus{}, fmt.Errorf("lookup_invoice failed: %w", err)
	}

	var lookupResult lookupInvoiceResult
	if err := json.Unmarshal(result, &lookupResult); err != nil {
		return InvoiceStatus{}, fmt.Errorf("failed to parse lookup_invoice result: %w", err)
	}

	// Determine status
	now := time.Now().Unix()
	settled := lookupResult.SettledAt > 0
	expired := lookupResult.ExpiresAt > 0 && lookupResult.ExpiresAt < now && !settled

	return InvoiceStatus{
		Settled: settled,
		Expired: expired,
	}, nil
}

// sendRequest sends a NIP-47 request and waits for the response.
func (p *NWCInvoiceProvider) sendRequest(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	// Create timeout context
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	// Build request payload
	req := nwcRequest{
		Method: method,
		Params: params,
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Encrypt content using NIP-44 via keyer
	encrypted, err := p.keyer.Encrypt(ctx, string(reqJSON), p.conn.WalletPubkey)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt request: %w", err)
	}

	// Get client pubkey
	clientPubkey, err := p.keyer.GetPublicKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get client pubkey: %w", err)
	}

	// Create request event
	evt := nostr.Event{
		Kind:      KindNWCRequest,
		CreatedAt: nostr.Now(),
		Content:   encrypted,
		Tags: nostr.Tags{
			{"p", p.conn.WalletPubkey.Hex()},
		},
	}

	// Sign with client secret via keyer
	if err := p.keyer.SignEvent(ctx, &evt); err != nil {
		return nil, fmt.Errorf("failed to sign request: %w", err)
	}

	// Create filter for response
	filter := nostr.Filter{
		Kinds:   []nostr.Kind{KindNWCResponse},
		Authors: []nostr.PubKey{p.conn.WalletPubkey},
		Tags:    nostr.TagMap{"p": []string{clientPubkey.Hex()}},
		Since:   nostr.Timestamp(time.Now().Add(-5 * time.Second).Unix()),
	}

	// Subscribe to responses and ensure cleanup.
	relay, err := p.pool.EnsureRelay(p.conn.RelayURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to relay %s: %w", p.conn.RelayURL, err)
	}
	sub, err := relay.Subscribe(ctx, filter, nostr.SubscriptionOptions{Label: "nwc-response"})
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe for response: %w", err)
	}
	defer sub.Unsub()

	// Publish request to relays
	for res := range p.pool.PublishMany(ctx, []string{p.conn.RelayURL}, evt) {
		if res.Error != nil {
			return nil, fmt.Errorf("failed to publish to %s: %w", res.RelayURL, res.Error)
		}
		break // published successfully
	}

	// Wait for response
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case responseEvt, ok := <-sub.Events:
			if !ok {
				return nil, errors.New("subscription closed")
			}

			// Check if response is for our request
			if !hasTag(responseEvt.Tags, "e", evt.ID.Hex()) {
				continue
			}

			// Decrypt response using keyer
			decrypted, err := p.keyer.Decrypt(ctx, responseEvt.Content, p.conn.WalletPubkey)
			if err != nil {
				return nil, fmt.Errorf("failed to decrypt response: %w", err)
			}

			// Parse response
			var resp nwcResponse
			if err := json.Unmarshal([]byte(decrypted), &resp); err != nil {
				return nil, fmt.Errorf("failed to parse response: %w", err)
			}

			// Check for error
			if resp.Error != nil {
				return nil, fmt.Errorf("NWC error [%s]: %s", resp.Error.Code, resp.Error.Message)
			}

			// Verify result type matches
			if resp.ResultType != method {
				continue // not our response
			}

			return resp.Result, nil
		}
	}
}

// hasTag checks if tags contain a specific tag key-value pair.
func hasTag(tags nostr.Tags, key, value string) bool {
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == key && tag[1] == value {
			return true
		}
	}
	return false
}
