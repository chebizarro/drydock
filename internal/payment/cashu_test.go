package payment

import (
	"encoding/base64"
	"encoding/json"
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
