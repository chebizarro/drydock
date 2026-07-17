package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
type nwcResponseError struct {
	Code    string
	Message string
}

func (e *nwcResponseError) Error() string {
	return fmt.Sprintf("NWC error [%s]: %s", e.Code, e.Message)
}

type payInvoiceParams struct {
	Invoice string `json:"invoice"`
	Amount  int64  `json:"amount,omitempty"`
}

type payInvoiceResult struct {
	Preimage string `json:"preimage"`
	FeesPaid int64  `json:"fees_paid,omitempty"`
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
	State       string `json:"state,omitempty"`
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
	if sats <= 0 || sats > math.MaxInt64/1000 {
		return Invoice{}, errors.New("invoice amount must be a positive, non-overflowing value")
	}
	if expiry <= 0 {
		return Invoice{}, errors.New("invoice expiry must be positive")
	}
	params := makeInvoiceParams{Amount: sats * 1000, Description: memo, Expiry: int64(expiry.Seconds())}

	result, err := p.sendRequest(ctx, "make_invoice", params)
	if err != nil {
		return Invoice{}, fmt.Errorf("make_invoice failed: %w", err)
	}
	var invoiceResult makeInvoiceResult
	if err := json.Unmarshal(result, &invoiceResult); err != nil {
		return Invoice{}, fmt.Errorf("failed to parse make_invoice result: %w", err)
	}
	return validateCreatedInvoice(invoiceResult, params.Amount, time.Now().Unix())
}

// PayInvoice pays a reviewer invoice via NIP-47 pay_invoice.
func (p *NWCInvoiceProvider) PayInvoice(ctx context.Context, bolt11 string, amountSats int64) (PayoutEvidence, error) {
	bolt11 = strings.TrimSpace(bolt11)
	if !strings.HasPrefix(strings.ToLower(bolt11), "ln") || amountSats <= 0 || amountSats > math.MaxInt64/1000 {
		return PayoutEvidence{}, &PayoutSubmissionError{MayHaveSubmitted: false, Err: errors.New("valid BOLT11 destination and positive amount are required")}
	}
	result, err := p.sendRequest(ctx, "pay_invoice", payInvoiceParams{Invoice: bolt11, Amount: amountSats * 1000})
	if err != nil {
		var responseErr *nwcResponseError
		return PayoutEvidence{}, &PayoutSubmissionError{MayHaveSubmitted: !errors.As(err, &responseErr), Err: err}
	}
	var paid payInvoiceResult
	if err := json.Unmarshal(result, &paid); err != nil {
		return PayoutEvidence{}, &PayoutSubmissionError{MayHaveSubmitted: true, Err: fmt.Errorf("parse pay_invoice result: %w", err)}
	}
	evidence, err := validatePayInvoiceResult(paid, time.Now().Unix())
	if err != nil {
		return PayoutEvidence{}, &PayoutSubmissionError{MayHaveSubmitted: true, Err: err}
	}
	return evidence, nil
}

// LookupPayment reconciles a submitted outgoing payment without paying again.
func (p *NWCInvoiceProvider) LookupPayment(ctx context.Context, bolt11 string, amountSats int64) (PayoutEvidence, error) {
	bolt11 = strings.TrimSpace(bolt11)
	if !strings.HasPrefix(strings.ToLower(bolt11), "ln") || amountSats <= 0 || amountSats > math.MaxInt64/1000 {
		return PayoutEvidence{}, errors.New("valid BOLT11 destination and positive amount are required")
	}
	result, err := p.sendRequest(ctx, "lookup_invoice", lookupInvoiceParams{Invoice: bolt11})
	if err != nil {
		return PayoutEvidence{}, fmt.Errorf("lookup outgoing invoice failed: %w", err)
	}
	var lookup lookupInvoiceResult
	if err := json.Unmarshal(result, &lookup); err != nil {
		return PayoutEvidence{}, fmt.Errorf("parse outgoing lookup result: %w", err)
	}
	return validateOutgoingPayment(bolt11, amountSats*1000, lookup, time.Now().Unix())
}

func validatePayInvoiceResult(result payInvoiceResult, now int64) (PayoutEvidence, error) {
	preimage, err := hex.DecodeString(result.Preimage)
	if err != nil || len(preimage) != 32 {
		return PayoutEvidence{}, errors.New("pay_invoice returned malformed preimage")
	}
	hash := sha256.Sum256(preimage)
	return PayoutEvidence{
		PaymentHash: hex.EncodeToString(hash[:]),
		Preimage:    strings.ToLower(result.Preimage),
		SettledAt:   now,
		Settled:     true,
	}, nil
}

func validateOutgoingPayment(bolt11 string, expectedMSats int64, result lookupInvoiceResult, now int64) (PayoutEvidence, error) {
	if result.Type != "outgoing" || result.Invoice != bolt11 || result.Amount != expectedMSats {
		return PayoutEvidence{}, errors.New("outgoing payment lookup correlation mismatch")
	}
	state := strings.ToLower(result.State)
	if state == "" {
		if result.Preimage != "" || result.SettledAt > 0 {
			state = "settled"
		} else {
			state = "pending"
		}
	}
	if state != "pending" && state != "settled" && state != "failed" {
		return PayoutEvidence{}, fmt.Errorf("outgoing payment lookup returned unknown state %q", result.State)
	}
	if state == "failed" {
		return PayoutEvidence{Failed: true}, nil
	}
	if state == "pending" {
		if result.Preimage != "" || result.SettledAt != 0 {
			return PayoutEvidence{}, errors.New("pending outgoing payment returned settlement evidence")
		}
		return PayoutEvidence{}, nil
	}
	if result.SettledAt <= 0 || result.SettledAt > now+300 {
		return PayoutEvidence{}, errors.New("settled outgoing payment returned invalid settled_at")
	}
	hashBytes, err := parsePaymentHash(result.PaymentHash)
	if err != nil {
		return PayoutEvidence{}, fmt.Errorf("settled outgoing payment hash: %w", err)
	}
	preimage, err := hex.DecodeString(result.Preimage)
	if err != nil || len(preimage) != 32 {
		return PayoutEvidence{}, errors.New("settled outgoing payment returned malformed preimage")
	}
	hash := sha256.Sum256(preimage)
	if !strings.EqualFold(hex.EncodeToString(hash[:]), hex.EncodeToString(hashBytes)) {
		return PayoutEvidence{}, errors.New("outgoing payment preimage does not match payment hash")
	}
	return PayoutEvidence{
		PaymentHash: strings.ToLower(result.PaymentHash),
		Preimage:    strings.ToLower(result.Preimage),
		SettledAt:   result.SettledAt,
		Settled:     true,
	}, nil
}

// LookupInvoice checks the status of a Lightning invoice via NWC.
// NIP-47 method: lookup_invoice
func (p *NWCInvoiceProvider) LookupInvoice(ctx context.Context, invoice Invoice) (InvoiceStatus, error) {
	if err := validateExpectedInvoice(invoice); err != nil {
		return InvoiceStatus{}, err
	}
	params := lookupInvoiceParams{PaymentHash: invoice.ID}
	result, err := p.sendRequest(ctx, "lookup_invoice", params)
	if err != nil {
		return InvoiceStatus{}, fmt.Errorf("lookup_invoice failed: %w", err)
	}
	var lookupResult lookupInvoiceResult
	if err := json.Unmarshal(result, &lookupResult); err != nil {
		return InvoiceStatus{}, fmt.Errorf("failed to parse lookup_invoice result: %w", err)
	}
	return validateLookupInvoice(invoice, lookupResult, time.Now().Unix())
}

func validateCreatedInvoice(result makeInvoiceResult, expectedMSats, now int64) (Invoice, error) {
	if result.Type != "incoming" {
		return Invoice{}, fmt.Errorf("make_invoice returned type %q, want incoming", result.Type)
	}
	if _, err := parsePaymentHash(result.PaymentHash); err != nil {
		return Invoice{}, fmt.Errorf("invalid make_invoice payment hash: %w", err)
	}
	if result.Invoice == "" || !strings.HasPrefix(strings.ToLower(result.Invoice), "ln") {
		return Invoice{}, errors.New("make_invoice returned an empty or malformed BOLT11")
	}
	if result.Amount != expectedMSats || result.Amount <= 0 {
		return Invoice{}, fmt.Errorf("make_invoice amount mismatch: got %d want %d", result.Amount, expectedMSats)
	}
	if result.ExpiresAt <= now {
		return Invoice{}, errors.New("make_invoice returned an already-expired invoice")
	}
	if result.SettledAt != 0 || result.Preimage != "" {
		return Invoice{}, errors.New("make_invoice unexpectedly returned settlement evidence")
	}
	return Invoice{ID: strings.ToLower(result.PaymentHash), Request: result.Invoice, AmountMSats: result.Amount, ExpiresAt: result.ExpiresAt}, nil
}

func validateExpectedInvoice(invoice Invoice) error {
	if _, err := parsePaymentHash(invoice.ID); err != nil {
		return fmt.Errorf("invalid expected payment hash: %w", err)
	}
	if invoice.Request == "" || invoice.AmountMSats <= 0 || invoice.ExpiresAt <= 0 {
		return errors.New("persisted invoice evidence is incomplete")
	}
	return nil
}

func validateLookupInvoice(expected Invoice, result lookupInvoiceResult, now int64) (InvoiceStatus, error) {
	if err := validateExpectedInvoice(expected); err != nil {
		return InvoiceStatus{}, err
	}
	if result.Type != "incoming" {
		return InvoiceStatus{}, fmt.Errorf("lookup_invoice returned type %q, want incoming", result.Type)
	}
	if _, err := parsePaymentHash(result.PaymentHash); err != nil || !strings.EqualFold(result.PaymentHash, expected.ID) {
		return InvoiceStatus{}, errors.New("lookup_invoice payment hash mismatch")
	}
	if result.Invoice != "" && result.Invoice != expected.Request {
		return InvoiceStatus{}, errors.New("lookup_invoice BOLT11 mismatch")
	}
	if result.Amount != expected.AmountMSats || result.Amount <= 0 {
		return InvoiceStatus{}, fmt.Errorf("lookup_invoice amount mismatch: got %d want %d", result.Amount, expected.AmountMSats)
	}
	if result.ExpiresAt != 0 && result.ExpiresAt != expected.ExpiresAt {
		return InvoiceStatus{}, errors.New("lookup_invoice expiry mismatch")
	}

	settled := result.SettledAt > 0 || result.Preimage != ""
	if result.Preimage != "" {
		preimage, err := hex.DecodeString(result.Preimage)
		if err != nil || len(preimage) != 32 {
			return InvoiceStatus{}, errors.New("lookup_invoice returned malformed preimage")
		}
		hash := sha256.Sum256(preimage)
		if !strings.EqualFold(hex.EncodeToString(hash[:]), expected.ID) {
			return InvoiceStatus{}, errors.New("lookup_invoice preimage does not match payment hash")
		}
	}
	if result.SettledAt < 0 || (result.SettledAt > now+300) {
		return InvoiceStatus{}, errors.New("lookup_invoice returned invalid settled_at")
	}
	return InvoiceStatus{
		PaymentHash: strings.ToLower(result.PaymentHash), AmountMSats: result.Amount,
		SettledAt: result.SettledAt, Preimage: result.Preimage, Settled: settled,
		Expired: !settled && expected.ExpiresAt < now,
	}, nil
}

func parsePaymentHash(value string) ([]byte, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return nil, errors.New("payment hash must be 32-byte hex")
	}
	allZero := true
	for _, b := range decoded {
		allZero = allZero && b == 0
	}
	if allZero {
		return nil, errors.New("payment hash must not be zero")
	}
	return decoded, nil
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
			if responseEvt.PubKey != p.conn.WalletPubkey || !responseEvt.CheckID() || !responseEvt.VerifySignature() {
				continue
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
				return nil, &nwcResponseError{Code: resp.Error.Code, Message: resp.Error.Message}
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
