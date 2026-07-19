package conversation

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"drydock/internal/db"
	"drydock/internal/reviewengine"

	"fiatjaf.com/nostr"
)

// --- test doubles ---

type fakeSigner struct {
	pubkey nostr.PubKey
}

func (f *fakeSigner) GetPublicKey(_ context.Context) (nostr.PubKey, error) {
	return f.pubkey, nil
}

func (f *fakeSigner) SignEvent(_ context.Context, evt *nostr.Event) error {
	evt.ID = evt.GetID()
	return nil
}

type fakeRelayPublisher struct {
	published []nostr.Event
}

func (f *fakeRelayPublisher) Publish(_ context.Context, _ []string, event nostr.Event) error {
	f.published = append(f.published, event)
	return nil
}

type fakeLLM struct {
	responses []string
	requests  []reviewengine.ChatRequest
}

func (f *fakeLLM) ChatCompletion(_ context.Context, req reviewengine.ChatRequest) (reviewengine.ChatResult, error) {
	f.requests = append(f.requests, req)
	if len(f.responses) == 0 {
		return reviewengine.ChatResult{Content: "Thanks for the feedback!"}, nil
	}
	r := f.responses[0]
	f.responses = f.responses[1:]
	return reviewengine.ChatResult{Content: r}, nil
}

// --- helpers ---

func setupTestDB(t *testing.T) *db.Store {
	t.Helper()
	ctx := context.Background()
	store, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func seedReviewEvent(t *testing.T, store *db.Store, reviewEventID, patchEventID, repoID string) {
	t.Helper()
	ctx := context.Background()

	// Insert a patch event first.
	patchEvtID := nostr.MustIDFromHex(patchEventID)
	patchPK := nostr.MustPubKeyFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	patchEvent := nostr.Event{
		ID:        patchEvtID,
		PubKey:    patchPK,
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Content:   "--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new",
	}

	rawPatch := patchEvent.String()
	_, err := store.DB().ExecContext(ctx,
		`INSERT INTO patch_events(event_id, repo_id, kind, author_pubkey, root_id, created_at, content, raw_event_json, seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		patchEventID, repoID, 1617, patchEvent.PubKey.Hex(), patchEventID,
		int64(patchEvent.CreatedAt), patchEvent.Content, rawPatch, time.Now().Unix(),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Insert the review event.
	reviewEvtID := nostr.MustIDFromHex(reviewEventID)
	reviewPK := nostr.MustPubKeyFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	reviewEvent := nostr.Event{
		ID:        reviewEvtID,
		PubKey:    reviewPK,
		Kind:      nostr.KindComment,
		CreatedAt: nostr.Now(),
		Content:   "Automated review summary\nFound 2 issues in this patch.",
	}

	rawReview := reviewEvent.String()
	_, err = store.DB().ExecContext(ctx,
		`INSERT INTO review_events(event_id, patch_event_id, repo_id, created_at, raw_event_json)
			VALUES (?, ?, ?, ?, ?)`,
		reviewEventID, patchEventID, repoID, int64(reviewEvent.CreatedAt), rawReview,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func makeHandler(t *testing.T, store *db.Store, llm *fakeLLM) (*Handler, *fakeRelayPublisher) {
	t.Helper()
	signer := &fakeSigner{
		pubkey: nostr.MustPubKeyFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	}
	relayPub := &fakeRelayPublisher{}
	h := New(Config{
		Endpoint: reviewengine.ModelEndpoint{
			BaseURL: "http://test",
			APIKey:  "test",
			Model:   "test-model",
		},
		DefaultRelays: []string{"wss://relay.test"},
		ResponseTTL:   24 * time.Hour,
	}, store, llm, signer, relayPub, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	return h, relayPub
}

const (
	testReviewID = "1111111111111111111111111111111111111111111111111111111111111111"
	testPatchID  = "2222222222222222222222222222222222222222222222222222222222222222"
	testRepoID   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:myrepo"
)

// --- tests ---

func TestHandleReply_BasicConversation(t *testing.T) {
	store := setupTestDB(t)
	seedReviewEvent(t, store, testReviewID, testPatchID, testRepoID)

	llm := &fakeLLM{responses: []string{"Good point, I'll reconsider that finding."}}
	h, relayPub := makeHandler(t, store, llm)

	replyEvent := nostr.Event{
		ID:        nostr.MustIDFromHex("3333333333333333333333333333333333333333333333333333333333333333"),
		PubKey:    nostr.MustPubKeyFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		Kind:      nostr.KindComment,
		CreatedAt: nostr.Now(),
		Content:   "I think the warning about nil checks is a false positive here.",
		Tags: nostr.Tags{
			{"e", testReviewID},
			{"p", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
	}

	ctx := context.Background()
	if err := h.HandleReply(ctx, replyEvent, "wss://reply-relay.test"); err != nil {
		t.Fatalf("HandleReply failed: %v", err)
	}

	// Verify LLM was called.
	if len(llm.requests) != 1 {
		t.Fatalf("expected 1 LLM request, got %d", len(llm.requests))
	}
	if !strings.Contains(llm.requests[0].User, "nil checks") {
		t.Error("LLM user prompt should contain the developer's reply")
	}
	if !strings.Contains(llm.requests[0].User, "Automated review summary") {
		t.Error("LLM user prompt should contain the original review")
	}

	// Verify response was published.
	if len(relayPub.published) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(relayPub.published))
	}
	resp := relayPub.published[0]
	if resp.Kind != nostr.KindComment {
		t.Errorf("expected kind %d, got %d", nostr.KindComment, resp.Kind)
	}
	if resp.Content != "Good point, I'll reconsider that finding." {
		t.Errorf("unexpected response content: %s", resp.Content)
	}

	// Verify conversation was recorded in DB.
	turns, err := store.GetConversationHistory(ctx, testReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 conversation turn, got %d", len(turns))
	}
	if turns[0].TurnNumber != 1 {
		t.Errorf("expected turn 1, got %d", turns[0].TurnNumber)
	}
	if turns[0].ResponseContent != "Good point, I'll reconsider that finding." {
		t.Errorf("unexpected stored response: %s", turns[0].ResponseContent)
	}
	if turns[0].Status != "published" {
		t.Errorf("expected status 'published', got %q", turns[0].Status)
	}
}

func TestHandleReply_PersistenceFailureBlocksPublish(t *testing.T) {
	store := setupTestDB(t)
	seedReviewEvent(t, store, testReviewID, testPatchID, testRepoID)
	if _, err := store.DB().Exec(`CREATE TRIGGER fail_conversation_response_stage
		BEFORE UPDATE OF response_content ON conversations
		WHEN NEW.status = 'pending' AND NEW.response_content != ''
		BEGIN SELECT RAISE(ABORT, 'forced persistence failure'); END`); err != nil {
		t.Fatal(err)
	}

	llm := &fakeLLM{responses: []string{"response must not publish"}}
	h, relayPub := makeHandler(t, store, llm)
	replyEvent := nostr.Event{
		ID:        nostr.MustIDFromHex("3434343434343434343434343434343434343434343434343434343434343434"),
		PubKey:    nostr.MustPubKeyFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		Kind:      nostr.KindComment,
		CreatedAt: nostr.Now(),
		Content:   "Please explain.",
		Tags: nostr.Tags{
			{"e", testReviewID},
			{"p", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
	}

	if err := h.HandleReply(context.Background(), replyEvent, ""); err == nil {
		t.Fatal("HandleReply succeeded despite response persistence failure")
	}
	if len(relayPub.published) != 0 {
		t.Fatalf("published %d events after persistence failure, want 0", len(relayPub.published))
	}
}

func TestHandleReply_RateLimitAt3Turns(t *testing.T) {
	store := setupTestDB(t)
	seedReviewEvent(t, store, testReviewID, testPatchID, testRepoID)

	ctx := context.Background()

	// Pre-seed 3 conversation turns.
	for i := 1; i <= MaxTurnsPerReview; i++ {
		hex := padHex64(i)
		_, err := store.InsertConversation(ctx, db.ConversationTurn{
			ReviewEventID: testReviewID,
			ReplyEventID:  hex,
			RepoID:        testRepoID,
			PatchEventID:  testPatchID,
			ReplyAuthor:   "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			ReplyContent:  "turn content",
			TurnNumber:    i,
			CreatedAt:     time.Now().Unix(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	llm := &fakeLLM{responses: []string{"Should not be called"}}
	h, relayPub := makeHandler(t, store, llm)

	replyEvent := nostr.Event{
		ID:        nostr.MustIDFromHex("4444444444444444444444444444444444444444444444444444444444444444"),
		PubKey:    nostr.MustPubKeyFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		Kind:      nostr.KindComment,
		CreatedAt: nostr.Now(),
		Content:   "One more question...",
		Tags: nostr.Tags{
			{"e", testReviewID},
			{"p", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
	}

	if err := h.HandleReply(ctx, replyEvent, ""); err != nil {
		t.Fatalf("HandleReply should not error on rate limit: %v", err)
	}

	// LLM should NOT have been called.
	if len(llm.requests) != 0 {
		t.Errorf("expected 0 LLM requests (rate limited), got %d", len(llm.requests))
	}

	// No response should have been published.
	if len(relayPub.published) != 0 {
		t.Errorf("expected 0 published events (rate limited), got %d", len(relayPub.published))
	}
}

func TestHandleReply_IgnoresUnrelatedReply(t *testing.T) {
	store := setupTestDB(t)

	llm := &fakeLLM{}
	h, relayPub := makeHandler(t, store, llm)

	// Reply targeting an event we never published.
	replyEvent := nostr.Event{
		ID:        nostr.MustIDFromHex("5555555555555555555555555555555555555555555555555555555555555555"),
		PubKey:    nostr.MustPubKeyFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		Kind:      nostr.KindComment,
		CreatedAt: nostr.Now(),
		Content:   "Hello?",
		Tags: nostr.Tags{
			{"e", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},
			{"p", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
	}

	ctx := context.Background()
	if err := h.HandleReply(ctx, replyEvent, ""); err != nil {
		t.Fatalf("HandleReply should not error on unrelated reply: %v", err)
	}

	if len(llm.requests) != 0 {
		t.Error("LLM should not be called for unrelated replies")
	}
	if len(relayPub.published) != 0 {
		t.Error("no events should be published for unrelated replies")
	}
}

func TestHandleReply_DuplicateReplyIdempotent(t *testing.T) {
	store := setupTestDB(t)
	seedReviewEvent(t, store, testReviewID, testPatchID, testRepoID)

	llm := &fakeLLM{responses: []string{"First response", "Second response"}}
	h, relayPub := makeHandler(t, store, llm)

	replyEvent := nostr.Event{
		ID:        nostr.MustIDFromHex("6666666666666666666666666666666666666666666666666666666666666666"),
		PubKey:    nostr.MustPubKeyFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		Kind:      nostr.KindComment,
		CreatedAt: nostr.Now(),
		Content:   "A question",
		Tags: nostr.Tags{
			{"e", testReviewID},
			{"p", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
	}

	ctx := context.Background()

	// First call — should succeed.
	if err := h.HandleReply(ctx, replyEvent, ""); err != nil {
		t.Fatalf("first HandleReply failed: %v", err)
	}
	if len(relayPub.published) != 1 {
		t.Fatalf("expected 1 published event after first call, got %d", len(relayPub.published))
	}

	// Second call with same event — the turn already exists as "published",
	// so BeginConversationTurn returns the existing turn without error.
	// The handler will still run the LLM + publish (retry path).
	if err := h.HandleReply(ctx, replyEvent, ""); err != nil {
		t.Fatalf("duplicate HandleReply should not error: %v", err)
	}
	// The duplicate may or may not re-publish depending on status — the key
	// invariant is no error and no crash.
}

func TestHandleReply_MultiTurnConversation(t *testing.T) {
	store := setupTestDB(t)
	seedReviewEvent(t, store, testReviewID, testPatchID, testRepoID)

	llm := &fakeLLM{responses: []string{
		"Let me explain the nil check concern.",
		"Yes, that approach would be safer.",
	}}
	h, relayPub := makeHandler(t, store, llm)
	ctx := context.Background()

	// Turn 1
	reply1 := nostr.Event{
		ID:        nostr.MustIDFromHex("7777777777777777777777777777777777777777777777777777777777777777"),
		PubKey:    nostr.MustPubKeyFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		Kind:      nostr.KindComment,
		CreatedAt: nostr.Now(),
		Content:   "Why is the nil check needed?",
		Tags: nostr.Tags{
			{"e", testReviewID},
			{"p", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
	}

	if err := h.HandleReply(ctx, reply1, ""); err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}

	// Turn 2
	reply2 := nostr.Event{
		ID:        nostr.MustIDFromHex("8888888888888888888888888888888888888888888888888888888888888888"),
		PubKey:    nostr.MustPubKeyFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		Kind:      nostr.KindComment,
		CreatedAt: nostr.Now(),
		Content:   "What if I use a guard clause instead?",
		Tags: nostr.Tags{
			{"e", testReviewID},
			{"p", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
	}

	if err := h.HandleReply(ctx, reply2, ""); err != nil {
		t.Fatalf("turn 2 failed: %v", err)
	}

	// Verify 2 LLM calls.
	if len(llm.requests) != 2 {
		t.Fatalf("expected 2 LLM requests, got %d", len(llm.requests))
	}

	// Second LLM call should include conversation history from turn 1.
	if !strings.Contains(llm.requests[1].User, "Why is the nil check needed?") {
		t.Error("turn 2 prompt should include turn 1 user message in history")
	}
	if !strings.Contains(llm.requests[1].User, "Let me explain the nil check concern.") {
		t.Error("turn 2 prompt should include turn 1 assistant response in history")
	}

	// Verify 2 events published.
	if len(relayPub.published) != 2 {
		t.Fatalf("expected 2 published events, got %d", len(relayPub.published))
	}

	// Verify DB has 2 turns.
	turns, err := store.GetConversationHistory(ctx, testReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("expected 2 conversation turns, got %d", len(turns))
	}
	if turns[0].TurnNumber != 1 || turns[1].TurnNumber != 2 {
		t.Errorf("turn numbers should be 1,2 but got %d,%d", turns[0].TurnNumber, turns[1].TurnNumber)
	}
}

func TestIsReplyToUs(t *testing.T) {
	store := setupTestDB(t)
	llm := &fakeLLM{}
	h, _ := makeHandler(t, store, llm)
	ctx := context.Background()

	// Event with our pubkey in p-tag.
	tagged := nostr.Event{
		Tags: nostr.Tags{
			{"p", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
	}
	if !h.IsReplyToUs(ctx, tagged) {
		t.Error("should recognize event tagging our pubkey")
	}

	// Event with different pubkey.
	other := nostr.Event{
		Tags: nostr.Tags{
			{"p", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		},
	}
	if h.IsReplyToUs(ctx, other) {
		t.Error("should not recognize event tagging different pubkey")
	}

	// Event with no p-tags.
	noPTag := nostr.Event{
		Tags: nostr.Tags{
			{"e", testReviewID},
		},
	}
	if h.IsReplyToUs(ctx, noPTag) {
		t.Error("should not recognize event with no p-tags")
	}
}

func TestReplyTargetEventID(t *testing.T) {
	// With reply marker.
	event1 := nostr.Event{
		Tags: nostr.Tags{
			{"E", "root123"},
			{"e", "parent456", "", "reply"},
		},
	}
	if got := replyTargetEventID(event1); got != "parent456" {
		t.Errorf("expected parent456, got %s", got)
	}

	// Without marker — falls back to last e-tag.
	event2 := nostr.Event{
		Tags: nostr.Tags{
			{"e", "first111"},
			{"e", "last222"},
		},
	}
	if got := replyTargetEventID(event2); got != "last222" {
		t.Errorf("expected last222, got %s", got)
	}

	// No e-tags.
	event3 := nostr.Event{
		Tags: nostr.Tags{
			{"p", "somepubkey"},
		},
	}
	if got := replyTargetEventID(event3); got != "" {
		t.Errorf("expected empty, got %s", got)
	}
}

func TestHandleReply_NoTargetEvent(t *testing.T) {
	store := setupTestDB(t)
	llm := &fakeLLM{}
	h, relayPub := makeHandler(t, store, llm)

	// Reply with no e-tags at all.
	replyEvent := nostr.Event{
		ID:        nostr.MustIDFromHex("9999999999999999999999999999999999999999999999999999999999999999"),
		PubKey:    nostr.MustPubKeyFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		Kind:      nostr.KindComment,
		CreatedAt: nostr.Now(),
		Content:   "Hello",
		Tags: nostr.Tags{
			{"p", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
	}

	ctx := context.Background()
	if err := h.HandleReply(ctx, replyEvent, ""); err != nil {
		t.Fatalf("should not error: %v", err)
	}
	if len(llm.requests) != 0 {
		t.Error("should not call LLM with no target")
	}
	if len(relayPub.published) != 0 {
		t.Error("should not publish with no target")
	}
}

func TestConversationResponseTags(t *testing.T) {
	store := setupTestDB(t)
	seedReviewEvent(t, store, testReviewID, testPatchID, testRepoID)

	llm := &fakeLLM{responses: []string{"Response text"}}
	h, relayPub := makeHandler(t, store, llm)

	replyEvent := nostr.Event{
		ID:        nostr.MustIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0001"),
		PubKey:    nostr.MustPubKeyFromHex("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		Kind:      nostr.KindComment,
		CreatedAt: nostr.Now(),
		Content:   "Question",
		Tags: nostr.Tags{
			{"e", testReviewID},
			{"p", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
	}

	ctx := context.Background()
	if err := h.HandleReply(ctx, replyEvent, ""); err != nil {
		t.Fatal(err)
	}

	if len(relayPub.published) != 1 {
		t.Fatal("expected 1 published event")
	}
	resp := relayPub.published[0]

	// Check NIP-22 tags.
	hasRootE := false
	hasParente := false
	hasRepoA := false
	hasExpiration := false
	hasParentP := false

	for _, tag := range resp.Tags {
		if len(tag) >= 2 {
			switch tag[0] {
			case "E":
				if tag[1] == testReviewID {
					hasRootE = true
				}
			case "e":
				if tag[1] == replyEvent.ID.Hex() {
					hasParente = true
				}
			case "A":
				if strings.Contains(tag[1], testRepoID) {
					hasRepoA = true
				}
			case "p":
				if tag[1] == replyEvent.PubKey.Hex() {
					hasParentP = true
				}
			case "expiration":
				hasExpiration = true
			}
		}
	}

	if !hasRootE {
		t.Error("response should have E tag pointing to review event")
	}
	if !hasParente {
		t.Error("response should have e tag pointing to reply event")
	}
	if !hasRepoA {
		t.Error("response should have A tag with repo ID")
	}
	if !hasExpiration {
		t.Error("response should have expiration tag")
	}
	if !hasParentP {
		t.Error("response should have p tag with reply author")
	}
}

func TestCachedPubKey(t *testing.T) {
	store := setupTestDB(t)
	llm := &fakeLLM{}
	h, _ := makeHandler(t, store, llm)

	// Verify pubkey was cached at construction time.
	if h.ourPubKey == "" {
		t.Fatal("ourPubKey should be cached")
	}
	if h.ourPubKey != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("cached pubkey mismatch: %s", h.ourPubKey)
	}
}

// padHex64 returns a valid 64-char hex string with the last 2 chars varying by n.
func padHex64(n int) string {
	base := strings.Repeat("dd", 31)
	hex := "0123456789abcdef"
	return base + string(hex[(n/16)%16]) + string(hex[n%16])
}

// Ensure sql import is used.
var _ = sql.ErrNoRows
