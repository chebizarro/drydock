package payment

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// CashuMintClient implements MintClient using HTTP requests to Cashu mints.
type CashuMintClient struct {
	httpClient *http.Client
	timeout    time.Duration
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
			Mint   string `json:"mint"`
			Proofs []struct {
				Amount int64  `json:"amount"`
				ID     string `json:"id"`
				Secret string `json:"secret"`
				C      string `json:"C"`
			} `json:"proofs"`
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

	// Sum proof amounts
	var total int64
	for _, proof := range entry.Proofs {
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
	}, nil
}

// CreateMeltQuote requests a melt quote from the mint for a Lightning invoice.
func (c *CashuMintClient) CreateMeltQuote(ctx context.Context, mintURL, bolt11 string) (MeltQuote, error) {
	// NUT-05: POST /v1/melt/quote/bolt11
	// Request: {"request": "<bolt11>", "unit": "sat"}
	// Response: {"quote": "...", "amount": N, "fee_reserve": M, ...}

	mintURL = strings.TrimRight(mintURL, "/")
	url := mintURL + "/v1/melt/quote/bolt11"

	reqBody := fmt.Sprintf(`{"request":"%s","unit":"sat"}`, bolt11)

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(reqBody))
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
	// NUT-05: POST /v1/melt/bolt11
	// Request: {"quote": "...", "inputs": [...proofs...]}
	// Response: {"paid": true, ...}

	mintURL = strings.TrimRight(mintURL, "/")
	url := mintURL + "/v1/melt/bolt11"

	// Re-parse token to get proofs
	payload := token.Raw[6:]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	decoded, _ := base64.URLEncoding.DecodeString(payload)
	if decoded == nil {
		decoded, _ = base64.StdEncoding.DecodeString(payload)
	}

	var tokenData struct {
		Token []struct {
			Proofs json.RawMessage `json:"proofs"`
		} `json:"token"`
	}
	json.Unmarshal(decoded, &tokenData)

	if len(tokenData.Token) == 0 {
		return errors.New("no proofs in token")
	}

	reqBody := fmt.Sprintf(`{"quote":"%s","inputs":%s}`, quote.ID, string(tokenData.Token[0].Proofs))

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mint request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mint returned status %d", resp.StatusCode)
	}

	var result struct {
		Paid bool `json:"paid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode melt response: %w", err)
	}

	if !result.Paid {
		return errors.New("melt not paid")
	}

	return nil
}
