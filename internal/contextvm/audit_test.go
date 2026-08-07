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
	announcement nostr.Event
	cloneURLs    []string
}

func (s auditTestStore) GetRepositoryAnnouncement(context.Context, string) (nostr.Event, error) {
	return s.announcement, nil
}

func (s auditTestStore) GetRepositoryCloneURLs(context.Context, string) ([]string, error) {
	return append([]string(nil), s.cloneURLs...), nil
}

type auditTestConfigLoader struct {
	data []byte
}

func (l auditTestConfigLoader) LoadBaseRepoConfig(context.Context, string) ([]byte, error) {
	return l.data, nil
}

type auditTestPublisher struct {
	events []nostr.Event
	err    error
}

func (p *auditTestPublisher) Publish(_ context.Context, _ []string, event nostr.Event) error {
	p.events = append(p.events, event)
	return p.err
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

	signer := newTestSigner(23)
	published := &auditTestPublisher{}
	feedback := NewAuditFeedbackReporter(signer, published, []string{"wss://write.test"})
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
	if len(published.events) != 2 {
		t.Fatalf("published feedback count = %d, want 2", len(published.events))
	}
	assertAuditFeedback(t, published.events[0], requestEvent.ID.Hex(), requester.Hex(), "prepare", "processing")
	assertAuditFeedback(t, published.events[1], requestEvent.ID.Hex(), requester.Hex(), "published", "success")
}

func TestSecurityAuditMethodPublishesFailure(t *testing.T) {
	owner := nostr.GetPublicKey(nostr.Generate())
	requester := nostr.GetPublicKey(nostr.Generate())
	announcement := nostr.Event{Kind: 30617, PubKey: owner, Tags: nostr.Tags{{"d", "repo"}}}
	announcement.ID = announcement.GetID()

	signer := newTestSigner(29)
	published := &auditTestPublisher{}
	feedback := NewAuditFeedbackReporter(signer, published, []string{"wss://write.test"})
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
	if len(published.events) != 2 {
		t.Fatalf("published feedback count = %d, want progress + failure", len(published.events))
	}
	assertAuditFeedback(t, published.events[1], requestEvent.ID.Hex(), requester.Hex(), "failed", "error")
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

func assertAuditFeedback(t *testing.T, event nostr.Event, requestID, requester, phase, status string) {
	t.Helper()
	if event.Kind != eventkindReviewFeedbackForTest() {
		t.Fatalf("feedback kind = %d, want 7000", event.Kind)
	}
	for name, want := range map[string]string{
		"e": requestID, "p": requester, "phase": phase, "status": status, "t": "security-audit",
	} {
		tag := event.Tags.Find(name)
		if tag == nil || len(tag) < 2 || tag[1] != want {
			t.Fatalf("%s tag = %#v, want %q", name, tag, want)
		}
	}
	if !event.CheckID() || !event.VerifySignature() {
		t.Fatal("feedback event is not validly signed")
	}
}

func eventkindReviewFeedbackForTest() nostr.Kind { return nostr.KindJobFeedback }
