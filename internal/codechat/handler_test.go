package codechat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"drydock/internal/db"
	"drydock/internal/ratelimit"
	"drydock/internal/reviewengine"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip59"
)

func TestParseMessage_RepoPrefix(t *testing.T) {
	h := &Handler{}

	cases := []struct {
		input    string
		wantRepo string
		wantQ    string
	}{
		{
			input:    "repo:npub1abc/myrepo what does foo do?",
			wantRepo: "npub1abc/myrepo",
			wantQ:    "what does foo do?",
		},
		{
			input:    "@npub1xyz/project how does auth work?",
			wantRepo: "npub1xyz/project",
			wantQ:    "how does auth work?",
		},
		{
			input:    "what does the main function do?",
			wantRepo: "",
			wantQ:    "what does the main function do?",
		},
		{
			input:    "repo:myrepo",
			wantRepo: "myrepo",
			wantQ:    "",
		},
		{
			input:    "",
			wantRepo: "",
			wantQ:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			result := h.parseMessage(tc.input)
			if result.repoID != tc.wantRepo {
				t.Errorf("repoID: got %q, want %q", result.repoID, tc.wantRepo)
			}
			if result.question != tc.wantQ {
				t.Errorf("question: got %q, want %q", result.question, tc.wantQ)
			}
		})
	}
}

type responseKeyer struct{}

func (responseKeyer) GetPublicKey(context.Context) (nostr.PubKey, error) {
	return nostr.MustPubKeyFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), nil
}
func (responseKeyer) SignEvent(_ context.Context, event *nostr.Event) error {
	event.ID = event.GetID()
	return nil
}
func (responseKeyer) Encrypt(context.Context, string, nostr.PubKey) (string, error) {
	return "ciphertext", nil
}
func (responseKeyer) Decrypt(context.Context, string, nostr.PubKey) (string, error) {
	return "repo:test/repo what does main do?", nil
}

type responseLLM struct{}

func (responseLLM) ChatCompletion(context.Context, reviewengine.ChatRequest) (string, error) {
	return "main starts the service", nil
}

type recordingPublisher struct{ events []nostr.Event }

func (p *recordingPublisher) Publish(_ context.Context, _ []string, event nostr.Event) error {
	p.events = append(p.events, event)
	return nil
}

func TestHandleDM_PersistenceFailureBlocksPublish(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`CREATE TRIGGER fail_codechat_response_stage
		BEFORE UPDATE OF response ON codechat_turns
		WHEN NEW.status = 'pending' AND NEW.response != ''
		BEGIN SELECT RAISE(ABORT, 'forced persistence failure'); END`); err != nil {
		t.Fatal(err)
	}

	publisher := &recordingPublisher{}
	h := New(Config{}, store, nil, nil, responseLLM{}, responseKeyer{}, publisher,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	event := nostr.Event{
		ID:        nostr.MustIDFromHex("1111111111111111111111111111111111111111111111111111111111111111"),
		PubKey:    nostr.MustPubKeyFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Kind:      kindPrivateDirectMessage,
		CreatedAt: nostr.Now(),
		Content:   "repo:test/repo what does main do?",
		Tags:      nostr.Tags{{"p", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
	}

	if err := h.HandleDM(ctx, event, ""); err == nil {
		t.Fatal("HandleDM succeeded despite response persistence failure")
	}
	if len(publisher.events) != 0 {
		t.Fatalf("published %d events after persistence failure, want 0", len(publisher.events))
	}
}

func TestSendEncryptedResponseUsesNIP17GiftWrap(t *testing.T) {
	ctx := context.Background()
	sender := keyer.NewPlainKeySigner(nostr.Generate())
	recipient := keyer.NewPlainKeySigner(nostr.Generate())
	recipientPubKey, err := recipient.GetPublicKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	senderPubKey, err := sender.GetPublicKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	publisher := &recordingPublisher{}
	h := &Handler{
		cfg:     Config{DefaultRelays: []string{"wss://relay.test"}},
		keyer:   sender,
		publish: publisher,
	}
	incoming := nostr.Event{
		ID:     nostr.MustIDFromHex("2222222222222222222222222222222222222222222222222222222222222222"),
		PubKey: recipientPubKey,
		Kind:   kindPrivateDirectMessage,
	}

	if err := h.sendEncryptedResponse(ctx, incoming, "end-to-end response", ""); err != nil {
		t.Fatalf("sendEncryptedResponse failed: %v", err)
	}
	if len(publisher.events) != 1 {
		t.Fatalf("published events = %d, want 1", len(publisher.events))
	}
	wrapper := publisher.events[0]
	if wrapper.Kind != nostr.KindGiftWrap {
		t.Fatalf("outer kind = %d, want %d", wrapper.Kind, nostr.KindGiftWrap)
	}
	if !wrapper.VerifySignature() {
		t.Fatal("gift wrap has invalid ephemeral signature")
	}
	if len(wrapper.Tags) != 1 || len(wrapper.Tags[0]) < 2 || wrapper.Tags[0][0] != "p" || wrapper.Tags[0][1] != recipientPubKey.Hex() {
		t.Fatalf("gift wrap leaks non-routing tags: %#v", wrapper.Tags)
	}

	rumor, err := nip59.GiftUnwrap(wrapper, func(other nostr.PubKey, ciphertext string) (string, error) {
		return recipient.Decrypt(ctx, ciphertext, other)
	})
	if err != nil {
		t.Fatalf("GiftUnwrap failed: %v", err)
	}
	if rumor.Kind != kindPrivateDirectMessage {
		t.Fatalf("rumor kind = %d, want %d", rumor.Kind, kindPrivateDirectMessage)
	}
	if rumor.PubKey != senderPubKey {
		t.Fatalf("rumor pubkey = %s, want %s", rumor.PubKey.Hex(), senderPubKey.Hex())
	}
	if rumor.Content != "end-to-end response" {
		t.Fatalf("rumor content = %q", rumor.Content)
	}
	if !rumor.Tags.ContainsAny("p", []string{recipientPubKey.Hex()}) {
		t.Fatalf("rumor missing recipient tag: %#v", rumor.Tags)
	}
	if !rumor.Tags.ContainsAny("e", []string{incoming.ID.Hex()}) {
		t.Fatalf("rumor missing reply reference: %#v", rumor.Tags)
	}
}

func TestHandleDM_RateLimiterEnforced(t *testing.T) {
	ctx := context.Background()
	event := nostr.Event{}
	limiter := ratelimit.New(ratelimit.Config{
		Window:      time.Hour,
		MaxRequests: 1,
		KeyPrefix:   "handler-test:",
	}, ratelimit.NewMemoryStore())
	if result, err := limiter.Allow(ctx, event.PubKey.Hex()); err != nil || !result.Allowed {
		t.Fatalf("pre-consume rate limit: result=%+v err=%v", result, err)
	}

	keyer := &trackingKeyer{}
	h := &Handler{
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		sem:         make(chan struct{}, 1),
		keyer:       keyer,
		rateLimiter: limiter,
	}
	if err := h.HandleDM(ctx, event, ""); err != nil {
		t.Fatalf("HandleDM returned error: %v", err)
	}
	if keyer.decryptCalls != 0 {
		t.Fatalf("rate-limited request reached decryption path %d times", keyer.decryptCalls)
	}
}

func TestHandleDM_RateLimiterBackendFailureDenies(t *testing.T) {
	keyer := &trackingKeyer{}
	h := &Handler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sem:    make(chan struct{}, 1),
		keyer:  keyer,
		rateLimiter: ratelimit.New(ratelimit.Config{
			Window:      time.Hour,
			MaxRequests: 1,
			KeyPrefix:   "handler-failure-test:",
		}, failingRateLimitStore{}),
	}

	if err := h.HandleDM(context.Background(), nostr.Event{}, ""); err != nil {
		t.Fatalf("HandleDM returned error: %v", err)
	}
	if keyer.decryptCalls != 0 {
		t.Fatalf("request bypassed failed limiter and reached decryption path %d times", keyer.decryptCalls)
	}
}

type trackingKeyer struct {
	decryptCalls int
}

func (k *trackingKeyer) GetPublicKey(context.Context) (nostr.PubKey, error) {
	return nostr.PubKey{}, nil
}

func (k *trackingKeyer) SignEvent(context.Context, *nostr.Event) error { return nil }

func (k *trackingKeyer) Encrypt(context.Context, string, nostr.PubKey) (string, error) {
	return "", nil
}

func (k *trackingKeyer) Decrypt(context.Context, string, nostr.PubKey) (string, error) {
	k.decryptCalls++
	return "", errors.New("unexpected decrypt")
}

type failingRateLimitStore struct{}

func (failingRateLimitStore) GetRateLimitCount(context.Context, string, int64) (int, error) {
	return 0, errors.New("backend unavailable")
}

func (failingRateLimitStore) IncrementRateLimit(context.Context, string, int64) error {
	return errors.New("backend unavailable")
}

func (failingRateLimitStore) CheckAndIncrementRateLimit(context.Context, string, int64, int64, int) (int, bool, error) {
	return 0, false, errors.New("backend unavailable")
}

func (failingRateLimitStore) CleanupOldRateLimits(context.Context, int64) (int64, error) {
	return 0, errors.New("backend unavailable")
}

func TestPayloadInt(t *testing.T) {
	cases := []struct {
		payload map[string]any
		key     string
		want    int
	}{
		{map[string]any{"line": float64(42)}, "line", 42},
		{map[string]any{"line": 42}, "line", 42},
		{map[string]any{"line": int64(42)}, "line", 42},
		{map[string]any{}, "line", 0},
		{map[string]any{"line": "not a number"}, "line", 0},
	}

	for _, tc := range cases {
		got := payloadInt(tc.payload, tc.key)
		if got != tc.want {
			t.Errorf("payloadInt(%v, %q) = %d, want %d", tc.payload, tc.key, got, tc.want)
		}
	}
}
