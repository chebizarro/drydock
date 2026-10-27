package publisher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"fiatjaf.com/nostr/nip44"
	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
	"git.sharegap.net/cascadia/drydock/internal/repoconfig"
)

func concordFixture() (*repoconfig.ConcordConfig, nostr.SecretKey, string) {
	epoch := uint64(7)
	cfg := &repoconfig.ConcordConfig{Community: strings.Repeat("a", 64), Channel: strings.Repeat("b", 64), Epoch: &epoch, Relays: []string{"wss://community.example"}}
	key := nostr.MustSecretKeyFromHex(strings.Repeat("0", 63) + "2")
	raw, _ := json.Marshal(map[string]string{cfg.CredentialID(): key.Hex()})
	return cfg, key, string(raw)
}
func openConcordFixture(t *testing.T, wrap nostr.Event, stream nostr.SecretKey) (nostr.Event, nostr.Event, map[string]json.RawMessage) {
	t.Helper()
	key, err := nip44.GenerateConversationKey(stream.Public(), stream)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := nip44.Decrypt(wrap.Content, key)
	if err != nil {
		t.Fatal(err)
	}
	var seal nostr.Event
	if err := json.Unmarshal([]byte(raw), &seal); err != nil {
		t.Fatal(err)
	}
	raw, err = nip44.Decrypt(seal.Content, key)
	if err != nil {
		t.Fatal(err)
	}
	var rumor nostr.Event
	if err := json.Unmarshal([]byte(raw), &rumor); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	return seal, rumor, fields
}
func finishConcordFixture(t *testing.T, rumor nostr.Event, stream, author nostr.SecretKey, epoch uint64, channel string, ephemeral nostr.PubKey, nonces [2][32]byte) (nostr.Event, error) {
	t.Helper()
	prepared, err := concordEnvelope(rumor, stream, epoch, channel, ephemeral, nonces, nil)
	if err != nil {
		return nostr.Event{}, err
	}
	if err := prepared.Seal.Sign(author); err != nil {
		return nostr.Event{}, err
	}
	complete, err := concordEnvelope(rumor, stream, epoch, channel, ephemeral, nonces, &prepared.Seal.Sig)
	return complete.Wrap, err
}

func TestConcordEnvelopeCORD01AndCORD03Fixture(t *testing.T) {
	cfg, stream, _ := concordFixture()
	author := nostr.MustSecretKeyFromHex(strings.Repeat("0", 63) + "1")
	ephemeral := nostr.MustSecretKeyFromHex(strings.Repeat("0", 63) + "3").Public()
	content := "Drydock review published: nostr:" + strings.Repeat("c", 64)
	rumor := nostr.Event{Kind: eventkind.ConcordMessage, PubKey: author.Public(), CreatedAt: 1700000000, Content: content, Tags: nostr.Tags{{"ms", "417"}}}
	var nonces [2][32]byte
	nonces[0][0] = 1
	nonces[1][0] = 2
	wrap, err := finishConcordFixture(t, rumor, stream, author, *cfg.Epoch, cfg.Channel, ephemeral, nonces)
	if err != nil {
		t.Fatal(err)
	}
	if wrap.Kind != eventkind.GiftWrap || wrap.PubKey != stream.Public() || !wrap.VerifySignature() || !wrap.CheckID() {
		t.Fatalf("invalid stream wrapper: %#v", wrap)
	}
	if !reflect.DeepEqual(wrap.Tags, nostr.Tags{{"p", ephemeral.Hex()}}) {
		t.Fatalf("public tags: %#v", wrap.Tags)
	}
	seal, inner, fields := openConcordFixture(t, wrap, stream)
	if seal.Kind != eventkind.ConcordEncryptedSeal || seal.PubKey != author.Public() || !seal.VerifySignature() || !seal.CheckID() || len(seal.Tags) != 0 {
		t.Fatalf("seal: %#v", seal)
	}
	if _, ok := fields["sig"]; ok {
		t.Fatal("rumor must omit signature field entirely")
	}
	if inner.ID.Hex() != "c9d33390b5515c0a2809ce568fb1e3710f1b1e1cdfaac1da18d4293becb28510" {
		t.Fatal("rumor differs from fixed canonical-hash fixture")
	}
	expectedTags := nostr.Tags{{"ms", "417"}, {"channel", cfg.Channel}, {"epoch", "7"}}
	if inner.Kind != eventkind.ConcordMessage || inner.PubKey != author.Public() || inner.Content != content || !inner.CheckID() || !reflect.DeepEqual(inner.Tags, expectedTags) {
		t.Fatalf("rumor: %#v", inner)
	}
	for _, at := range []nostr.Timestamp{wrap.CreatedAt, seal.CreatedAt, inner.CreatedAt} {
		if at != 1700000000 {
			t.Fatal("Concord timestamps must not be tweaked")
		}
	}
	wrongKey, err := nip44.GenerateConversationKey(ephemeral, stream)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nip44.Decrypt(wrap.Content, wrongKey); err == nil {
		t.Fatal("must encrypt with self-ECDH, NOT the p recipient")
	}
	if strings.Contains(wrap.Content, content) || strings.Contains(seal.Content, content) {
		t.Fatal("both layers must be encrypted")
	}
	// Rotation is explicit; the prior epoch's stream credential cannot decrypt it.
	rotated := nostr.MustSecretKeyFromHex(strings.Repeat("0", 63) + "4")
	next, err := finishConcordFixture(t, rumor, rotated, author, 8, cfg.Channel, ephemeral, nonces)
	if err != nil {
		t.Fatal(err)
	}
	oldKey, _ := nip44.GenerateConversationKey(stream.Public(), stream)
	if _, err := nip44.Decrypt(next.Content, oldKey); err == nil {
		t.Fatal("old key decrypted rotated envelope")
	}
	_, nextRumor, _ := openConcordFixture(t, next, rotated)
	if nextRumor.Tags.Find("epoch")[1] != "8" {
		t.Fatal("epoch not bound to rumor")
	}
	invalidSignature := [64]byte{}
	_, err = concordEnvelope(rumor, stream, 7, cfg.Channel, ephemeral, nonces, &invalidSignature)
	if err == nil || strings.Contains(err.Error(), stream.Hex()) {
		t.Fatal("invalid seal signature accepted or input exposed")
	}
}

type blockedConcordPublisher struct {
	entered chan publishCall
	release <-chan struct{}
}

func (p blockedConcordPublisher) Publish(ctx context.Context, relays []string, e nostr.Event) error {
	p.entered <- publishCall{relays: relays, event: e}
	select {
	case <-p.release:
		return errors.New("synthetic failure")
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestConcordDoesNotBlockOrFailReviewPublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := mustStore(t, ctx)
	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	if _, err := store.BeginReviewWithClaim(ctx, patchID, repoID, db.ReviewClaim{}); err != nil {
		t.Fatal(err)
	}
	cfg, stream, raw := concordFixture()
	entered := make(chan publishCall, 1)
	release := make(chan struct{})
	signer := fakeSigner{sk: nostr.Generate()}
	logs := make(chan string, 10)
	delivery, err := NewConcordDelivery(ctx, raw, signer, blockedConcordPublisher{entered: entered, release: release}, slog.New(concordLogHandler{logs: logs}))
	if err != nil {
		t.Fatal(err)
	}
	reviewPub := &fakeRelayPublisher{}
	svc := New(Config{DefaultRelays: []string{"wss://reviews.example"}, Concord: delivery}, store, signer, reviewPub, slog.Default())
	// Publication returns even though the Concord transport cannot return yet.
	id, err := svc.PublishReview(ctx, PublishInput{PatchEventID: patchID, RepoID: repoID, Summary: "review", Model: "test", Concord: cfg})
	if err != nil || id == "" {
		t.Fatalf("review failed: %s %v", id, err)
	}
	select {
	case call := <-entered:
		if !reflect.DeepEqual(call.relays, cfg.Relays) {
			t.Fatalf("wrong relays: %v", call.relays)
		}
		_, rumor, _ := openConcordFixture(t, call.event, stream)
		eventID, _ := nostr.IDFromHex(id)
		if rumor.Content != "Drydock review published: nostr:"+nip19.EncodeNevent(eventID, reviewPub.calls[0].relays, signer.sk.Public()) {
			t.Fatalf("notice content: %s", rumor.Content)
		}
		if strings.Contains(fmt.Sprint(reviewPub.calls), stream.Hex()) {
			t.Fatal("stream key leaked into review")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notification not attempted")
	}
	close(release)
	select {
	case msg := <-logs:
		if !strings.Contains(msg, "delivery failed") {
			t.Fatalf("unexpected warning: %s", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delivery failure not logged")
	}
}

type concordLogHandler struct{ logs chan string }

func (h concordLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h concordLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.logs <- r.Message
	return nil
}
func (h concordLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h concordLogHandler) WithGroup(string) slog.Handler      { return h }

func TestConcordDisabledAndMissingKeyNoOp(t *testing.T) {
	cfg, stream, _ := concordFixture()
	var disabled *ConcordDelivery
	disabled.Notify(cfg, strings.Repeat("c", 64))
	var logs bytes.Buffer
	d, err := NewConcordDelivery(context.Background(), "", nil, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	d.Notify(nil, "id")
	d.Notify(cfg, "id")
	d.Notify(cfg, "id")
	if strings.Count(logs.String(), "credential missing") != 1 {
		t.Fatalf("want one warning: %s", logs.String())
	}
	if strings.Contains(logs.String(), stream.Hex()) || len(d.slots) != 0 {
		t.Fatal("missing key attempted delivery or leaked a credential")
	}
}
func TestPublishReviewConcordDisabledIgnoresRepositoryOptIn(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, ctx)
	patchID, repoID := seedRepoAndPatch(t, ctx, store)
	if _, err := store.BeginReviewWithClaim(ctx, patchID, repoID, db.ReviewClaim{}); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ := concordFixture()
	pub := &fakeRelayPublisher{}
	svc := New(Config{DefaultRelays: []string{"wss://reviews.example"}}, store, fakeSigner{sk: nostr.Generate()}, pub, slog.Default())
	if _, err := svc.PublishReview(ctx, PublishInput{PatchEventID: patchID, RepoID: repoID, Summary: "review", Model: "test", Concord: cfg}); err != nil {
		t.Fatal(err)
	}
	if len(pub.calls) != 1 || pub.calls[0].event.Kind != eventkind.Comment {
		t.Fatalf("disabled publisher emitted optional notice: %#v", pub.calls)
	}
}

func TestConcordCredentialErrorsDoNotEchoInput(t *testing.T) {
	_, stream, valid := concordFixture()
	if _, err := NewConcordDelivery(context.Background(), valid, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"secret":"` + stream.Hex() + `"}`, `{"` + strings.Repeat("a", 64) + `/` + strings.Repeat("b", 64) + `/7":"` + stream.Hex() + `INVALID"}`, `{"` + stream.Hex() + `":`, "null"} {
		_, err := NewConcordDelivery(context.Background(), raw, nil, nil, nil)
		if err == nil || strings.Contains(err.Error(), stream.Hex()) {
			t.Fatalf("unsafe parse result: %v", err)
		}
	}
}
