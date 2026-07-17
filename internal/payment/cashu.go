package payment

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CashuMintClient implements MintClient using HTTP requests to Cashu mints.
type CashuMintClient struct {
	httpClient *http.Client
	timeout    time.Duration
}

type createMeltQuoteRequest struct {
	Request string `json:"request"`
	Unit    string `json:"unit"`
}

type meltTokenRequest struct {
	Quote  string          `json:"quote"`
	Inputs json.RawMessage `json:"inputs"`
}

// NewCashuMintClient creates a new Cashu mint client.
func NewCashuMintClient(timeout time.Duration) *CashuMintClient {
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &CashuMintClient{
		httpClient: &http.Client{Timeout: timeout},
		timeout:    timeout,
	}
}

// ParseToken parses a cashuA-encoded token and extracts mint URL, unit, and amount.
func (c *CashuMintClient) ParseToken(raw string) (ParsedToken, error) {
	raw = strings.TrimSpace(raw)

	// cashuA tokens start with "cashuA" prefix
	if !strings.HasPrefix(raw, "cashuA") {
		return ParsedToken{}, errors.New("unsupported token format (expected cashuA)")
	}

	// Decode base64url payload after prefix
	payload := raw[6:] // strip "cashuA"

	// Add padding if needed
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}

	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		// Try standard base64 as fallback
		decoded, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return ParsedToken{}, fmt.Errorf("decode token payload: %w", err)
		}
	}

	// Parse JSON structure
	var tokenData struct {
		Token []struct {
			Mint   string          `json:"mint"`
			Proofs json.RawMessage `json:"proofs"`
		} `json:"token"`
		Unit string `json:"unit"`
		Memo string `json:"memo,omitempty"`
	}

	if err := json.Unmarshal(decoded, &tokenData); err != nil {
		return ParsedToken{}, fmt.Errorf("parse token json: %w", err)
	}

	if len(tokenData.Token) == 0 {
		return ParsedToken{}, errors.New("token contains no mint entries")
	}
	if len(tokenData.Token) > 1 {
		return ParsedToken{}, errors.New("multi-mint tokens not supported")
	}

	entry := tokenData.Token[0]
	if entry.Mint == "" {
		return ParsedToken{}, errors.New("token missing mint URL")
	}

	var proofs []struct {
		Amount int64 `json:"amount"`
	}
	if err := json.Unmarshal(entry.Proofs, &proofs); err != nil {
		return ParsedToken{}, fmt.Errorf("parse token proofs: %w", err)
	}

	// Sum proof amounts
	var total int64
	for _, proof := range proofs {
		total += proof.Amount
	}

	unit := tokenData.Unit
	if unit == "" {
		unit = "sat" // default to sats
	}

	return ParsedToken{
		MintURL:    entry.Mint,
		Unit:       unit,
		AmountSats: total,
		Raw:        raw,
		Proofs:     append(json.RawMessage(nil), entry.Proofs...),
	}, nil
}

// CreateMeltQuote requests a melt quote from the mint for a Lightning invoice.
func (c *CashuMintClient) CreateMeltQuote(ctx context.Context, mintURL, bolt11 string) (MeltQuote, error) {
	// NUT-05: POST /v1/melt/quote/bolt11
	// Request: {"request": "<bolt11>", "unit": "sat"}
	// Response: {"quote": "...", "amount": N, "fee_reserve": M, ...}

	mintURL = strings.TrimRight(mintURL, "/")
	url := mintURL + "/v1/melt/quote/bolt11"

	reqBody, err := json.Marshal(createMeltQuoteRequest{
		Request: bolt11,
		Unit:    "sat",
	})
	if err != nil {
		return MeltQuote{}, fmt.Errorf("marshal quote request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(reqBody)))
	if err != nil {
		return MeltQuote{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return MeltQuote{}, fmt.Errorf("mint request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return MeltQuote{}, fmt.Errorf("mint returned status %d", resp.StatusCode)
	}

	var result struct {
		Quote      string `json:"quote"`
		Amount     int64  `json:"amount"`
		FeeReserve int64  `json:"fee_reserve"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return MeltQuote{}, fmt.Errorf("decode quote response: %w", err)
	}

	return MeltQuote{
		ID:         result.Quote,
		Amount:     result.Amount,
		FeeReserve: result.FeeReserve,
	}, nil
}

// MeltToken melts a token to pay a Lightning invoice via the mint.
func (c *CashuMintClient) MeltToken(ctx context.Context, mintURL string, quote MeltQuote, token ParsedToken) error {
	mintURL = strings.TrimRight(mintURL, "/")
	endpoint := mintURL + "/v1/melt/bolt11"
	beforeSend := func(err error) error { return &MeltSubmissionError{MayHaveSubmitted: false, Err: err} }
	ambiguous := func(err error) error { return &MeltSubmissionError{MayHaveSubmitted: true, Err: err} }

	if quote.ID == "" {
		return beforeSend(errors.New("empty melt quote id"))
	}
	if len(token.Proofs) == 0 {
		return beforeSend(errors.New("no proofs in token"))
	}
	reqBody, err := json.Marshal(meltTokenRequest{Quote: quote.ID, Inputs: token.Proofs})
	if err != nil {
		return beforeSend(fmt.Errorf("marshal melt request: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(reqBody)))
	if err != nil {
		return beforeSend(fmt.Errorf("create request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")

	// From this boundary onward the request may have reached the mint. Go's HTTP
	// errors do not prove that zero bytes were transmitted.
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ambiguous(fmt.Errorf("mint request: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ambiguous(fmt.Errorf("mint returned status %d", resp.StatusCode))
	}
	var result struct {
		Paid  bool   `json:"paid"`
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ambiguous(fmt.Errorf("decode melt response: %w", err))
	}
	state := strings.ToLower(result.State)
	if state != "" && state != "paid" && state != "pending" && state != "unpaid" {
		return ambiguous(fmt.Errorf("mint returned unknown melt state %q", result.State))
	}
	if result.Paid && state != "" && state != "paid" {
		return ambiguous(errors.New("mint returned contradictory paid/state fields"))
	}
	if result.Paid || state == "paid" {
		return nil
	}
	return ambiguous(errors.New("mint did not provide definitive paid evidence"))
}

// LookupMeltQuote reconciles a previously submitted NUT-05 quote without
// re-sending proofs.
func (c *CashuMintClient) LookupMeltQuote(ctx context.Context, mintURL string, quote MeltQuote) (MeltQuoteStatus, error) {
	if quote.ID == "" {
		return MeltQuoteStatus{}, errors.New("empty melt quote id")
	}
	endpoint := strings.TrimRight(mintURL, "/") + "/v1/melt/quote/bolt11/" + url.PathEscape(quote.ID)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return MeltQuoteStatus{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return MeltQuoteStatus{}, fmt.Errorf("lookup melt quote: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return MeltQuoteStatus{}, fmt.Errorf("lookup melt quote returned status %d", resp.StatusCode)
	}
	var result struct {
		Quote      string `json:"quote"`
		Amount     int64  `json:"amount"`
		FeeReserve int64  `json:"fee_reserve"`
		State      string `json:"state"`
		Paid       *bool  `json:"paid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return MeltQuoteStatus{}, fmt.Errorf("decode melt quote status: %w", err)
	}
	if result.Quote != quote.ID {
		return MeltQuoteStatus{}, errors.New("melt quote lookup returned mismatched quote id")
	}
	if result.Amount != quote.Amount {
		return MeltQuoteStatus{}, errors.New("melt quote lookup returned mismatched amount")
	}
	if result.FeeReserve != quote.FeeReserve {
		return MeltQuoteStatus{}, errors.New("melt quote lookup returned mismatched fee reserve")
	}
	state := strings.ToLower(result.State)
	if state == "" && result.Paid != nil && *result.Paid {
		state = "paid"
	}
	if state != "paid" && state != "pending" && state != "unpaid" {
		return MeltQuoteStatus{}, fmt.Errorf("melt quote returned unknown state %q", result.State)
	}
	if result.Paid != nil && ((*result.Paid && state != "paid") || (!*result.Paid && state == "paid")) {
		return MeltQuoteStatus{}, errors.New("melt quote returned contradictory paid/state fields")
	}
	return MeltQuoteStatus{State: state}, nil
}
