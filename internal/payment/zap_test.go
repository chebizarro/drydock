package payment

import (
	"context"
	"testing"
	"time"

	"drydock/internal/db"
	"drydock/internal/repoconfig"

	"fiatjaf.com/nostr"
)

func TestAuthorizePatchViaZap(t *testing.T) {
	tests := []struct {
		name         string
		receiptPatch string
		amountMSat   int64
		acceptZaps   *bool
		wantAllowed  bool
	}{
		{name: "covers price", receiptPatch: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", amountMSat: 100_000, wantAllowed: true},
		{name: "insufficient", receiptPatch: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", amountMSat: 99_999, wantAllowed: false},
		{name: "wrong event", receiptPatch: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", amountMSat: 100_000, wantAllowed: false},
		{name: "disabled by policy", receiptPatch: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", amountMSat: 100_000, acceptZaps: boolPointer(false), wantAllowed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, store := setupTestService(t)
			defer store.Close()
			_, _, err := store.InsertZapReceiptAndClaimBlockedReviews(context.Background(), db.ZapReceiptRecord{
				EventID:       "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
				PatchEventID:  tt.receiptPatch,
				PayerPubkey:   "payer",
				ReceiptAuthor: "zapper",
				AmountMSat:    tt.amountMSat,
				CreatedAt:     time.Now().Unix(),
			})
			if err != nil {
				t.Fatal(err)
			}
			event := nostr.Event{
				ID:     mustParseID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
				PubKey: mustParsePubKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
			}
			freeMaintainers := false
			policy := repoconfig.PaymentsConfig{
				Enabled: true, PriceSats: 100, FreeForMaintainers: &freeMaintainers,
				AcceptZaps: tt.acceptZaps,
			}
			result, err := svc.AuthorizePatch(context.Background(), event, "owner:repo", policy)
			if err != nil {
				t.Fatal(err)
			}
			if result.Allowed != tt.wantAllowed {
				t.Fatalf("Allowed = %v, want %v (reason %q)", result.Allowed, tt.wantAllowed, result.Reason)
			}
			if tt.wantAllowed && result.AccessKind != "zap" {
				t.Fatalf("AccessKind = %q, want zap", result.AccessKind)
			}
		})
	}
}

func boolPointer(v bool) *bool { return &v }
