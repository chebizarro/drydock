package ingest

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"drydock/internal/db"

	"fiatjaf.com/nostr"
	"github.com/btcsuite/btcd/btcutil/bech32"
)

func TestZapReceiptValidation(t *testing.T) {
	serviceKey := nostr.GetPublicKey(nostr.Generate())
	zapperKey := nostr.Generate()
	zapperPubkey := nostr.GetPublicKey(zapperKey)
	patchID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	tests := []struct {
		name    string
		service string
		trusted []string
		tags    nostr.Tags
		wantErr string
	}{
		{
			name: "valid amount tag", service: serviceKey.Hex(), trusted: []string{zapperPubkey.Hex()},
			tags: nostr.Tags{{"p", serviceKey.Hex()}, {"e", patchID}, {"amount", "100000"}},
		},
		{
			name: "wrong recipient", service: serviceKey.Hex(),
			tags:    nostr.Tags{{"p", nostr.GetPublicKey(nostr.Generate()).Hex()}, {"e", patchID}, {"amount", "100000"}},
			wantErr: "wrong_recipient",
		},
		{
			name: "invalid event", service: serviceKey.Hex(),
			tags:    nostr.Tags{{"p", serviceKey.Hex()}, {"e", "not-an-event"}, {"amount", "100000"}},
			wantErr: "invalid_patch_event",
		},
		{
			name: "zero amount", service: serviceKey.Hex(),
			tags:    nostr.Tags{{"p", serviceKey.Hex()}, {"e", patchID}, {"amount", "0"}},
			wantErr: "invalid_amount",
		},
		{
			name: "untrusted author", service: serviceKey.Hex(), trusted: []string{nostr.GetPublicKey(nostr.Generate()).Hex()},
			tags:    nostr.Tags{{"p", serviceKey.Hex()}, {"e", patchID}, {"amount", "100000"}},
			wantErr: "untrusted_zapper",
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

	processor := NewProcessor(
		store,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithZapReceipts(serviceKey.Hex(), nil),
	)
	receipt := nostr.Event{
		Kind: 9735, CreatedAt: nostr.Now(),
		Tags: nostr.Tags{{"p", serviceKey.Hex()}, {"e", patchID}, {"amount", "100000"}},
	}
	if err := receipt.Sign(nostr.Generate()); err != nil {
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
