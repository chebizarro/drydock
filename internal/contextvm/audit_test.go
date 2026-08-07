package contextvm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"drydock/internal/auditengine"

	"fiatjaf.com/nostr"
)

type auditTestStore struct {
	announcement   nostr.Event
	cloneURLs      []string
	sarif          []byte
	sarifHash      string
	sarifRequester string
}

func (s auditTestStore) GetRepositoryAnnouncement(context.Context, string) (nostr.Event, error) {
	return s.announcement, nil
}

func (s auditTestStore) GetRepositoryCloneURLs(context.Context, string) ([]string, error) {
	return append([]string(nil), s.cloneURLs...), nil
}

func (s auditTestStore) SecurityAuditSARIFForRequester(_ context.Context, _ int64, requester string) ([]byte, string, error) {
	if requester != s.sarifRequester || len(s.sarif) == 0 {
		return nil, "", errors.New("not found")
	}
	return append([]byte(nil), s.sarif...), s.sarifHash, nil
}

type auditTestConfigLoader struct {
	data []byte
}

func (l auditTestConfigLoader) LoadBaseRepoConfig(context.Context, string) ([]byte, error) {
	return l.data, nil
}

type auditTestNotifier struct {
	notifications []Notification
	err           error
}

func (n *auditTestNotifier) Notify(_ context.Context, notification Notification) (string, error) {
	n.notifications = append(n.notifications, notification)
	return "notification-event-id", n.err
}

type auditTestRunner struct {
	req      auditengine.Request
	progress *AuditFeedbackReporter
	result   auditengine.Result
	err      error
}

func (r *auditTestRunner) Run(ctx context.Context, req auditengine.Request) (auditengine.Result, error) {
	r.req = req
	if r.progress != nil {
		if err := r.progress.ReportAuditProgress(ctx, 42, "prepare"); err != nil {
			return auditengine.Result{}, err
		}
		if r.err == nil {
			if err := r.progress.ReportAuditProgress(ctx, 42, "published"); err != nil {
				return auditengine.Result{}, err
			}
		}
	}
	return r.result, r.err
}

func TestSecurityAuditMethodRunsEngineAndPublishesProgress(t *testing.T) {
	ctx := context.Background()
	ownerSK := nostr.Generate()
	owner := nostr.GetPublicKey(ownerSK)
	requesterSK := nostr.Generate()
	requester := nostr.GetPublicKey(requesterSK)
	announcement := nostr.Event{
		Kind: 30617, PubKey: owner,
		Tags: nostr.Tags{{"d", "repo-one"}, {"clone", "https://example.test/repo.git"}},
	}
	announcement.ID = announcement.GetID()

	published := &auditTestNotifier{}
	feedback := NewAuditFeedbackReporter(published, []string{"wss://write.test"})
	runner := &auditTestRunner{progress: feedback, result: auditengine.Result{AuditID: 42}}
	handler := NewSecurityAuditHandler(
		runner,
		auditTestStore{announcement: announcement, cloneURLs: []string{"https://example.test/repo.git"}},
		auditTestConfigLoader{data: []byte("security:\n  audit:\n    depth: deep\n")},
		feedback,
		[]string{"wss://write.test"},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	handler.start = func(run func()) { run() }

	params, err := json.Marshal(SecurityAuditParams{
		RepoAddr:    "30617:" + owner.Hex() + ":repo-one",
		Subtree:     "internal/auth",
		SinceCommit: "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	requestEvent := nostr.Event{
		ID:     nostr.MustIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		PubKey: requester,
		Kind:   KindContextVM,
	}
	result, rpcErr := handler.HandleSecurityAudit(ctx, Request{
		Event: requestEvent, Relay: "wss://read.test", Sender: requester,
		Msg: Message{JSONRPC: jsonRPCVersion, ID: "rpc-1", Method: MethodSecurityAudit, Params: params},
	})
	if rpcErr != nil {
		t.Fatalf("HandleSecurityAudit error: %+v", rpcErr)
	}
	accepted, ok := result.(SecurityAuditAccepted)
	if !ok || !accepted.Accepted || accepted.RequestEventID != requestEvent.ID.Hex() {
		t.Fatalf("unexpected acceptance: %#v", result)
	}
	if runner.req.RepoID != owner.Hex()+":repo-one" || runner.req.Depth != auditengine.DepthDeep {
		t.Fatalf("unexpected audit request: %+v", runner.req)
	}
	if runner.req.Ref != "" || runner.req.Subtree != "internal/auth" || runner.req.SinceCommit != "abc123" {
		t.Fatalf("audit params not forwarded: %+v", runner.req)
	}
	if len(published.notifications) != 2 {
		t.Fatalf("published progress count = %d, want 2", len(published.notifications))
	}
	assertAuditProgressNotification(t, published.notifications[0], requestEvent.ID.Hex(), requester, "prepare", "processing")
	assertAuditProgressNotification(t, published.notifications[1], requestEvent.ID.Hex(), requester, "published", "success")
}

func TestSecurityAuditMethodPublishesFailure(t *testing.T) {
	owner := nostr.GetPublicKey(nostr.Generate())
	requester := nostr.GetPublicKey(nostr.Generate())
	announcement := nostr.Event{Kind: 30617, PubKey: owner, Tags: nostr.Tags{{"d", "repo"}}}
	announcement.ID = announcement.GetID()

	published := &auditTestNotifier{}
	feedback := NewAuditFeedbackReporter(published, []string{"wss://write.test"})
	runner := &auditTestRunner{progress: feedback, result: auditengine.Result{AuditID: 42}, err: errors.New("review failed")}
	handler := NewSecurityAuditHandler(
		runner,
		auditTestStore{announcement: announcement, cloneURLs: []string{"https://example.test/repo.git"}},
		nil,
		feedback,
		[]string{"wss://write.test"},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	handler.start = func(run func()) { run() }
	params, _ := json.Marshal(SecurityAuditParams{RepoAddr: "30617:" + owner.Hex() + ":repo", Depth: "quick"})
	requestEvent := nostr.Event{ID: nostr.MustIDFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), PubKey: requester, Kind: KindContextVM}

	_, rpcErr := handler.HandleSecurityAudit(context.Background(), Request{
		Event: requestEvent, Sender: requester,
		Msg: Message{JSONRPC: jsonRPCVersion, ID: "rpc-2", Method: MethodSecurityAudit, Params: params},
	})
	if rpcErr != nil {
		t.Fatalf("HandleSecurityAudit error: %+v", rpcErr)
	}
	if len(published.notifications) != 2 {
		t.Fatalf("published progress count = %d, want progress + failure", len(published.notifications))
	}
	assertAuditProgressNotification(t, published.notifications[1], requestEvent.ID.Hex(), requester, "failed", "error")
}

func TestSecurityAuditSARIFMethodAuthorizesRequester(t *testing.T) {
	requester := nostr.GetPublicKey(nostr.Generate())
	store := auditTestStore{
		sarif: []byte(`{"version":"2.1.0","runs":[]}`), sarifHash: "abc123",
		sarifRequester: requester.Hex(),
	}
	handler := NewSecurityAuditHandler(nil, store, nil, nil, nil, nil)
	params, _ := json.Marshal(SecurityAuditSARIFParams{AuditID: 42})
	result, rpcErr := handler.HandleSecurityAuditSARIF(context.Background(), Request{
		Sender: requester, Msg: Message{Params: params},
	})
	if rpcErr != nil {
		t.Fatalf("HandleSecurityAuditSARIF error: %+v", rpcErr)
	}
	got, ok := result.(SecurityAuditSARIFResult)
	if !ok || got.AuditID != 42 || got.SHA256 != "abc123" || !json.Valid(got.SARIF) {
		t.Fatalf("unexpected SARIF result: %#v", result)
	}

	_, rpcErr = handler.HandleSecurityAuditSARIF(context.Background(), Request{
		Sender: nostr.GetPublicKey(nostr.Generate()), Msg: Message{Params: params},
	})
	if rpcErr == nil || rpcErr.Code != ErrorNotFound {
		t.Fatalf("unauthorized retrieval error = %+v, want not found", rpcErr)
	}
}

func TestSecurityAuditMethodRejectsInvalidDepth(t *testing.T) {
	owner := nostr.GetPublicKey(nostr.Generate())
	handler := NewSecurityAuditHandler(&auditTestRunner{}, auditTestStore{}, nil, nil, nil, nil)
	handler.start = func(run func()) { run() }
	params, _ := json.Marshal(SecurityAuditParams{RepoAddr: "30617:" + owner.Hex() + ":repo", Depth: "overnight"})
	_, rpcErr := handler.HandleSecurityAudit(context.Background(), Request{
		Event: nostr.Event{ID: nostr.MustIDFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")},
		Msg:   Message{Params: params},
	})
	if rpcErr == nil || rpcErr.Code != ErrorInvalidParams {
		t.Fatalf("rpc error = %+v, want invalid params", rpcErr)
	}
}

func assertAuditProgressNotification(t *testing.T, notification Notification, requestID string, requester nostr.PubKey, phase, status string) {
	t.Helper()
	if notification.Method != MethodSecurityAuditProgress || notification.RelatedEventID != requestID {
		t.Fatalf("notification routing = %+v", notification)
	}
	if len(notification.Recipients) != 1 || notification.Recipients[0] != requester {
		t.Fatalf("notification recipients = %v, want %s", notification.Recipients, requester.Hex())
	}
	progress, ok := notification.Params.(SecurityAuditProgress)
	if !ok {
		t.Fatalf("notification params type = %T", notification.Params)
	}
	if progress.AuditID != 42 || progress.RequestEventID != requestID || progress.Phase != phase || progress.Status != status || progress.OccurredAt <= 0 {
		t.Fatalf("progress params = %+v", progress)
	}
}

func TestSecurityAuditProgressTerminalAndOutOfOrderSemantics(t *testing.T) {
	processing := SecurityAuditProgress{Status: "processing", OccurredAt: 10}
	success := SecurityAuditProgress{Status: "success", OccurredAt: 20}
	if !ShouldApplySecurityAuditProgress(processing, "event-a", success, "event-b") {
		t.Fatal("newer terminal progress was rejected")
	}
	if !ShouldApplySecurityAuditProgress(SecurityAuditProgress{Status: "processing", OccurredAt: 20}, "event-z", success, "event-a") {
		t.Fatal("equal-timestamp terminal progress lost to event-id ordering")
	}
	if ShouldApplySecurityAuditProgress(success, "event-b", SecurityAuditProgress{Status: "processing", OccurredAt: 30}, "event-c") {
		t.Fatal("terminal progress regressed to later processing")
	}
	if ShouldApplySecurityAuditProgress(processing, "event-b", SecurityAuditProgress{Status: "processing", OccurredAt: 9}, "event-z") {
		t.Fatal("older out-of-order progress was accepted")
	}
	if !ShouldApplySecurityAuditProgress(processing, "event-b", SecurityAuditProgress{Status: "processing", OccurredAt: 10}, "event-c") {
		t.Fatal("event id did not break equal-timestamp ordering")
	}
	if ShouldApplySecurityAuditProgress(processing, "event-b", processing, "event-b") {
		t.Fatal("exact duplicate progress was accepted")
	}
}
