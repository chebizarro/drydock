package revieworder

import (
	"context"
	"testing"
	"time"

	"drydock/internal/contextvm"
	"drydock/internal/db"
	"drydock/internal/payment"
	"drydock/internal/ratelimit"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
)

type integrationMonitoring bool

func (m integrationMonitoring) Contains(string) bool { return bool(m) }

func TestIntegrationPaidOrderBypassesMonitoringWithinStaticCeiling(t *testing.T) {
	loader := orderConfigLoader{config: []byte("payments:\n  enabled: true\n  price_sats: 100\n")}
	authorizer := &orderPaymentAuthorizer{result: payment.AuthorizeResult{
		Allowed: true, AccessKind: payment.AccessCashuReview,
	}}
	f := newOrderFixture(t, true, []string{""}, loader, authorizer, 20)
	f.service = New(
		Config{QueueSize: 4},
		f.store,
		scope.NewMatcher([]string{f.repoID}, nil),
		integrationMonitoring(false),
		loader,
		authorizer,
		testLogger(),
	).WithRateLimiter(ratelimit.New(ratelimit.Config{
		Window: time.Hour, MaxRequests: 20, KeyPrefix: "review-order-integration:",
	}, ratelimit.NewMemoryStore()))
	f.handler = NewHandler(f.service, f.servicePK.Hex(), testLogger())

	result, rpcErr := f.request(t, "paid-unmonitored", ReviewOrderParams{
		PatchEventID: f.patch.ID.Hex(),
		RepoAddr:     f.repoAddress,
	}, &f.repoAddress)
	if rpcErr != nil {
		t.Fatalf("paid order outside monitoring was rejected: %+v", rpcErr)
	}
	accepted := result.(ReviewOrderAccepted)
	if !accepted.Accepted || accepted.State != "queued" || accepted.RepoAddr != f.repoAddress {
		t.Fatalf("accepted order = %+v", accepted)
	}
	if authorizer.calls != 1 {
		t.Fatalf("payment authorizer calls = %d, want 1", authorizer.calls)
	}
	select {
	case task := <-f.service.Queue():
		if task.Invocation != db.ReviewInvocationContextVM || task.OrderID != "paid-unmonitored" {
			t.Fatalf("queued task = %+v", task)
		}
	default:
		t.Fatal("accepted paid order was not queued")
	}
}

func TestIntegrationStaticCeilingRejectsOtherwisePaidOrder(t *testing.T) {
	loader := orderConfigLoader{config: []byte("payments:\n  enabled: true\n  price_sats: 100\n")}
	authorizer := &orderPaymentAuthorizer{result: payment.AuthorizeResult{
		Allowed: true, AccessKind: payment.AccessCashuReview,
	}}
	f := newOrderFixture(t, true, []string{""}, loader, authorizer, 20)
	f.service = New(
		Config{QueueSize: 4},
		f.store,
		scope.NewMatcher([]string{"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff:other"}, nil),
		integrationMonitoring(true),
		loader,
		authorizer,
		testLogger(),
	).WithRateLimiter(ratelimit.New(ratelimit.Config{
		Window: time.Hour, MaxRequests: 20, KeyPrefix: "review-order-ceiling-integration:",
	}, ratelimit.NewMemoryStore()))
	f.handler = NewHandler(f.service, f.servicePK.Hex(), testLogger())

	_, rpcErr := f.request(t, "ceiling-reject", ReviewOrderParams{
		PatchEventID: f.patch.ID.Hex(),
		RepoAddr:     f.repoAddress,
	}, &f.repoAddress)
	if rpcErr == nil || rpcErr.Code != contextvm.ErrorUnauthorized {
		t.Fatalf("static-ceiling error = %+v, want unauthorized", rpcErr)
	}
	if authorizer.calls != 0 {
		t.Fatalf("payment was consulted before the static ceiling; calls = %d", authorizer.calls)
	}
	requester := nostr.GetPublicKey(f.requesterKey).Hex()
	if _, ok, err := f.store.GetReviewOrder(context.Background(), requester, "ceiling-reject"); err != nil || ok {
		t.Fatalf("rejected order receipt: ok=%v err=%v", ok, err)
	}
}
