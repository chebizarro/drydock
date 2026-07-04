package payment

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseToken_ValidToken(t *testing.T) {
	// Construct a valid cashuA token
	tokenData := map[string]any{
		"token": []map[string]any{
			{
				"mint": "https://mint.example.com",
				"proofs": []map[string]any{
					{"amount": 64, "id": "abc", "secret": "xxx", "C": "yyy"},
					{"amount": 32, "id": "abc", "secret": "zzz", "C": "www"},
				},
			},
		},
		"unit": "sat",
	}
	jsonBytes, _ := json.Marshal(tokenData)
	encoded := base64.URLEncoding.EncodeToString(jsonBytes)
	token := "cashuA" + strings.TrimRight(encoded, "=")

	client := NewCashuMintClient(0)
	parsed, err := client.ParseToken(token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if parsed.MintURL != "https://mint.example.com" {
		t.Errorf("expected mint URL, got %q", parsed.MintURL)
	}
	if parsed.Unit != "sat" {
		t.Errorf("expected unit=sat, got %q", parsed.Unit)
	}
	if parsed.AmountSats != 96 {
		t.Errorf("expected amount=96, got %d", parsed.AmountSats)
	}

	var proofs []map[string]any
	if err := json.Unmarshal(parsed.Proofs, &proofs); err != nil {
		t.Fatalf("failed to parse stored proofs: %v", err)
	}
	if len(proofs) != 2 {
		t.Errorf("expected 2 proofs, got %d", len(proofs))
	}
}

func TestCreateMeltQuote_MarshalsRequestBody(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/melt/quote/bolt11" {
			t.Fatalf("expected quote path, got %q", r.URL.Path)
		}
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"quote":"quote-1","amount":10,"fee_reserve":1}`))
	}))
	defer server.Close()

	client := NewCashuMintClient(0)
	invoice := "lnbc\"special"
	if _, err := client.CreateMeltQuote(context.Background(), server.URL, invoice); err != nil {
		t.Fatalf("CreateMeltQuote: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("request body is not valid JSON: %v; body=%s", err, body)
	}
	if got["request"] != invoice {
		t.Errorf("expected request %q, got %q", invoice, got["request"])
	}
	if got["unit"] != "sat" {
		t.Errorf("expected unit sat, got %q", got["unit"])
	}
}

func TestMeltToken_MarshalsRequestBody(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/melt/bolt11" {
			t.Fatalf("expected melt path, got %q", r.URL.Path)
		}
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"paid":true}`))
	}))
	defer server.Close()

	client := NewCashuMintClient(0)
	quoteID := "quote\"special"
	proofs := json.RawMessage(`[{"amount":10,"id":"abc","secret":"s","C":"c"}]`)
	if err := client.MeltToken(context.Background(), server.URL, MeltQuote{ID: quoteID}, ParsedToken{Proofs: proofs}); err != nil {
		t.Fatalf("MeltToken: %v", err)
	}

	var got struct {
		Quote  string           `json:"quote"`
		Inputs []map[string]any `json:"inputs"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("request body is not valid JSON: %v; body=%s", err, body)
	}
	if got.Quote != quoteID {
		t.Errorf("expected quote %q, got %q", quoteID, got.Quote)
	}
	if len(got.Inputs) != 1 || got.Inputs[0]["id"] != "abc" {
		t.Errorf("unexpected inputs: %#v", got.Inputs)
	}
}

func TestParseToken_InvalidPrefix(t *testing.T) {
	client := NewCashuMintClient(0)
	_, err := client.ParseToken("cashuB123456")
	if err == nil {
		t.Error("expected error for invalid prefix")
	}
}

func TestParseToken_MultiMintRejected(t *testing.T) {
	tokenData := map[string]any{
		"token": []map[string]any{
			{"mint": "https://mint1.example.com", "proofs": []map[string]any{}},
			{"mint": "https://mint2.example.com", "proofs": []map[string]any{}},
		},
		"unit": "sat",
	}
	jsonBytes, _ := json.Marshal(tokenData)
	encoded := base64.URLEncoding.EncodeToString(jsonBytes)
	token := "cashuA" + strings.TrimRight(encoded, "=")

	client := NewCashuMintClient(0)
	_, err := client.ParseToken(token)
	if err == nil {
		t.Error("expected error for multi-mint token")
	}
	if !strings.Contains(err.Error(), "multi-mint") {
		t.Errorf("expected multi-mint error, got %v", err)
	}
}

func TestParseToken_EmptyToken(t *testing.T) {
	tokenData := map[string]any{
		"token": []map[string]any{},
		"unit":  "sat",
	}
	jsonBytes, _ := json.Marshal(tokenData)
	encoded := base64.URLEncoding.EncodeToString(jsonBytes)
	token := "cashuA" + strings.TrimRight(encoded, "=")

	client := NewCashuMintClient(0)
	_, err := client.ParseToken(token)
	if err == nil {
		t.Error("expected error for empty token")
	}
}

func TestParseToken_DefaultUnit(t *testing.T) {
	tokenData := map[string]any{
		"token": []map[string]any{
			{"mint": "https://mint.example.com", "proofs": []map[string]any{{"amount": 10}}},
		},
		// no unit field
	}
	jsonBytes, _ := json.Marshal(tokenData)
	encoded := base64.URLEncoding.EncodeToString(jsonBytes)
	token := "cashuA" + strings.TrimRight(encoded, "=")

	client := NewCashuMintClient(0)
	parsed, err := client.ParseToken(token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.Unit != "sat" {
		t.Errorf("expected default unit=sat, got %q", parsed.Unit)
	}
}
