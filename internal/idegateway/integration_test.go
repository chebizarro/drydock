package idegateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/contextbuilder"
	"drydock/internal/contextvm"
	"drydock/internal/db"
	"drydock/internal/reviewengine"
	"drydock/internal/testutil"

	"fiatjaf.com/nostr"
)

type integSigner struct {
	sk nostr.SecretKey
}

func (s integSigner) GetPublicKey(_ context.Context) (nostr.PubKey, error) {
	return nostr.GetPublicKey(s.sk), nil
}

func (s integSigner) SignEvent(_ context.Context, evt *nostr.Event) error {
	return evt.Sign(s.sk)
}

type integRelayPublisher struct {
	events []nostr.Event
}

func (p *integRelayPublisher) Publish(_ context.Context, _ []string, event nostr.Event) error {
	p.events = append(p.events, event)
	return nil
}

func assertTagValue(t *testing.T, tags nostr.Tags, name, want string) {
	t.Helper()
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == name && tag[1] == want {
			return
		}
	}
	t.Fatalf("missing tag %q=%q in %v", name, want, tags)
}

func TestIntegrationIDEGatewaySignedHandleEventReviewToFixFlow(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	storePath := filepath.Join(t.TempDir(), "ide-gateway.db")
	store, err := db.Open(ctx, storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate store: %v", err)
	}

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\nfunc x() int { return 0 }\n"), 0o644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	llm := &testutil.FakeLLM{
		Responses: []string{
			`{"change_type":"feature","risk_areas":["correctness"],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`,
			`{"summary":"Found one issue.","findings":[{"severity":"high","category":"correctness","file":"main.go","line":2,"evidence":"unused branch","explanation":"Prefer explicit return handling.","suggestion":"apply patch","suggested_diff":"@@ -2,1 +2,1 @@\n-return 0\n+return 1","confidence":0.95}],"needs_more_context":[]}`,
		},
	}

	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "planner"},
		Coder32B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder32b"},
		LLM70B:   reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "llm70b"},
		Coder14B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder14b"},
	}, llm, logger)

	pub := &integRelayPublisher{}
	gatewaySK := nostr.Generate()
	ideSK := nostr.Generate()
	handler := New(
		Config{DefaultRelays: []string{"wss://relay.test"}},
		store,
		contextbuilder.NewDefault(),
		engine,
		integSigner{sk: gatewaySK},
		pub,
		logger,
	)

	idePubKey := nostr.GetPublicKey(ideSK)

	sessionEvent := nostr.Event{
		Kind:      nostr.Kind(KindIDESession),
		CreatedAt: nostr.Now(),
		Content:   `{"workspace_path":"` + workspace + `","editor":"vscode","version":"1.0.0"}`,
		Tags:      nostr.Tags{{"d", "sess-1"}, {"p", handler.ourPubKey}},
	}
	if err := sessionEvent.Sign(ideSK); err != nil {
		t.Fatalf("sign session event: %v", err)
	}
	if err := handler.HandleEvent(ctx, sessionEvent, "wss://relay.test"); err != nil {
		t.Fatalf("handle session: %v", err)
	}

	reviewContent, err := contextvm.MarshalRequest("req-1", MethodIDEReview, ReviewRequest{
		SessionID:    "sess-1",
		Diff:         "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1,2 @@\n package main\n+func x() int { return 0 }\n",
		ChangedFiles: []string{"main.go"},
	})
	if err != nil {
		t.Fatalf("marshal review request: %v", err)
	}
	reviewEvent := nostr.Event{
		Kind:      nostr.Kind(KindIDEReviewRequest),
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"p", handler.ourPubKey}, {"session", "sess-1"}, {"request", "req-1"}, {"method", MethodIDEReview}, {"t", "drydock-ide"}},
		Content:   reviewContent,
	}
	if err := reviewEvent.Sign(ideSK); err != nil {
		t.Fatalf("sign review event: %v", err)
	}

	tamperedReview := reviewEvent
	tamperedReview.Content = strings.Replace(tamperedReview.Content, "return 0", "return 2", 1)
	if err := handler.HandleEvent(ctx, tamperedReview, "wss://relay.test"); err != nil {
		t.Fatalf("handle tampered review request: %v", err)
	}
	if len(pub.events) != 0 {
		t.Fatalf("tampered signed review request produced %d response(s), want 0", len(pub.events))
	}

	if err := handler.HandleEvent(ctx, reviewEvent, "wss://relay.test"); err != nil {
		t.Fatalf("handle review request: %v", err)
	}

	if len(pub.events) != 1 {
		t.Fatalf("published events after review = %d, want 1", len(pub.events))
	}

	reviewEnvelope := pub.events[0]
	if reviewEnvelope.Kind != nostr.Kind(KindIDEReviewResponse) {
		t.Fatalf("review response kind = %d, want %d", reviewEnvelope.Kind, KindIDEReviewResponse)
	}
	if !reviewEnvelope.CheckID() || !reviewEnvelope.VerifySignature() {
		t.Fatal("review response is not a valid signed nostr event")
	}
	assertTagValue(t, reviewEnvelope.Tags, "e", reviewEvent.ID.Hex())
	assertTagValue(t, reviewEnvelope.Tags, "p", idePubKey.Hex())
	assertTagValue(t, reviewEnvelope.Tags, "session", "sess-1")

	reviewMsg, err := contextvm.ParseMessage(pub.events[0].Content)
	if err != nil {
		t.Fatalf("parse review ContextVM response: %v", err)
	}
	if reviewMsg.ID != "req-1" {
		t.Fatalf("review response id = %q, want req-1", reviewMsg.ID)
	}
	var reviewResp ReviewResponse
	if err := json.Unmarshal(reviewMsg.Result, &reviewResp); err != nil {
		t.Fatalf("unmarshal review response result: %v", err)
	}
	if len(reviewResp.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %d, want 1", len(reviewResp.Diagnostics))
	}

	diag := reviewResp.Diagnostics[0]
	if !diag.HasFix || diag.FixID == "" {
		t.Fatalf("diagnostic fix metadata missing: %+v", diag)
	}
	if diag.SuggestedFix == "" {
		t.Fatal("expected suggested_fix in review response")
	}
	stored, ok, err := store.GetIDEGatewayFix(ctx, diag.FixID, "sess-1")
	if err != nil {
		t.Fatalf("load stored fix: %v", err)
	}
	if !ok {
		t.Fatalf("expected fix %q to be persisted", diag.FixID)
	}
	if stored.AuthorPubKey != idePubKey.Hex() || stored.File != "main.go" || stored.Diff != diag.SuggestedFix {
		t.Fatalf("unexpected stored fix: %+v; diagnostic fix=%q", stored, diag.SuggestedFix)
	}

	fixContent, err := contextvm.MarshalRequest("fix-req-1", MethodIDEApplyFix, FixRequest{
		SessionID: "sess-1",
		FixID:     diag.FixID,
		File:      "main.go",
	})
	if err != nil {
		t.Fatalf("marshal fix request: %v", err)
	}
	fixEvent := nostr.Event{
		Kind:      nostr.Kind(KindIDEFixRequest),
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"p", handler.ourPubKey}, {"session", "sess-1"}, {"request", "fix-req-1"}, {"method", MethodIDEApplyFix}, {"t", "drydock-ide"}},
		Content:   fixContent,
	}
	if err := fixEvent.Sign(ideSK); err != nil {
		t.Fatalf("sign fix event: %v", err)
	}
	if err := handler.HandleEvent(ctx, fixEvent, "wss://relay.test"); err != nil {
		t.Fatalf("handle fix request: %v", err)
	}

	if len(pub.events) != 2 {
		t.Fatalf("published events after fix = %d, want 2", len(pub.events))
	}

	fixEnvelope := pub.events[1]
	if fixEnvelope.Kind != nostr.Kind(KindIDEFixResponse) {
		t.Fatalf("fix response kind = %d, want %d", fixEnvelope.Kind, KindIDEFixResponse)
	}
	if !fixEnvelope.CheckID() || !fixEnvelope.VerifySignature() {
		t.Fatal("fix response is not a valid signed nostr event")
	}
	assertTagValue(t, fixEnvelope.Tags, "e", fixEvent.ID.Hex())
	assertTagValue(t, fixEnvelope.Tags, "p", idePubKey.Hex())
	assertTagValue(t, fixEnvelope.Tags, "session", "sess-1")

	fixMsg, err := contextvm.ParseMessage(pub.events[1].Content)
	if err != nil {
		t.Fatalf("parse fix ContextVM response: %v", err)
	}
	if fixMsg.ID != "fix-req-1" {
		t.Fatalf("fix response id = %q, want fix-req-1", fixMsg.ID)
	}
	var fixResp FixResponse
	if err := json.Unmarshal(fixMsg.Result, &fixResp); err != nil {
		t.Fatalf("unmarshal fix response result: %v", err)
	}
	if !fixResp.Success {
		t.Fatalf("fix response not successful: %s", fixResp.Error)
	}
	if fixResp.Diff == "" {
		t.Fatal("expected diff in fix response")
	}
	if fixResp.Diff != diag.SuggestedFix {
		t.Fatalf("fix diff mismatch: got %q want %q", fixResp.Diff, diag.SuggestedFix)
	}
}
