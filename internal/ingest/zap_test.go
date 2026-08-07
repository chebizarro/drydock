package ingest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"drydock/internal/db"
	"drydock/internal/revieworder"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
	"github.com/btcsuite/btcd/btcutil/bech32"
)

type allowAllMonitoring struct{}

func (allowAllMonitoring) Contains(string) bool { return true }

// testBolt11 returns a syntactically decodable 1u (100,000 msat) invoice.
func testBolt11(t *testing.T) string {
	t.Helper()
	invoice, err := bech32.Encode("lnbc1u", []byte{0, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	return invoice
}

// signedZapRequest builds and signs a kind-9734 zap request.
func signedZapRequest(t *testing.T, payerKey nostr.SecretKey, tags nostr.Tags) string {
	t.Helper()
	request := nostr.Event{Kind: 9734, CreatedAt: nostr.Now(), Tags: tags}
	if err := request.Sign(payerKey); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestZapReceiptValidation(t *testing.T) {
	serviceKey := nostr.GetPublicKey(nostr.Generate())
	zapperKey := nostr.Generate()
	zapperPubkey := nostr.GetPublicKey(zapperKey)
	payerKey := nostr.Generate()
	patchID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	service := serviceKey.Hex()
	trusted := []string{zapperPubkey.Hex()}
	bolt11 := testBolt11(t)
	validRequest := signedZapRequest(t, payerKey, nostr.Tags{{"p", service}, {"e", patchID}, {"amount", "100000"}})

	unsignedRequest, err := json.Marshal(nostr.Event{Kind: 9734, Tags: nostr.Tags{{"p", service}, {"e", patchID}}})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		service string
		trusted []string
		tags    nostr.Tags
		wantErr string
	}{
		{
			name: "valid receipt", service: service, trusted: trusted,
			tags: nostr.Tags{{"p", service}, {"e", patchID}, {"bolt11", bolt11}, {"description", validRequest}},
		},
		{
			name: "amount tag without invoice is rejected", service: service, trusted: trusted,
			tags:    nostr.Tags{{"p", service}, {"e", patchID}, {"amount", "100000"}, {"description", validRequest}},
			wantErr: "bolt11_missing",
		},
		{
			name: "empty trusted zapper allowlist fails closed", service: service,
			tags:    nostr.Tags{{"p", service}, {"e", patchID}, {"bolt11", bolt11}, {"description", validRequest}},
			wantErr: "no_trusted_zappers_configured",
		},
		{
			name: "wrong recipient", service: service, trusted: trusted,
			tags:    nostr.Tags{{"p", nostr.GetPublicKey(nostr.Generate()).Hex()}, {"e", patchID}, {"bolt11", bolt11}, {"description", validRequest}},
			wantErr: "wrong_recipient",
		},
		{
			name: "invalid event", service: service, trusted: trusted,
			tags:    nostr.Tags{{"p", service}, {"e", "not-an-event"}, {"bolt11", bolt11}, {"description", validRequest}},
			wantErr: "invalid_patch_event",
		},
		{
			name: "zero amount", service: service, trusted: trusted,
			tags:    nostr.Tags{{"p", service}, {"e", patchID}, {"amount", "0"}, {"bolt11", bolt11}, {"description", validRequest}},
			wantErr: "invalid_amount",
		},
		{
			name: "conflicting amount tag and invoice", service: service, trusted: trusted,
			tags:    nostr.Tags{{"p", service}, {"e", patchID}, {"amount", "999"}, {"bolt11", bolt11}, {"description", validRequest}},
			wantErr: "conflicting_amount",
		},
		{
			name: "untrusted author", service: service, trusted: []string{nostr.GetPublicKey(nostr.Generate()).Hex()},
			tags:    nostr.Tags{{"p", service}, {"e", patchID}, {"bolt11", bolt11}, {"description", validRequest}},
			wantErr: "untrusted_zapper",
		},
		{
			name: "missing description", service: service, trusted: trusted,
			tags:    nostr.Tags{{"p", service}, {"e", patchID}, {"bolt11", bolt11}},
			wantErr: "description_missing",
		},
		{
			name: "unsigned zap request", service: service, trusted: trusted,
			tags:    nostr.Tags{{"p", service}, {"e", patchID}, {"bolt11", bolt11}, {"description", string(unsignedRequest)}},
			wantErr: "zap_request_unverified",
		},
		{
			name: "zap request names different patch", service: service, trusted: trusted,
			tags: nostr.Tags{{"p", service}, {"e", patchID}, {"bolt11", bolt11},
				{"description", signedZapRequest(t, payerKey, nostr.Tags{{"p", service}, {"e", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}})}},
			wantErr: "zap_request_event_mismatch",
		},
		{
			name: "zap request names different recipient", service: service, trusted: trusted,
			tags: nostr.Tags{{"p", service}, {"e", patchID}, {"bolt11", bolt11},
				{"description", signedZapRequest(t, payerKey, nostr.Tags{{"p", nostr.GetPublicKey(nostr.Generate()).Hex()}, {"e", patchID}})}},
			wantErr: "zap_request_recipient_mismatch",
		},
		{
			name: "zap request amount mismatch", service: service, trusted: trusted,
			tags: nostr.Tags{{"p", service}, {"e", patchID}, {"bolt11", bolt11},
				{"description", signedZapRequest(t, payerKey, nostr.Tags{{"p", service}, {"e", patchID}, {"amount", "5"}})}},
			wantErr: "zap_request_amount_mismatch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor := NewProcessor(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), WithZapReceipts(tt.service, tt.trusted))
			event := nostr.Event{Kind: 9735, PubKey: zapperPubkey, Tags: tt.tags}
			receipt, err := processor.validateZapReceipt(event)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if receipt.PatchEventID != patchID || receipt.AmountMSat != 100_000 {
					t.Fatalf("unexpected receipt: %+v", receipt)
				}
				if receipt.PayerPubkey != nostr.GetPublicKey(payerKey).Hex() {
					t.Fatalf("payer = %q, want verified zap request author", receipt.PayerPubkey)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestZapAmountFromBolt11(t *testing.T) {
	invoice, err := bech32.Encode("lnbc1u", []byte{0, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	amount, err := zapAmountMSat(nostr.Event{Tags: nostr.Tags{{"bolt11", invoice}}})
	if err != nil {
		t.Fatal(err)
	}
	if amount != 100_000 {
		t.Fatalf("amount = %d msat, want 100000", amount)
	}
}

func TestZapReceiptLateRequeuesPaymentBlockedReview(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	serviceKey := nostr.GetPublicKey(nostr.Generate())
	const patchID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const repoID = "owner:repo"
	acquired, err := store.BeginReview(ctx, patchID, repoID)
	if err != nil || !acquired {
		t.Fatalf("BeginReview = %v, %v", acquired, err)
	}
	if err := store.MarkReviewFailed(ctx, patchID, repoID, "payment_blocked:no_payment"); err != nil {
		t.Fatal(err)
	}

	zapperKey := nostr.Generate()
	payerKey := nostr.Generate()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	orders := revieworder.New(revieworder.Config{}, store, scope.Matcher{}, allowAllMonitoring{}, nil, nil, logger)
	processor := NewProcessor(
		store,
		logger,
		WithZapReceipts(serviceKey.Hex(), []string{nostr.GetPublicKey(zapperKey).Hex()}),
		WithReviewOrders(orders),
	)
	receipt := nostr.Event{
		Kind: 9735, CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"p", serviceKey.Hex()}, {"e", patchID}, {"bolt11", testBolt11(t)},
			{"description", signedZapRequest(t, payerKey, nostr.Tags{{"p", serviceKey.Hex()}, {"e", patchID}, {"amount", "100000"}})},
		},
	}
	if err := receipt.Sign(zapperKey); err != nil {
		t.Fatal(err)
	}
	if err := processor.ProcessEvent(ctx, receipt, "wss://relay.test"); err != nil {
		t.Fatal(err)
	}

	select {
	case task := <-processor.ReviewQueue:
		if task.PatchEventID != patchID || task.RepoID != repoID {
			t.Fatalf("unexpected queued task: %+v", task)
		}
	default:
		t.Fatal("late zap receipt did not re-enqueue review")
	}
	status, err := store.GetReviewStatus(ctx, patchID, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if status != "reviewing" {
		t.Fatalf("status = %q, want reviewing", status)
	}
}
