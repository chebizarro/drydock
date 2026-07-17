package payment

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"drydock/internal/db"
	"drydock/internal/repoconfig"

	"fiatjaf.com/nostr"
)

// fakeInvoiceProvider is a test double for InvoiceProvider.
type fakeInvoiceProvider struct {
	mu                 sync.Mutex
	invoices           map[string]InvoiceStatus
	createdStatus      InvoiceStatus
	createInvoiceCalls int
}

func (f *fakeInvoiceProvider) CreateInvoice(ctx context.Context, sats int64, memo string, expiry time.Duration) (Invoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash := sha256.Sum256([]byte(memo))
	id := hex.EncodeToString(hash[:])
	invoice := Invoice{ID: id, Request: "lnbc" + memo, AmountMSats: sats * 1000, ExpiresAt: time.Now().Add(expiry).Unix()}
	status := f.createdStatus
	status.PaymentHash, status.AmountMSats = id, invoice.AmountMSats
	if status.Settled && status.SettledAt == 0 && status.Preimage == "" {
		status.SettledAt = time.Now().Unix()
	}
	f.invoices[id] = status
	f.createInvoiceCalls++
	return invoice, nil
}

func (f *fakeInvoiceProvider) LookupInvoice(ctx context.Context, invoice Invoice) (InvoiceStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.invoices[invoice.ID], nil
}

func (f *fakeInvoiceProvider) setInvoiceStatus(invoiceID string, status InvoiceStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current := f.invoices[invoiceID]
	if status.PaymentHash == "" {
		status.PaymentHash = current.PaymentHash
	}
	if status.AmountMSats == 0 {
		status.AmountMSats = current.AmountMSats
	}
	if status.Settled && status.SettledAt == 0 && status.Preimage == "" {
		status.SettledAt = time.Now().Unix()
	}
	f.invoices[invoiceID] = status
}

func (f *fakeInvoiceProvider) createCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createInvoiceCalls
}

// fakeMintClient is a test double for MintClient.
type fakeMintClient struct {
	mu           sync.Mutex
	tokens       map[string]ParsedToken
	meltedTokens map[string]bool
	quoteStates  map[string]string
	meltErr      error
	meltCalls    int
}

func (f *fakeMintClient) ParseToken(raw string) (ParsedToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tokens[raw]; ok {
		return t, nil
	}
	return ParsedToken{}, errInvalidToken
}

func (f *fakeMintClient) CreateMeltQuote(ctx context.Context, mintURL, bolt11 string) (MeltQuote, error) {
	return MeltQuote{ID: "quote_" + bolt11, Amount: 100, FeeReserve: 5}, nil
}

func (f *fakeMintClient) MeltToken(ctx context.Context, mintURL string, quote MeltQuote, token ParsedToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.meltCalls++
	if f.meltErr != nil {
		return f.meltErr
	}
	if f.meltedTokens[token.Raw] {
		return errTokenSpent
	}
	f.meltedTokens[token.Raw] = true
	f.quoteStates[quote.ID] = "paid"
	return nil
}

func (f *fakeMintClient) LookupMeltQuote(ctx context.Context, mintURL string, quote MeltQuote) (MeltQuoteStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.quoteStates[quote.ID]
	if state == "" {
		state = "pending"
	}
	return MeltQuoteStatus{State: state}, nil
}

func (f *fakeMintClient) meltCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.meltCalls
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
		quoteStates:  make(map[string]string),
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
	if result.Reason != "payment_pending" {
		t.Fatalf("expected reason payment_pending, got %q", result.Reason)
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
	if rec.SettledAmountSats != policy.PriceSats {
		t.Fatalf("settled amount = %d, want %d", rec.SettledAmountSats, policy.PriceSats)
	}
}

func TestAuthorizePatch_CashuMeltReconcilesAfterSettlement(t *testing.T) {
	svc, store := setupTestService(t)
	defer store.Close()

	token := "cashuAreconcile"
	svc.mint.(*fakeMintClient).tokens[token] = ParsedToken{
		MintURL:    "https://mint.example.com",
		Unit:       "sat",
		AmountSats: 110,
		Raw:        token,
	}
	event := nostr.Event{
		ID:     mustParseID("5555555555555555555555555555555555555555555555555555555555555555"),
		PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		Tags:   nostr.Tags{{"cashu", token}},
	}
	policy := repoconfig.PaymentsConfig{Enabled: true, PriceSats: 100}

	result, err := svc.AuthorizePatch(context.Background(), event, "repo/test", policy)
	if err != nil {
		t.Fatalf("first AuthorizePatch: %v", err)
	}
	if result.Allowed || result.Reason != "payment_pending" {
		t.Fatalf("expected recoverable payment_pending denial, got allowed=%v reason=%q", result.Allowed, result.Reason)
	}
	rec, err := store.GetReviewPayment(context.Background(), event.ID.Hex())
	if err != nil {
		t.Fatalf("GetReviewPayment: %v", err)
	}
	if rec.Status != "token_spent" {
		t.Fatalf("expected token_spent pending settlement state, got %q", rec.Status)
	}

	svc.invoice.(*fakeInvoiceProvider).setInvoiceStatus(rec.InvoiceID, InvoiceStatus{Settled: true})
	result, err = svc.AuthorizePatch(context.Background(), event, "repo/test", policy)
	if err != nil {
		t.Fatalf("reconcile AuthorizePatch: %v", err)
	}
	if !result.Allowed || result.AccessKind != "cashu_review" {
		t.Fatalf("expected reconciled cashu_review authorization, got allowed=%v kind=%q reason=%q", result.Allowed, result.AccessKind, result.Reason)
	}
	if svc.mint.(*fakeMintClient).meltCallCount() != 1 {
		t.Fatalf("expected reconciliation not to re-melt token")
	}
}

func TestAuthorizePatch_ConcurrentDuplicateTokenDeniedCleanly(t *testing.T) {
	svc, store := setupTestService(t)
	defer store.Close()

	svc.invoice.(*fakeInvoiceProvider).createdStatus = InvoiceStatus{Settled: true}
	token := "cashuAduplicate"
	svc.mint.(*fakeMintClient).tokens[token] = ParsedToken{
		MintURL:    "https://mint.example.com",
		Unit:       "sat",
		AmountSats: 110,
		Raw:        token,
	}
	policy := repoconfig.PaymentsConfig{Enabled: true, PriceSats: 100}
	events := []nostr.Event{
		{ID: mustParseID("6666666666666666666666666666666666666666666666666666666666666666"), PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), Tags: nostr.Tags{{"cashu", token}}},
		{ID: mustParseID("7777777777777777777777777777777777777777777777777777777777777777"), PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), Tags: nostr.Tags{{"cashu", token}}},
	}

	start := make(chan struct{})
	type outcome struct {
		result AuthorizeResult
		err    error
	}
	out := make(chan outcome, len(events))
	for _, event := range events {
		event := event
		go func() {
			<-start
			res, err := svc.AuthorizePatch(context.Background(), event, "repo/test", policy)
			out <- outcome{result: res, err: err}
		}()
	}
	close(start)

	allowed := 0
	duplicateDenied := 0
	for range events {
		got := <-out
		if got.err != nil {
			t.Fatalf("duplicate reservation should be a clean denial, got error: %v", got.err)
		}
		if got.result.Allowed {
			allowed++
		} else if got.result.Reason == "token_already_used" {
			duplicateDenied++
		} else {
			t.Fatalf("unexpected denial reason %q", got.result.Reason)
		}
	}
	if allowed != 1 || duplicateDenied != 1 {
		t.Fatalf("expected one allowed and one duplicate denial, got allowed=%d duplicateDenied=%d", allowed, duplicateDenied)
	}
	if calls := svc.invoice.(*fakeInvoiceProvider).createCalls(); calls != 1 {
		t.Fatalf("expected only reserved winner to create invoice, got %d calls", calls)
	}
	if calls := svc.mint.(*fakeMintClient).meltCallCount(); calls != 1 {
		t.Fatalf("expected only reserved winner to melt token, got %d calls", calls)
	}
}

func TestAuthorizePatch_SubscriptionRecordsInvoicedAmount(t *testing.T) {
	svc, store := setupTestService(t)
	defer store.Close()

	svc.invoice.(*fakeInvoiceProvider).createdStatus = InvoiceStatus{Settled: true}
	token := "cashuAsubscription"
	svc.mint.(*fakeMintClient).tokens[token] = ParsedToken{
		MintURL:    "https://mint.example.com",
		Unit:       "sat",
		AmountSats: 150,
		Raw:        token,
	}
	event := nostr.Event{
		ID:     mustParseID("8888888888888888888888888888888888888888888888888888888888888888"),
		PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		Tags:   nostr.Tags{{"cashu", token, "subscription"}},
	}
	policy := repoconfig.PaymentsConfig{
		Enabled:               true,
		SubscriptionPriceSats: 100,
		SubscriptionDays:      30,
	}

	result, err := svc.AuthorizePatch(context.Background(), event, "repo/test", policy)
	if err != nil {
		t.Fatalf("AuthorizePatch: %v", err)
	}
	if !result.Allowed || result.AccessKind != "cashu_subscription" {
		t.Fatalf("expected subscription authorization, got allowed=%v kind=%q reason=%q", result.Allowed, result.AccessKind, result.Reason)
	}
	sub, active, err := store.GetActiveSubscription(context.Background(), event.PubKey.Hex(), "repo/test", time.Now().Unix())
	if err != nil {
		t.Fatalf("GetActiveSubscription: %v", err)
	}
	if !active {
		t.Fatal("expected active subscription")
	}
	if sub.PaidAmountSats != 100 {
		t.Fatalf("expected invoiced paid amount 100, got %d", sub.PaidAmountSats)
	}
}

func TestAuthorizePatch_MeltFailureBeforeSendReleasesReservation(t *testing.T) {
	svc, store := setupTestService(t)
	defer store.Close()
	mint := svc.mint.(*fakeMintClient)
	token := "cashuAbefore-send"
	mint.tokens[token] = ParsedToken{MintURL: "https://mint.example.com", Unit: "sat", AmountSats: 110, Raw: token}
	mint.meltErr = &MeltSubmissionError{MayHaveSubmitted: false, Err: errors.New("fault before send")}
	event := nostr.Event{ID: mustParseID("9999999999999999999999999999999999999999999999999999999999999999"), PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), Tags: nostr.Tags{{"cashu", token}}}

	result, err := svc.AuthorizePatch(context.Background(), event, "repo/test", repoconfig.PaymentsConfig{Enabled: true, PriceSats: 100})
	if err != nil {
		t.Fatalf("AuthorizePatch: %v", err)
	}
	if result.Allowed || result.Reason != "payment_not_submitted" {
		t.Fatalf("expected provably-not-submitted denial, got allowed=%v reason=%q", result.Allowed, result.Reason)
	}
	if _, err := store.GetReviewPayment(context.Background(), event.ID.Hex()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("safe pre-send failure should release reservation, got %v", err)
	}
}

func TestAuthorizePatch_AmbiguousMeltPreservedAndReconciledWithoutRemelt(t *testing.T) {
	svc, store := setupTestService(t)
	defer store.Close()
	mint := svc.mint.(*fakeMintClient)
	token := "cashuAafter-send"
	mint.tokens[token] = ParsedToken{MintURL: "https://mint.example.com", Unit: "sat", AmountSats: 110, Raw: token}
	mint.meltErr = &MeltSubmissionError{MayHaveSubmitted: true, Err: errors.New("response lost after send")}
	event := nostr.Event{ID: mustParseID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"), PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), Tags: nostr.Tags{{"cashu", token}}}
	policy := repoconfig.PaymentsConfig{Enabled: true, PriceSats: 100}

	result, err := svc.AuthorizePatch(context.Background(), event, "repo/test", policy)
	if err != nil || result.Allowed || result.Reason != "payment_pending" {
		t.Fatalf("expected preserved ambiguous payment, result=%+v err=%v", result, err)
	}
	rec, err := store.GetReviewPayment(context.Background(), event.ID.Hex())
	if err != nil {
		t.Fatalf("GetReviewPayment: %v", err)
	}
	if rec.MeltState != "submitted" || rec.MeltQuoteID == "" || rec.Status != "pending" {
		t.Fatalf("ambiguous payment evidence not preserved: %+v", rec)
	}

	mint.mu.Lock()
	mint.meltErr = nil
	mint.quoteStates[rec.MeltQuoteID] = "paid"
	mint.mu.Unlock()
	svc.invoice.(*fakeInvoiceProvider).setInvoiceStatus(rec.InvoiceID, InvoiceStatus{Settled: true})
	result, err = svc.AuthorizePatch(context.Background(), event, "repo/test", policy)
	if err != nil || !result.Allowed {
		t.Fatalf("reconciliation failed: result=%+v err=%v", result, err)
	}
	if calls := mint.meltCallCount(); calls != 1 {
		t.Fatalf("reconciliation re-melted token: calls=%d", calls)
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
