package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/contextvm"
	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/monitoring"
	"git.sharegap.net/cascadia/drydock/internal/payment"
	"git.sharegap.net/cascadia/drydock/internal/repoconfig"
	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
)

type fakeAuthorizer struct {
	result payment.AuthorizeResult
	calls  int
	policy repoconfig.PaymentsConfig
}

func (f *fakeAuthorizer) AuthorizePatch(_ context.Context, _ nostr.Event, _ string, policy repoconfig.PaymentsConfig) (payment.AuthorizeResult, error) {
	f.calls++
	f.policy = policy
	return f.result, nil
}

type fixture struct {
	t          *testing.T
	ctx        context.Context
	store      *db.Store
	registry   *monitoring.Registry
	handler    *Handler
	service    *Service
	servicePK  nostr.PubKey
	owner      nostr.SecretKey
	maintainer nostr.SecretKey
	repo       scope.RepositoryRef
}

func newFixture(t *testing.T, policy Policy, authorizer payment.PatchAuthorizer, ceiling scope.Matcher) *fixture {
	t.Helper()
	ctx := context.Background()
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "enroll.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	servicePK := nostr.GetPublicKey(nostr.Generate())
	registry, err := monitoring.NewRegistry(store, monitoring.Config{Drydock: servicePK.Hex(), Scope: ceiling})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Load(ctx); err != nil {
		t.Fatal(err)
	}
	owner, maintainer := nostr.Generate(), nostr.Generate()
	repo, err := scope.ParseRepositoryRef("30617:" + nostr.GetPublicKey(owner).Hex() + ":svc")
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, registry, ceiling, authorizer, policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f := &fixture{t: t, ctx: ctx, store: store, registry: registry, service: service,
		handler: NewHandler(service, servicePK.Hex(), nil), servicePK: servicePK, owner: owner, maintainer: maintainer, repo: repo}
	return f
}

func (f *fixture) announce(createdAt int64, maintainers ...nostr.SecretKey) nostr.Event {
	f.t.Helper()
	tags := nostr.Tags{{"d", "svc"}}
	if len(maintainers) > 0 {
		tag := nostr.Tag{"maintainers"}
		for _, sk := range maintainers {
			tag = append(tag, nostr.GetPublicKey(sk).Hex())
		}
		tags = append(tags, tag)
	}
	event := nostr.Event{Kind: nostr.KindRepositoryAnnouncement, CreatedAt: nostr.Timestamp(createdAt), Tags: tags}
	if err := event.Sign(f.owner); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.store.ApplyRepositoryAnnouncement(f.ctx, event); err != nil {
		f.t.Fatal(err)
	}
	if err := f.registry.ApplyAnnouncement(f.ctx, event); err != nil {
		f.t.Fatal(err)
	}
	return event
}

type reqOpts struct {
	noExpiration bool
	createdAt    nostr.Timestamp
	aTag         string
	expiresAt    int64
}

func (f *fixture) call(sender nostr.SecretKey, method, repoAddr string, opts reqOpts) (Receipt, *contextvm.Error, nostr.Event) {
	f.t.Helper()
	var raw []byte
	if method == MethodEnroll {
		raw, _ = json.Marshal(EnrollParams{RepoAddr: repoAddr, ExpiresAt: opts.expiresAt})
	} else {
		raw, _ = json.Marshal(UnenrollParams{RepoAddr: repoAddr})
	}
	tags := nostr.Tags{{"p", f.servicePK.Hex()}, {"method", method}}
	if !opts.noExpiration {
		tags = append(tags, nostr.Tag{"expiration", strconv.FormatInt(time.Now().Add(5*time.Minute).Unix(), 10)})
	}
	if opts.aTag != "" {
		tags = append(tags, nostr.Tag{"a", opts.aTag})
	}
	createdAt := opts.createdAt
	if createdAt == 0 {
		createdAt = nostr.Now()
	}
	event := nostr.Event{Kind: contextvm.KindContextVM, CreatedAt: createdAt, Tags: tags, Content: string(raw)}
	if err := event.Sign(sender); err != nil {
		f.t.Fatal(err)
	}
	request := contextvm.Request{Event: event, Sender: event.PubKey, Msg: contextvm.Message{JSONRPC: "2.0", ID: "req-" + event.ID.Hex()[:8], Method: method, Params: raw}}
	var (
		result any
		rpcErr *contextvm.Error
	)
	if method == MethodEnroll {
		result, rpcErr = f.handler.HandleEnroll(f.ctx, request)
	} else {
		result, rpcErr = f.handler.HandleUnenroll(f.ctx, request)
	}
	if rpcErr != nil {
		return Receipt{}, rpcErr, event
	}
	receipt, ok := result.(Receipt)
	if !ok {
		f.t.Fatalf("result type %T", result)
	}
	return receipt, nil, event
}

func TestEnrollOwnerAndMaintainerButNotStranger(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	ann := f.announce(10, f.maintainer)

	receipt, rpcErr, event := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{})
	if rpcErr != nil {
		t.Fatalf("owner enroll: %+v", rpcErr)
	}
	if !receipt.Monitored || receipt.State != "enrolled" || receipt.EnrollmentID != event.ID.Hex() ||
		receipt.AnnouncementEventID != ann.ID.Hex() || receipt.AccessKind != AccessFree || receipt.ExpiresAt != 0 {
		t.Fatalf("owner receipt = %+v", receipt)
	}
	if !f.registry.Contains(f.repo.Address) {
		t.Fatal("registry does not contain enrolled repository")
	}

	stranger := nostr.Generate()
	if _, rpcErr, _ := f.call(stranger, MethodEnroll, f.repo.Address, reqOpts{}); rpcErr == nil || rpcErr.Code != contextvm.ErrorUnauthorized {
		t.Fatalf("stranger enroll = %+v", rpcErr)
	}
	receipt, rpcErr, _ = f.call(f.maintainer, MethodEnroll, f.repo.Address, reqOpts{})
	if rpcErr != nil || !receipt.Monitored {
		t.Fatalf("maintainer enroll = %+v err=%+v", receipt, rpcErr)
	}
}

func TestEnrollFailsClosedWithoutAnnouncementAndOutsideCeiling(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	if _, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{}); rpcErr == nil || rpcErr.Code != contextvm.ErrorNotFound {
		t.Fatalf("enroll without announcement = %+v", rpcErr)
	}

	other := nostr.Generate()
	ceiling := scope.NewMatcher([]string{nostr.GetPublicKey(other).Hex() + ":elsewhere"}, nil)
	g := newFixture(t, Policy{}, nil, ceiling)
	g.announce(10)
	if _, rpcErr, _ := g.call(g.owner, MethodEnroll, g.repo.Address, reqOpts{}); rpcErr == nil || rpcErr.Code != contextvm.ErrorUnauthorized {
		t.Fatalf("enroll outside static ceiling = %+v", rpcErr)
	}
}

func TestEnrollEnvelopeRequirements(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	f.announce(10)
	if _, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{noExpiration: true}); rpcErr == nil || rpcErr.Code != contextvm.ErrorInvalidParams {
		t.Fatalf("missing expiration = %+v", rpcErr)
	}
	if _, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{aTag: "30617:" + nostr.GetPublicKey(nostr.Generate()).Hex() + ":svc"}); rpcErr == nil || rpcErr.Code != contextvm.ErrorInvalidParams {
		t.Fatalf("mismatched a tag = %+v", rpcErr)
	}
	if _, rpcErr, _ := f.call(f.owner, MethodEnroll, "not-an-address", reqOpts{}); rpcErr == nil || rpcErr.Code != contextvm.ErrorInvalidParams {
		t.Fatalf("bad repo_addr = %+v", rpcErr)
	}
	if _, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{expiresAt: time.Now().Add(400 * 24 * time.Hour).Unix()}); rpcErr == nil || rpcErr.Code != contextvm.ErrorInvalidParams {
		t.Fatalf("expiry beyond max = %+v", rpcErr)
	}
	// A JSON-RPC id is mandatory.
	raw, _ := json.Marshal(EnrollParams{RepoAddr: f.repo.Address})
	event := nostr.Event{Kind: contextvm.KindContextVM, CreatedAt: nostr.Now(), Content: string(raw),
		Tags: nostr.Tags{{"p", f.servicePK.Hex()}, {"method", MethodEnroll}, {"expiration", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)}}}
	_ = event.Sign(f.owner)
	if _, rpcErr := f.handler.HandleEnroll(f.ctx, contextvm.Request{Event: event, Sender: event.PubKey, Msg: contextvm.Message{JSONRPC: "2.0", Method: MethodEnroll, Params: raw}}); rpcErr == nil || rpcErr.Code != contextvm.ErrorInvalidRequest {
		t.Fatalf("missing id = %+v", rpcErr)
	}
}

func TestEnrollIsIdempotentAndIgnoresStaleReplays(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	f.announce(10)
	now := nostr.Now()
	first, rpcErr, firstEvent := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{createdAt: now})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	// Same signed request again (relay redelivery past ingest dedup).
	raw, _ := json.Marshal(EnrollParams{RepoAddr: f.repo.Address})
	again, rpcErr := f.handler.HandleEnroll(f.ctx, contextvm.Request{Event: firstEvent, Sender: firstEvent.PubKey, Msg: contextvm.Message{JSONRPC: "2.0", ID: "dup", Method: MethodEnroll, Params: raw}})
	if rpcErr != nil || again.(Receipt).State != "already_enrolled" || again.(Receipt).EnrollmentID != first.EnrollmentID {
		t.Fatalf("replay = %+v err=%+v", again, rpcErr)
	}
	// An older request than the stored one must not replace it.
	older, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{createdAt: now - 60})
	if rpcErr != nil || older.State != "already_enrolled" || older.EnrollmentID != first.EnrollmentID {
		t.Fatalf("older request = %+v err=%+v", older, rpcErr)
	}
	// A newer request replaces it (e.g. to change expiry).
	newer, rpcErr, newerEvent := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{createdAt: now + 60, expiresAt: time.Now().Add(time.Hour).Unix()})
	if rpcErr != nil || newer.State != "enrolled" || newer.EnrollmentID != newerEvent.ID.Hex() || newer.ExpiresAt == 0 {
		t.Fatalf("newer request = %+v err=%+v", newer, rpcErr)
	}
}

func TestEnrollPaymentPolicy(t *testing.T) {
	denied := &fakeAuthorizer{result: payment.AuthorizeResult{Allowed: false, Reason: "payment_pending", Retryable: true}}
	f := newFixture(t, Policy{PriceSats: 1000, TTL: 24 * time.Hour}, denied, scope.Matcher{})
	f.announce(10)
	_, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{})
	if rpcErr == nil || rpcErr.Code != contextvm.ErrorPaymentRequired {
		t.Fatalf("denied payment = %+v", rpcErr)
	}
	var data map[string]any
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil || data["reason"] != "payment_pending" || data["retryable"] != true {
		t.Fatalf("payment error data = %s", rpcErr.Data)
	}
	if denied.policy.MaintainersAreFree() || denied.policy.PriceSats != 1000 || !denied.policy.Enabled {
		t.Fatalf("enrollment payment policy = %+v (maintainers must not be free)", denied.policy)
	}
	if f.registry.Contains(f.repo.Address) {
		t.Fatal("denied payment still enrolled")
	}

	paid := &fakeAuthorizer{result: payment.AuthorizeResult{Allowed: true, AccessKind: payment.AccessCashuReview}}
	g := newFixture(t, Policy{PriceSats: 1000, TTL: 24 * time.Hour}, paid, scope.Matcher{})
	g.announce(10)
	receipt, rpcErr, _ := g.call(g.owner, MethodEnroll, g.repo.Address, reqOpts{})
	if rpcErr != nil || receipt.AccessKind != payment.AccessCashuReview || receipt.ExpiresAt == 0 || !receipt.Monitored {
		t.Fatalf("paid enrollment = %+v err=%+v", receipt, rpcErr)
	}

	// Price configured but no payment service: fail closed, not free.
	h := newFixture(t, Policy{PriceSats: 1000}, nil, scope.Matcher{})
	h.announce(10)
	if _, rpcErr, _ := h.call(h.owner, MethodEnroll, h.repo.Address, reqOpts{}); rpcErr == nil || rpcErr.Code != contextvm.ErrorInternal {
		t.Fatalf("no payment service = %+v", rpcErr)
	}
}

func TestUnenrollRevokesOnlyOwnEnrollment(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	f.announce(10, f.maintainer)
	if _, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if _, rpcErr, _ := f.call(f.maintainer, MethodEnroll, f.repo.Address, reqOpts{}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	stranger := nostr.Generate()
	if _, rpcErr, _ := f.call(stranger, MethodUnenroll, f.repo.Address, reqOpts{}); rpcErr == nil || rpcErr.Code != contextvm.ErrorUnauthorized {
		t.Fatalf("stranger unenroll = %+v", rpcErr)
	}
	receipt, rpcErr, _ := f.call(f.owner, MethodUnenroll, f.repo.Address, reqOpts{})
	if rpcErr != nil || receipt.State != "revoked" || !receipt.Monitored {
		t.Fatalf("owner unenroll = %+v err=%+v (maintainer's enrollment must still grant)", receipt, rpcErr)
	}
	receipt, rpcErr, _ = f.call(f.maintainer, MethodUnenroll, f.repo.Address, reqOpts{})
	if rpcErr != nil || receipt.State != "revoked" || receipt.Monitored {
		t.Fatalf("maintainer unenroll = %+v err=%+v", receipt, rpcErr)
	}
	receipt, rpcErr, _ = f.call(f.maintainer, MethodUnenroll, f.repo.Address, reqOpts{})
	if rpcErr != nil || receipt.State != "revoked" {
		t.Fatalf("repeat unenroll = %+v err=%+v", receipt, rpcErr)
	}
	// Re-enrolling after revocation works and clears the revocation.
	receipt, rpcErr, _ = f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{createdAt: nostr.Now() + 5})
	if rpcErr != nil || receipt.State != "enrolled" || !receipt.Monitored {
		t.Fatalf("re-enroll = %+v err=%+v", receipt, rpcErr)
	}
}

func TestEnrollmentSurvivesRestartAndFollowsAuthority(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	f.announce(10, f.maintainer)
	if _, rpcErr, _ := f.call(f.maintainer, MethodEnroll, f.repo.Address, reqOpts{}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	restarted, err := monitoring.NewRegistry(f.store, monitoring.Config{Drydock: f.servicePK.Hex()})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Load(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !restarted.Contains(f.repo.Address) {
		t.Fatal("enrollment lost on restart")
	}
	// Owner drops the maintainer: the enrollment no longer grants.
	f.announce(20)
	if f.registry.Contains(f.repo.Address) {
		t.Fatal("enrollment by removed maintainer still grants")
	}
}

func TestRevocationOrderingBeatsReplayedOlderEnrollment(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	f.announce(10)
	now := nostr.Now()
	if _, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{createdAt: now - 100}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if _, rpcErr, _ := f.call(f.owner, MethodUnenroll, f.repo.Address, reqOpts{createdAt: now}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	// A previously signed enrollment from between the two arrives late.
	late, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{createdAt: now - 50})
	if rpcErr != nil || late.State != "revoked" || late.Monitored || f.registry.Contains(f.repo.Address) {
		t.Fatalf("late enrollment after revocation = %+v err=%+v monitored=%v", late, rpcErr, f.registry.Contains(f.repo.Address))
	}
	// A stale revocation cannot undo a newer enrollment either.
	if _, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{createdAt: now + 100}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	stale, rpcErr, _ := f.call(f.owner, MethodUnenroll, f.repo.Address, reqOpts{createdAt: now + 50})
	if rpcErr != nil || stale.State != "already_enrolled" || !f.registry.Contains(f.repo.Address) {
		t.Fatalf("stale revocation = %+v err=%+v", stale, rpcErr)
	}
}

func TestEqualTimestampRevocationBeatsEnrollment(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	f.announce(10)
	ts := nostr.Now()
	// Regardless of delivery order, an enrollment and a revocation signed in
	// the same second resolve to revoked.
	if _, rpcErr, _ := f.call(f.owner, MethodUnenroll, f.repo.Address, reqOpts{createdAt: ts}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	late, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{createdAt: ts})
	if rpcErr != nil || late.State != "revoked" || f.registry.Contains(f.repo.Address) {
		t.Fatalf("enroll after same-second revoke = %+v err=%+v", late, rpcErr)
	}
	g := newFixture(t, Policy{}, nil, scope.Matcher{})
	g.announce(10)
	if _, rpcErr, _ := g.call(g.owner, MethodEnroll, g.repo.Address, reqOpts{createdAt: ts}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	rev, rpcErr, _ := g.call(g.owner, MethodUnenroll, g.repo.Address, reqOpts{createdAt: ts})
	if rpcErr != nil || rev.State != "revoked" || g.registry.Contains(g.repo.Address) {
		t.Fatalf("revoke after same-second enroll = %+v err=%+v", rev, rpcErr)
	}
}

func TestRevocationBeforeEnrollmentLeavesTombstone(t *testing.T) {
	f := newFixture(t, Policy{}, nil, scope.Matcher{})
	f.announce(10)
	now := nostr.Now()
	receipt, rpcErr, _ := f.call(f.owner, MethodUnenroll, f.repo.Address, reqOpts{createdAt: now})
	if rpcErr != nil || receipt.State != "revoked" {
		t.Fatalf("revoke without enrollment = %+v err=%+v", receipt, rpcErr)
	}
	older, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{createdAt: now - 10})
	if rpcErr != nil || older.State != "revoked" || f.registry.Contains(f.repo.Address) {
		t.Fatalf("older enrollment after tombstone = %+v err=%+v", older, rpcErr)
	}
	newer, rpcErr, _ := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{createdAt: now + 10})
	if rpcErr != nil || newer.State != "enrolled" || !f.registry.Contains(f.repo.Address) {
		t.Fatalf("newer enrollment after tombstone = %+v err=%+v", newer, rpcErr)
	}
}

type flakyRegistry struct {
	Registry
	failNext bool
	applies  int
}

func (r *flakyRegistry) ApplyEnrollment(ctx context.Context, e monitoring.Enrollment) error {
	if r.failNext {
		r.failNext = false
		return errors.New("registry unavailable")
	}
	r.applies++
	return r.Registry.ApplyEnrollment(ctx, e)
}

func TestRetryAfterFailedActivationRepairsWithoutCharging(t *testing.T) {
	paid := &fakeAuthorizer{result: payment.AuthorizeResult{Allowed: true, AccessKind: payment.AccessCashuReview}}
	f := newFixture(t, Policy{PriceSats: 10, TTL: time.Hour}, paid, scope.Matcher{})
	f.announce(10)
	flaky := &flakyRegistry{Registry: f.registry, failNext: true}
	f.service.registry = flaky

	_, rpcErr, event := f.call(f.owner, MethodEnroll, f.repo.Address, reqOpts{})
	if rpcErr == nil || rpcErr.Code != contextvm.ErrorInternal {
		t.Fatalf("first attempt should surface the activation failure: %+v", rpcErr)
	}
	if f.registry.Contains(f.repo.Address) {
		t.Fatal("membership live despite failed activation")
	}
	// Identical signed request again: persisted row wins, no second charge,
	// and the live registry is repaired.
	raw, _ := json.Marshal(EnrollParams{RepoAddr: f.repo.Address})
	result, rpcErr := f.handler.HandleEnroll(f.ctx, contextvm.Request{Event: event, Sender: event.PubKey, Msg: contextvm.Message{JSONRPC: "2.0", ID: "retry", Method: MethodEnroll, Params: raw}})
	if rpcErr != nil || result.(Receipt).State != "already_enrolled" || !result.(Receipt).Monitored {
		t.Fatalf("retry = %+v err=%+v", result, rpcErr)
	}
	if paid.calls != 1 || flaky.applies != 1 {
		t.Fatalf("charges=%d applies=%d, want 1 and 1", paid.calls, flaky.applies)
	}
}

func TestRPCErrorMapping(t *testing.T) {
	cases := map[error]int{
		ErrInvalidTarget:                 contextvm.ErrorInvalidParams,
		ErrInvalidExpiry:                 contextvm.ErrorInvalidParams,
		ErrSecurityCeiling:               contextvm.ErrorUnauthorized,
		ErrNotMaintainer:                 contextvm.ErrorUnauthorized,
		ErrAnnouncementUnknown:           contextvm.ErrorNotFound,
		ErrPaymentsUnavailable:           contextvm.ErrorInternal,
		&PaymentDeniedError{Reason: "x"}: contextvm.ErrorPaymentRequired,
		errors.New("something else"):     contextvm.ErrorInternal,
	}
	for err, code := range cases {
		if got := RPCError(err); got.Code != code {
			t.Errorf("RPCError(%v) = %d, want %d", err, got.Code, code)
		}
	}
}
