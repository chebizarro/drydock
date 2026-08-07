package revieworder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"drydock/internal/contextvm"
	"drydock/internal/db"
	"drydock/internal/payment"
	"drydock/internal/ratelimit"
	"drydock/internal/repoconfig"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
)

type orderConfigLoader struct {
	config []byte
}

func (l orderConfigLoader) LoadBaseRepoConfig(context.Context, string) ([]byte, error) {
	return l.config, nil
}

type orderPaymentAuthorizer struct {
	result payment.AuthorizeResult
	calls  int
}

func (a *orderPaymentAuthorizer) AuthorizePatch(context.Context, nostr.Event, string, repoconfig.PaymentsConfig) (payment.AuthorizeResult, error) {
	a.calls++
	return a.result, nil
}

type orderFixture struct {
	ctx          context.Context
	store        *db.Store
	service      *Service
	handler      *Handler
	serviceKey   nostr.SecretKey
	servicePK    nostr.PubKey
	requesterKey nostr.SecretKey
	repoAddress  string
	repoID       string
	patch        nostr.Event
}

func newOrderFixture(t *testing.T, announcement bool, patchAddresses []string, loader RepositoryConfigLoader, authorizer PaymentAuthorizer, limit int) *orderFixture {
	t.Helper()
	ctx := context.Background()
	store := testStore(t, ctx, filepath.Join(t.TempDir(), "orders.db"))
	store.DB().SetMaxOpenConns(1)
	ownerKey := nostr.Generate()
	owner := nostr.GetPublicKey(ownerKey)
	repoID := owner.Hex() + ":repo"
	repoAddress := "30617:" + repoID
	if announcement {
		event := nostr.Event{
			Kind:      30617,
			CreatedAt: nostr.Now(),
			Tags:      nostr.Tags{{"d", "repo"}, {"clone", "https://example.com/repo.git"}},
		}
		if err := event.Sign(ownerKey); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertRepositoryAnnouncement(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	patch := nostr.Event{Kind: 1617, CreatedAt: nostr.Now(), Content: "diff"}
	for _, address := range patchAddresses {
		if address == "" {
			address = repoAddress
		}
		patch.Tags = append(patch.Tags, nostr.Tag{"a", address})
	}
	if err := patch.Sign(nostr.Generate()); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertPatchEvent(ctx, patch); err != nil {
		t.Fatal(err)
	}
	if loader == nil {
		loader = orderConfigLoader{}
	}
	service := New(Config{QueueSize: 32}, store, scope.Matcher{}, nil, loader, authorizer, testLogger()).
		WithRateLimiter(ratelimit.New(ratelimit.Config{
			Window: time.Hour, MaxRequests: limit, KeyPrefix: "review-order-test:",
		}, ratelimit.NewMemoryStore()))
	serviceKey := nostr.Generate()
	servicePK := nostr.GetPublicKey(serviceKey)
	return &orderFixture{
		ctx: ctx, store: store, service: service,
		handler:    NewHandler(service, servicePK.Hex(), slog.New(slog.NewTextHandler(io.Discard, nil))),
		serviceKey: serviceKey, servicePK: servicePK,
		requesterKey: nostr.Generate(), repoAddress: repoAddress, repoID: repoID, patch: patch,
	}
}

func (f *orderFixture) request(t *testing.T, orderID string, params ReviewOrderParams, addressTag *string) (any, *contextvm.Error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	tags := nostr.Tags{
		{"p", f.servicePK.Hex()},
		{"method", MethodReviewOrder},
		{"e", params.PatchEventID},
		{"expiration", strconv.FormatInt(time.Now().Add(5*time.Minute).Unix(), 10)},
	}
	if addressTag != nil {
		tags = append(tags, nostr.Tag{"a", *addressTag})
	}
	event := nostr.Event{
		Kind:      contextvm.KindContextVM,
		CreatedAt: nostr.Now(),
		Tags:      tags,
		Content:   string(raw),
	}
	if err := event.Sign(f.requesterKey); err != nil {
		t.Fatal(err)
	}
	return f.handler.HandleReviewOrder(f.ctx, contextvm.Request{
		Event: event, Sender: event.PubKey,
		Msg: contextvm.Message{JSONRPC: "2.0", ID: orderID, Method: MethodReviewOrder, Params: raw},
	})
}

func TestReviewOrderTargetCases(t *testing.T) {
	t.Run("patch only", func(t *testing.T) {
		f := newOrderFixture(t, true, []string{""}, nil, nil, 20)
		result, rpcErr := f.request(t, "patch-only", ReviewOrderParams{PatchEventID: f.patch.ID.Hex()}, nil)
		if rpcErr != nil {
			t.Fatalf("patch-only error: %+v", rpcErr)
		}
		accepted := result.(ReviewOrderAccepted)
		if !accepted.Accepted || accepted.RepoAddr != f.repoAddress || accepted.State != "queued" {
			t.Fatalf("accepted = %+v", accepted)
		}
	})

	t.Run("address only", func(t *testing.T) {
		f := newOrderFixture(t, true, []string{}, nil, nil, 20)
		_, rpcErr := f.request(t, "address-only", ReviewOrderParams{RepoAddr: f.repoAddress}, &f.repoAddress)
		if rpcErr == nil || rpcErr.Code != contextvm.ErrorInvalidParams {
			t.Fatalf("address-only error = %+v", rpcErr)
		}
	})

	t.Run("both consistent", func(t *testing.T) {
		f := newOrderFixture(t, true, []string{""}, nil, nil, 20)
		result, rpcErr := f.request(t, "consistent", ReviewOrderParams{
			PatchEventID: f.patch.ID.Hex(), RepoAddr: f.repoAddress,
		}, &f.repoAddress)
		if rpcErr != nil {
			t.Fatalf("consistent error: %+v", rpcErr)
		}
		if result.(ReviewOrderAccepted).RepoAddr != f.repoAddress {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("both mismatched", func(t *testing.T) {
		f := newOrderFixture(t, true, []string{""}, nil, nil, 20)
		other := "30617:" + nostr.GetPublicKey(nostr.Generate()).Hex() + ":other"
		_, rpcErr := f.request(t, "mismatch", ReviewOrderParams{
			PatchEventID: f.patch.ID.Hex(), RepoAddr: other,
		}, &other)
		if rpcErr == nil || rpcErr.Code != contextvm.ErrorInvalidParams {
			t.Fatalf("mismatch error = %+v", rpcErr)
		}
	})

	t.Run("missing patch", func(t *testing.T) {
		f := newOrderFixture(t, true, nil, nil, nil, 20)
		missing := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		_, rpcErr := f.request(t, "missing-patch", ReviewOrderParams{PatchEventID: missing}, nil)
		if rpcErr == nil || rpcErr.Code != contextvm.ErrorNotFound {
			t.Fatalf("missing patch error = %+v", rpcErr)
		}
	})

	t.Run("missing announcement", func(t *testing.T) {
		f := newOrderFixture(t, false, []string{""}, nil, nil, 20)
		_, rpcErr := f.request(t, "missing-announcement", ReviewOrderParams{PatchEventID: f.patch.ID.Hex()}, nil)
		if rpcErr == nil || rpcErr.Code != contextvm.ErrorNotFound {
			t.Fatalf("missing announcement error = %+v", rpcErr)
		}
	})

	t.Run("patch without unique repository", func(t *testing.T) {
		f := newOrderFixture(t, true, nil, nil, nil, 20)
		_, rpcErr := f.request(t, "missing-address", ReviewOrderParams{PatchEventID: f.patch.ID.Hex()}, nil)
		if rpcErr == nil || rpcErr.Code != contextvm.ErrorInvalidParams {
			t.Fatalf("missing address error = %+v", rpcErr)
		}
	})
}

func TestReviewOrderPaymentPreflightReturnsStructuredErrorWithoutReceipt(t *testing.T) {
	authorizer := &orderPaymentAuthorizer{result: payment.AuthorizeResult{
		Allowed: false, Reason: payment.ReasonPaymentPending, Retryable: true,
	}}
	loader := orderConfigLoader{config: []byte("payments:\n  enabled: true\n  price_sats: 100\n")}
	f := newOrderFixture(t, true, []string{""}, loader, authorizer, 20)
	_, rpcErr := f.request(t, "payment", ReviewOrderParams{PatchEventID: f.patch.ID.Hex()}, nil)
	if rpcErr == nil || rpcErr.Code != contextvm.ErrorPaymentRequired {
		t.Fatalf("payment error = %+v", rpcErr)
	}
	var data struct {
		Reason    string `json:"reason"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Reason != payment.ReasonPaymentPending || !data.Retryable || authorizer.calls != 1 {
		t.Fatalf("payment data=%+v calls=%d", data, authorizer.calls)
	}
	if _, ok, err := f.store.GetReviewOrder(f.ctx, nostr.GetPublicKey(f.requesterKey).Hex(), "payment"); err != nil || ok {
		t.Fatalf("payment denial created receipt: ok=%v err=%v", ok, err)
	}
}

func TestConcurrentDuplicateReviewOrdersShareOneReceipt(t *testing.T) {
	f := newOrderFixture(t, true, []string{""}, nil, nil, 20)
	requester := nostr.GetPublicKey(f.requesterKey).Hex()
	request := OnDemandRequest{
		PatchEventID: f.patch.ID.Hex(), RequesterPubkey: requester,
		OrderID: "concurrent", RequestEventID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Invocation: db.ReviewInvocationContextVM,
	}

	const callers = 8
	results := make([]AcceptedOrder, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.service.SubmitOnDemand(f.ctx, request)
		}(i)
	}
	close(start)
	wg.Wait()

	acquired := 0
	idempotent := 0
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i].Idempotent {
			idempotent++
		} else {
			acquired++
		}
	}
	if acquired != 1 || idempotent != callers-1 {
		t.Fatalf("acquired=%d idempotent=%d results=%+v", acquired, idempotent, results)
	}
	var receipts int
	if err := f.store.DB().QueryRowContext(f.ctx, `SELECT COUNT(*) FROM review_orders
		WHERE requester_pubkey=? AND order_id=?`, requester, request.OrderID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 {
		t.Fatalf("receipt count = %d, want 1", receipts)
	}

	conflict := request
	conflict.OrderID = "different-order"
	conflict.RequestEventID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if _, err := f.service.SubmitOnDemand(f.ctx, conflict); !errors.Is(err, ErrOrderConflict) {
		t.Fatalf("different order for active target error = %v", err)
	}
}

func TestReviewOrderRateLimitAndIdempotentRetry(t *testing.T) {
	f := newOrderFixture(t, true, []string{""}, nil, nil, 1)
	result, rpcErr := f.request(t, "one", ReviewOrderParams{PatchEventID: f.patch.ID.Hex()}, nil)
	if rpcErr != nil {
		t.Fatalf("first order: %+v", rpcErr)
	}
	first := result.(ReviewOrderAccepted)
	result, rpcErr = f.request(t, "one", ReviewOrderParams{PatchEventID: f.patch.ID.Hex()}, nil)
	if rpcErr != nil {
		t.Fatalf("idempotent retry consumed quota: %+v", rpcErr)
	}
	if result.(ReviewOrderAccepted).RequestEventID != first.RequestEventID {
		t.Fatalf("retry did not return original receipt: first=%+v retry=%+v", first, result)
	}

	otherPatch := nostr.Event{
		Kind: 1617, CreatedAt: nostr.Now(),
		Tags: nostr.Tags{{"a", f.repoAddress}}, Content: "other",
	}
	if err := otherPatch.Sign(nostr.Generate()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.InsertPatchEvent(f.ctx, otherPatch); err != nil {
		t.Fatal(err)
	}
	_, rpcErr = f.request(t, "two", ReviewOrderParams{PatchEventID: otherPatch.ID.Hex()}, nil)
	if rpcErr == nil || rpcErr.Code != contextvm.ErrorRateLimited {
		t.Fatalf("second order error = %+v", rpcErr)
	}
}
