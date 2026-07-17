package marketplace

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"drydock/internal/contextvm"
	"drydock/internal/db"

	"fiatjaf.com/nostr"
)

type integrationSigner struct{ sk nostr.SecretKey }

func newIntegrationSigner() integrationSigner {
	return integrationSigner{sk: nostr.Generate()}
}

func (s integrationSigner) pubkey() nostr.PubKey {
	return nostr.GetPublicKey(s.sk)
}

func (s integrationSigner) GetPublicKey(_ context.Context) (nostr.PubKey, error) {
	return s.pubkey(), nil
}

func (s integrationSigner) SignEvent(_ context.Context, evt *nostr.Event) error {
	return evt.Sign(s.sk)
}

type integrationContextVMTransport struct {
	method     string
	id         string
	assignment ReviewAssignment
	recipients []nostr.PubKey
}

func (t *integrationContextVMTransport) SendWithID(_ context.Context, id, method string, params any, recipients ...nostr.PubKey) (string, error) {
	t.id = id
	t.method = method
	t.recipients = append([]nostr.PubKey(nil), recipients...)
	t.assignment = params.(ReviewAssignment)
	return "contextvm-event-" + id, nil
}

func TestIntegrationReviewerProfileCreationWithNIP89(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	registry := NewRegistry(store, slog.Default())
	router := NewRouter(RouterConfig{}, registry, store, nil, nil, nil, slog.Default())
	handler := NewHandler(registry, router, store, slog.Default())
	signer := newIntegrationSigner()

	profile := ReviewerProfile{
		Pubkey:         signer.pubkey().Hex(),
		DisplayName:    "NIP-89 Reviewer",
		About:          "Go reviewer",
		Languages:      []string{"go"},
		Domains:        []string{"correctness"},
		Availability:   AvailabilityAvailable,
		PricePerReview: 500,
		MaxConcurrent:  2,
		ResponseTime:   "1h",
	}
	event, err := ReviewerProfileEvent(profile)
	if err != nil {
		t.Fatalf("ReviewerProfileEvent: %v", err)
	}
	if event.Kind != KindReviewerProfile {
		t.Fatalf("profile kind = %d, want %d", event.Kind, KindReviewerProfile)
	}
	if err := signer.SignEvent(ctx, &event); err != nil {
		t.Fatalf("sign profile: %v", err)
	}

	if err := handler.HandleEvent(ctx, event, "wss://relay.test"); err != nil {
		t.Fatalf("HandleEvent profile: %v", err)
	}
	got, _, err := registry.GetReviewer(ctx, signer.pubkey().Hex())
	if err != nil {
		t.Fatalf("GetReviewer: %v", err)
	}
	if got.DisplayName != profile.DisplayName || got.Availability != AvailabilityAvailable || got.PricePerReview != 500 {
		t.Fatalf("registered profile = %+v", got)
	}
}

func TestIntegrationMarketplaceContextVMAssignmentAcceptanceAndRejection(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	registry := NewRegistry(store, slog.Default())
	reviewer := newIntegrationSigner()
	requester := newIntegrationSigner()

	transport := &integrationContextVMTransport{}
	router := NewRouter(RouterConfig{MaxReviewersPerPatch: 1}, registry, store, requester, nil, transport, slog.Default())
	handler := NewHandler(registry, router, store, slog.Default())

	if err := registry.RegisterReviewer(ctx, ReviewerProfile{
		Pubkey:         reviewer.pubkey().Hex(),
		DisplayName:    "ContextVM Reviewer",
		Languages:      []string{"go"},
		Availability:   AvailabilityAvailable,
		PricePerReview: 100,
		MaxConcurrent:  1,
	}, "profile-event"); err != nil {
		t.Fatalf("RegisterReviewer: %v", err)
	}

	seedAuthorizedMarketplacePayment(t, ctx, store, "patch-route", "repo-1", requester.pubkey().Hex(), 100)
	route, err := router.RoutePatch(ctx, PatchInfo{
		PatchEventID: "patch-route",
		RepoID:       "repo-1",
		AuthorPubkey: requester.pubkey().Hex(),
		ChangedFiles: []string{"main.go"},
		PriceSats:    100,
	})
	if err != nil {
		t.Fatalf("RoutePatch: %v", err)
	}
	if route.AssignedCount != 1 || transport.method != MethodAssign {
		t.Fatalf("assignment route = %+v, transport method %q", route, transport.method)
	}
	if transport.assignment.AssignmentID == "" || len(transport.recipients) != 1 || transport.recipients[0].Hex() != reviewer.pubkey().Hex() {
		t.Fatalf("ContextVM assignment not addressed to reviewer: %+v recipients=%+v", transport.assignment, transport.recipients)
	}
	storedAssignment, err := store.GetAssignmentByEventID(ctx, transport.assignment.AssignmentID)
	if err != nil {
		t.Fatalf("GetAssignmentByEventID: %v", err)
	}
	if storedAssignment.RequesterPubkey != requester.pubkey().Hex() {
		t.Fatalf("stored requester = %q, want %q", storedAssignment.RequesterPubkey, requester.pubkey().Hex())
	}

	cvRouter := contextvm.NewRouter()
	if err := handler.RegisterContextVMMethods(cvRouter); err != nil {
		t.Fatalf("RegisterContextVMMethods: %v", err)
	}
	patchEvent := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"a", "30617:" + requester.pubkey().Hex() + ":repo-1"}},
		Content:   "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -0,0 +1 @@\n+package main\n",
	}
	if err := requester.SignEvent(ctx, &patchEvent); err != nil {
		t.Fatalf("sign patch event: %v", err)
	}
	if err := store.InsertPatchEvent(ctx, patchEvent); err != nil {
		t.Fatalf("InsertPatchEvent: %v", err)
	}

	assignedViaIntent := ReviewAssignment{
		AssignmentID:   "assign-via-contextvm",
		PatchEventID:   patchEvent.ID.Hex(),
		RepoID:         "repo-1",
		ReviewerPubkey: reviewer.pubkey().Hex(),
		Languages:      []string{"go"},
		PriceSats:      0,
		Deadline:       time.Now().Add(time.Hour).Unix(),
	}
	assignResp, err := cvRouter.Handle(ctx, contextVMRequest(t, requester, MethodAssign, "assign-rpc", assignedViaIntent, nostr.Tags{{"p", reviewer.pubkey().Hex()}}))
	if err != nil {
		t.Fatalf("ContextVM assign Handle: %v", err)
	}
	if assignResp.Error != nil || assignResp.ID != "assign-rpc" {
		t.Fatalf("assignment response = %+v", assignResp)
	}
	stored, err := store.GetAssignmentByEventID(ctx, "assign-rpc")
	if err != nil {
		t.Fatalf("GetAssignmentByEventID assign-rpc: %v", err)
	}
	if stored.Status != "pending" || stored.ReviewerPubkey != reviewer.pubkey().Hex() {
		t.Fatalf("stored assignment = %+v", stored)
	}

	acceptResp, err := cvRouter.Handle(ctx, contextVMRequest(t, reviewer, MethodAccept, "accept-rpc", ReviewAcceptance{AssignmentID: "assign-rpc", EstimatedTime: "2h"}, nil))
	if err != nil {
		t.Fatalf("ContextVM accept Handle: %v", err)
	}
	if acceptResp.Error != nil {
		t.Fatalf("acceptance response error: %+v", acceptResp.Error)
	}
	accepted, err := store.GetAssignmentByEventID(ctx, "assign-rpc")
	if err != nil {
		t.Fatalf("GetAssignmentByEventID accepted: %v", err)
	}
	if accepted.Status != "accepted" {
		t.Fatalf("accepted status = %q, want accepted", accepted.Status)
	}

	if err := store.CreateAssignment(ctx, db.ReviewAssignment{
		PatchEventID:      "patch-reject",
		RepoID:            "repo-1",
		ReviewerPubkey:    reviewer.pubkey().Hex(),
		RequesterPubkey:   requester.pubkey().Hex(),
		Status:            "pending",
		AssignmentEventID: "reject-assignment",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("CreateAssignment reject: %v", err)
	}
	rejectResp, err := cvRouter.Handle(ctx, contextVMRequest(t, reviewer, MethodReject, "reject-rpc", ReviewRejection{AssignmentID: "reject-assignment", Reason: "busy"}, nil))
	if err != nil {
		t.Fatalf("ContextVM reject Handle: %v", err)
	}
	if rejectResp.Error != nil {
		t.Fatalf("rejection response error: %+v", rejectResp.Error)
	}
	rejected, err := store.GetAssignmentByEventID(ctx, "reject-assignment")
	if err != nil {
		t.Fatalf("GetAssignmentByEventID rejected: %v", err)
	}
	if rejected.Status != "rejected" {
		t.Fatalf("rejected status = %q, want rejected", rejected.Status)
	}
}

func TestIntegrationFeedbackWithNIP90Kind7000(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	registry := NewRegistry(store, slog.Default())
	router := NewRouter(RouterConfig{}, registry, store, nil, nil, nil, slog.Default())
	handler := NewHandler(registry, router, store, slog.Default())
	reviewer := newIntegrationSigner()
	rater := newIntegrationSigner()

	if err := registry.RegisterReviewer(ctx, ReviewerProfile{
		Pubkey:       reviewer.pubkey().Hex(),
		Languages:    []string{"go"},
		Availability: AvailabilityAvailable,
	}, "profile-event"); err != nil {
		t.Fatalf("RegisterReviewer: %v", err)
	}
	if err := store.CreateAssignment(ctx, db.ReviewAssignment{
		PatchEventID:      "patch-feedback",
		RepoID:            "repo-1",
		ReviewerPubkey:    reviewer.pubkey().Hex(),
		RequesterPubkey:   rater.pubkey().Hex(),
		Status:            "accepted",
		AssignmentEventID: "feedback-assignment",
		CompletionEventID: "review-complete-event",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("CreateAssignment feedback: %v", err)
	}
	assignment, err := store.GetAssignmentByEventID(ctx, "feedback-assignment")
	if err != nil {
		t.Fatalf("GetAssignmentByEventID feedback: %v", err)
	}
	if err := store.UpdateAssignmentStatus(ctx, assignment.ID, "completed", "review-complete-event"); err != nil {
		t.Fatalf("UpdateAssignmentStatus completed: %v", err)
	}

	content, err := json.Marshal(ReviewFeedback{Helpful: true, Accurate: true, Comment: "Useful review"})
	if err != nil {
		t.Fatalf("marshal feedback: %v", err)
	}
	feedbackEvent := nostr.Event{
		Kind:      KindReviewFeedback,
		CreatedAt: nostr.Now(),
		Tags:      ReviewFeedbackTags("review-complete-event", reviewer.pubkey().Hex(), 5),
		Content:   string(content),
	}
	if feedbackEvent.Kind != KindReviewFeedback {
		t.Fatalf("feedback kind = %d, want %d", feedbackEvent.Kind, KindReviewFeedback)
	}
	if err := rater.SignEvent(ctx, &feedbackEvent); err != nil {
		t.Fatalf("sign feedback: %v", err)
	}

	if err := handler.HandleEvent(ctx, feedbackEvent, "wss://relay.test"); err != nil {
		t.Fatalf("HandleEvent feedback: %v", err)
	}
	_, reputation, err := registry.GetReviewer(ctx, reviewer.pubkey().Hex())
	if err != nil {
		t.Fatalf("GetReviewer reputation: %v", err)
	}
	if reputation.AverageRating != 5 || reputation.TotalReviews != 1 {
		t.Fatalf("reputation = %+v, want average 5 and one review", reputation)
	}
}

func contextVMRequest(t *testing.T, signer integrationSigner, method, id string, params any, tags nostr.Tags) contextvm.Request {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	msg := contextvm.Message{JSONRPC: "2.0", ID: id, Method: method, Params: raw}
	content, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	event := nostr.Event{Kind: nostr.Kind(25910), CreatedAt: nostr.Now(), Tags: tags, Content: string(content)}
	if err := signer.SignEvent(context.Background(), &event); err != nil {
		t.Fatalf("sign contextvm event: %v", err)
	}
	return contextvm.Request{Event: event, Relay: "wss://relay.test", Sender: signer.pubkey(), Msg: msg}
}
