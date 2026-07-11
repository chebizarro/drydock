package publisher

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

type auditTestSigner struct {
	sk nostr.SecretKey
}

func (s auditTestSigner) GetPublicKey(context.Context) (nostr.PubKey, error) {
	return nostr.GetPublicKey(s.sk), nil
}

func (s auditTestSigner) SignEvent(_ context.Context, evt *nostr.Event) error {
	return evt.Sign(s.sk)
}

type auditTestPublisher struct {
	events []nostr.Event
	relays [][]string
}

func (p *auditTestPublisher) Publish(_ context.Context, relays []string, event nostr.Event) error {
	p.events = append(p.events, event)
	p.relays = append(p.relays, append([]string(nil), relays...))
	return nil
}

func TestAuditPublisherPublishesKind4903(t *testing.T) {
	pub := &auditTestPublisher{}
	audit := NewAuditPublisher(auditTestSigner{sk: nostr.Generate()}, pub, []string{"wss://relay.test"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	audit.now = func() time.Time { return time.Unix(1234, 0).UTC() }

	if err := audit.Publish(context.Background(), AuditInput{
		Action:  "review-published",
		Subject: "event-1",
		Tags:    map[string]string{"repo_id": "repo-1"},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(pub.events) != 1 {
		t.Fatalf("published events = %d", len(pub.events))
	}
	evt := pub.events[0]
	if evt.Kind != KindCASAudit {
		t.Fatalf("kind = %d, want %d", evt.Kind, KindCASAudit)
	}
	if evt.ID.Hex() == "" || evt.Sig == ([64]byte{}) {
		t.Fatalf("audit event not signed: id=%s", evt.ID.Hex())
	}
	var content map[string]string
	if err := json.Unmarshal([]byte(evt.Content), &content); err != nil {
		t.Fatalf("content json: %v", err)
	}
	if content["action"] != "review-published" || content["subject"] != "event-1" || content["timestamp"] != "1970-01-01T00:20:34Z" {
		t.Fatalf("unexpected content: %#v", content)
	}
	if !hasTag(evt.Tags, "domain", "drydock") || !hasTag(evt.Tags, "type", "review-published") || !hasTag(evt.Tags, "schema", "cascadia.audit.v1") || !hasTag(evt.Tags, "repo_id", "repo-1") {
		t.Fatalf("missing expected tags: %#v", evt.Tags)
	}
}

func TestAuditedSignerAuditsNonAuditEvents(t *testing.T) {
	pub := &auditTestPublisher{}
	base := auditTestSigner{sk: nostr.Generate()}
	audit := NewAuditPublisher(base, pub, []string{"wss://relay.test"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	signer := NewAuditedSigner(base, audit, slog.New(slog.NewTextHandler(io.Discard, nil)))

	evt := nostr.Event{Kind: nostr.KindComment, CreatedAt: nostr.Now(), Content: "review"}
	if err := signer.SignEvent(context.Background(), &evt); err != nil {
		t.Fatalf("SignEvent: %v", err)
	}
	if evt.ID.Hex() == "" {
		t.Fatal("event was not signed")
	}
	if len(pub.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(pub.events))
	}
	if pub.events[0].Kind != KindCASAudit || !hasTag(pub.events[0].Tags, "type", "event-signed") || !hasTag(pub.events[0].Tags, "event_kind", "1111") {
		t.Fatalf("unexpected signing audit: %#v", pub.events[0])
	}

	auditEvt := nostr.Event{Kind: KindCASAudit, CreatedAt: nostr.Now(), Content: "{}"}
	if err := signer.SignEvent(context.Background(), &auditEvt); err != nil {
		t.Fatalf("SignEvent audit: %v", err)
	}
	if len(pub.events) != 1 {
		t.Fatalf("audit signer should not recursively audit kind 4903; got %d events", len(pub.events))
	}
}

func hasTag(tags nostr.Tags, key, value string) bool {
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == key && tag[1] == value {
			return true
		}
	}
	return false
}
