package payment

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"drydock/internal/db"
	"drydock/internal/repoconfig"

	"fiatjaf.com/nostr"
)

// fakeInvoiceProvider is a test double for InvoiceProvider.
type fakeInvoiceProvider struct {
	invoices      map[string]InvoiceStatus
	createdStatus InvoiceStatus
}

func (f *fakeInvoiceProvider) CreateInvoice(ctx context.Context, sats int64, memo string, expiry time.Duration) (Invoice, error) {
	id := "inv_" + memo
	f.invoices[id] = f.createdStatus
	return Invoice{ID: id, Request: "lnbc" + memo}, nil
}

func (f *fakeInvoiceProvider) LookupInvoice(ctx context.Context, invoiceID string) (InvoiceStatus, error) {
	return f.invoices[invoiceID], nil
}

// fakeMintClient is a test double for MintClient.
type fakeMintClient struct {
	tokens       map[string]ParsedToken
	meltedTokens map[string]bool
}

func (f *fakeMintClient) ParseToken(raw string) (ParsedToken, error) {
	if t, ok := f.tokens[raw]; ok {
		return t, nil
	}
	return ParsedToken{}, errInvalidToken
}

func (f *fakeMintClient) CreateMeltQuote(ctx context.Context, mintURL, bolt11 string) (MeltQuote, error) {
	return MeltQuote{ID: "quote_" + bolt11, Amount: 100, FeeReserve: 5}, nil
}

func (f *fakeMintClient) MeltToken(ctx context.Context, mintURL string, quote MeltQuote, token ParsedToken) error {
	if f.meltedTokens[token.Raw] {
		return errTokenSpent
	}
	f.meltedTokens[token.Raw] = true
	return nil
}

var errInvalidToken = &tokenError{"invalid_token"}
var errTokenSpent = &tokenError{"token_spent"}

type tokenError struct{ reason string }

func (e *tokenError) Error() string { return e.reason }

func setupTestService(t *testing.T) (*Service, *db.Store) {
	ctx := context.Background()
	store, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	invoice := &fakeInvoiceProvider{invoices: make(map[string]InvoiceStatus)}
	mint := &fakeMintClient{
		tokens:       make(map[string]ParsedToken),
		meltedTokens: make(map[string]bool),
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(Config{
		TrustedMints:  []string{"https://mint.example.com"},
		Timeout:       time.Second,
		InvoiceExpiry: time.Minute,
	}, store, invoice, mint, logger)

	return svc, store
}

func TestAuthorizePatch_PaymentsDisabled(t *testing.T) {
	svc, store := setupTestService(t)
	defer store.Close()

	event := nostr.Event{
		ID:     mustParseID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	}
	policy := repoconfig.PaymentsConfig{Enabled: false}

	result, err := svc.AuthorizePatch(context.Background(), event, "repo/test", policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("expected allowed when payments disabled")
	}
}

func TestAuthorizePatch_FreeTier(t *testing.T) {
	svc, store := setupTestService(t)
	defer store.Close()

	event := nostr.Event{
		ID:     mustParseID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	}
	policy := repoconfig.PaymentsConfig{
		Enabled:           true,
		PriceSats:         100,
		FreeReviewsPerDay: 1,
	}

	result, err := svc.AuthorizePatch(context.Background(), event, "repo/test", policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("expected allowed via free tier")
	}
	if result.AccessKind != "free_tier" {
		t.Errorf("expected access_kind=free_tier, got %q", result.AccessKind)
	}
}

func TestAuthorizePatch_FreeTierExhausted(t *testing.T) {
	svc, store := setupTestService(t)
	defer store.Close()

	policy := repoconfig.PaymentsConfig{
		Enabled:           true,
		PriceSats:         100,
		FreeReviewsPerDay: 1,
	}

	// First review uses free tier
	event1 := nostr.Event{
		ID:     mustParseID("1111111111111111111111111111111111111111111111111111111111111111"),
		PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	}
	result1, _ := svc.AuthorizePatch(context.Background(), event1, "repo/test", policy)
	if !result1.Allowed {
		t.Fatal("first review should be allowed")
	}

	// Second review should be denied (same author, same day)
	event2 := nostr.Event{
		ID:     mustParseID("2222222222222222222222222222222222222222222222222222222222222222"),
		PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	}
	result2, _ := svc.AuthorizePatch(context.Background(), event2, "repo/test", policy)
	if result2.Allowed {
		t.Error("second review should be denied (free tier exhausted)")
	}
	if result2.Reason != "no_payment" {
		t.Errorf("expected reason=no_payment, got %q", result2.Reason)
	}
}

func TestAuthorizePatch_IdempotentAuthorization(t *testing.T) {
	svc, store := setupTestService(t)
	defer store.Close()

	event := nostr.Event{
		ID:     mustParseID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	}
	policy := repoconfig.PaymentsConfig{
		Enabled:           true,
		PriceSats:         100,
		FreeReviewsPerDay: 1,
	}

	// First call
	result1, _ := svc.AuthorizePatch(context.Background(), event, "repo/test", policy)
	if !result1.Allowed {
		t.Fatal("first authorization should succeed")
	}

	// Second call with same event should return same result (idempotent)
	result2, _ := svc.AuthorizePatch(context.Background(), event, "repo/test", policy)
	if !result2.Allowed {
		t.Error("second authorization should still succeed (idempotent)")
	}
}

func TestAuthorizePatch_CashuMeltRequiresSettledInvoice(t *testing.T) {
	svc, store := setupTestService(t)
	defer store.Close()

	token := "cashuAunsettled"
	svc.mint.(*fakeMintClient).tokens[token] = ParsedToken{
		MintURL:    "https://mint.example.com",
		Unit:       "sat",
		AmountSats: 110,
		Raw:        token,
	}

	event := nostr.Event{
		ID:     mustParseID("3333333333333333333333333333333333333333333333333333333333333333"),
		PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		Tags:   nostr.Tags{{"cashu", token}},
	}
	policy := repoconfig.PaymentsConfig{
		Enabled:   true,
		PriceSats: 100,
	}

	result, err := svc.AuthorizePatch(context.Background(), event, "repo/test", policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected unsettled invoice lookup to deny access")
	}
	if result.Reason != "invoice_not_settled" {
		t.Fatalf("expected reason invoice_not_settled, got %q", result.Reason)
	}

	rec, err := store.GetReviewPayment(context.Background(), event.ID.Hex())
	if err != nil {
		t.Fatalf("GetReviewPayment: %v", err)
	}
	if rec.Status != "token_spent" {
		t.Fatalf("expected recoverable token_spent state, got %q", rec.Status)
	}
	if rec.AccessKind != "" {
		t.Fatalf("expected no access kind for unsettled payment, got %q", rec.AccessKind)
	}
}

func TestAuthorizePatch_CashuMeltAuthorizesWhenInvoiceSettled(t *testing.T) {
	svc, store := setupTestService(t)
	defer store.Close()

	svc.invoice.(*fakeInvoiceProvider).createdStatus = InvoiceStatus{Settled: true}
	token := "cashuAsettled"
	svc.mint.(*fakeMintClient).tokens[token] = ParsedToken{
		MintURL:    "https://mint.example.com",
		Unit:       "sat",
		AmountSats: 110,
		Raw:        token,
	}

	event := nostr.Event{
		ID:     mustParseID("4444444444444444444444444444444444444444444444444444444444444444"),
		PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		Tags:   nostr.Tags{{"cashu", token}},
	}
	policy := repoconfig.PaymentsConfig{
		Enabled:   true,
		PriceSats: 100,
	}

	result, err := svc.AuthorizePatch(context.Background(), event, "repo/test", policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("expected settled invoice to authorize access, reason=%q", result.Reason)
	}
	if result.AccessKind != "cashu_review" {
		t.Fatalf("expected cashu_review access, got %q", result.AccessKind)
	}

	rec, err := store.GetReviewPayment(context.Background(), event.ID.Hex())
	if err != nil {
		t.Fatalf("GetReviewPayment: %v", err)
	}
	if rec.Status != "authorized" {
		t.Fatalf("expected authorized payment, got %q", rec.Status)
	}
	if rec.AccessKind != "cashu_review" {
		t.Fatalf("expected cashu_review record, got %q", rec.AccessKind)
	}
}

func TestExtractPaymentTag_NoTag(t *testing.T) {
	event := nostr.Event{Tags: nostr.Tags{}}
	_, _, err := extractPaymentTag(event)
	if err == nil || err.Error() != "no_payment" {
		t.Errorf("expected no_payment error, got %v", err)
	}
}

func TestExtractPaymentTag_MultipleTags(t *testing.T) {
	event := nostr.Event{
		Tags: nostr.Tags{
			{"cashu", "token1"},
			{"cashu", "token2"},
		},
	}
	_, _, err := extractPaymentTag(event)
	if err == nil || err.Error() != "multiple_cashu_tags" {
		t.Errorf("expected multiple_cashu_tags error, got %v", err)
	}
}

func TestExtractPaymentTag_ValidReview(t *testing.T) {
	event := nostr.Event{
		Tags: nostr.Tags{
			{"cashu", "cashuAtoken123"},
		},
	}
	token, mode, err := extractPaymentTag(event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "cashuAtoken123" {
		t.Errorf("expected token=cashuAtoken123, got %q", token)
	}
	if mode != "review" {
		t.Errorf("expected mode=review, got %q", mode)
	}
}

func TestExtractPaymentTag_ValidSubscription(t *testing.T) {
	event := nostr.Event{
		Tags: nostr.Tags{
			{"cashu", "cashuAtoken123", "subscription"},
		},
	}
	token, mode, err := extractPaymentTag(event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "cashuAtoken123" {
		t.Errorf("expected token, got %q", token)
	}
	if mode != "subscription" {
		t.Errorf("expected mode=subscription, got %q", mode)
	}
}

func mustParseID(hex string) nostr.ID {
	var id nostr.ID
	b := mustDecodeHex(hex)
	copy(id[:], b)
	return id
}

func mustParsePubKey(hex string) nostr.PubKey {
	var pk nostr.PubKey
	b := mustDecodeHex(hex)
	copy(pk[:], b)
	return pk
}

func mustDecodeHex(s string) []byte {
	b := make([]byte, len(s)/2)
	for i := 0; i < len(s)/2; i++ {
		b[i] = hexByte(s[2*i])<<4 | hexByte(s[2*i+1])
	}
	return b
}

func hexByte(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	default:
		return 0
	}
}
