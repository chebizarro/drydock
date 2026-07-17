package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"fiatjaf.com/nostr"
)

const (
	KindCASAudit             nostr.Kind = 4903
	KindClientAuthentication nostr.Kind = 22242
)

// AuditInput describes one consequential action to record as a CAS audit event.
type AuditInput struct {
	Action  string
	Subject string
	Tags    map[string]string
	Relays  []string
}

// AuditPublisher emits signed kind-4903 audit events via the configured signer.
type AuditPublisher struct {
	signer  Signer
	publish RelayPublisher
	relays  []string
	logger  *slog.Logger
	now     func() time.Time
}

func NewAuditPublisher(signer Signer, relayPublisher RelayPublisher, relays []string, logger *slog.Logger) *AuditPublisher {
	if signer == nil || relayPublisher == nil || len(relays) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &AuditPublisher{
		signer:  signer,
		publish: relayPublisher,
		relays:  append([]string(nil), relays...),
		logger:  logger,
		now:     time.Now,
	}
}

// EmitAudit implements contextbuilder.AuditEmitter.
func (p *AuditPublisher) EmitAudit(ctx context.Context, action, subject string, tags map[string]string) error {
	return p.Publish(ctx, AuditInput{Action: action, Subject: subject, Tags: tags})
}

func (p *AuditPublisher) Publish(ctx context.Context, in AuditInput) error {
	if p == nil {
		return nil
	}
	if in.Action == "" {
		return fmt.Errorf("audit action is required")
	}
	if in.Subject == "" {
		return fmt.Errorf("audit subject is required")
	}
	now := p.now
	if now == nil {
		now = time.Now
	}
	ts := now().UTC()
	content, err := json.Marshal(map[string]any{
		"action":    in.Action,
		"subject":   in.Subject,
		"timestamp": ts.Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}

	tags := nostr.Tags{
		{"domain", "drydock"},
		{"type", in.Action},
		{"schema", "cascadia.audit.v1"},
		{"subject", in.Subject},
	}
	for k, v := range in.Tags {
		if k == "" || v == "" {
			continue
		}
		tags = append(tags, nostr.Tag{k, v})
	}

	evt := nostr.Event{
		Kind:      KindCASAudit,
		CreatedAt: nostr.Timestamp(ts.Unix()),
		Tags:      tags,
		Content:   string(content),
	}
	if err := p.signer.SignEvent(ctx, &evt); err != nil {
		return fmt.Errorf("sign audit event: %w", err)
	}
	relays := in.Relays
	if len(relays) == 0 {
		relays = p.relays
	}
	if err := p.publish.Publish(ctx, relays, evt); err != nil {
		return fmt.Errorf("publish audit event: %w", err)
	}
	p.logger.Info("audit event published", "action", in.Action, "subject", in.Subject, "audit_event_id", evt.ID.Hex())
	return nil
}

// AuditedSigner wraps a signer and emits a best-effort audit event for each
// non-audit, non-auth event it signs. Audit events are signed by the underlying
// signer directly to avoid recursive audit emission.
type AuditedSigner struct {
	base       Signer
	audit      *AuditPublisher
	log        *slog.Logger
	failClosed bool
	failures   atomic.Uint64
}

type AuditedSignerOptions struct {
	// FailClosed makes signing fail when its audit event cannot be delivered.
	// The default remains best-effort for existing callers.
	FailClosed bool
}

func NewAuditedSigner(base Signer, audit *AuditPublisher, logger *slog.Logger) Signer {
	return NewAuditedSignerWithOptions(base, audit, logger, AuditedSignerOptions{})
}

func NewAuditedSignerWithOptions(base Signer, audit *AuditPublisher, logger *slog.Logger, opts AuditedSignerOptions) Signer {
	if base == nil || audit == nil {
		return base
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &AuditedSigner{base: base, audit: audit, log: logger, failClosed: opts.FailClosed}
}

// AuditFailureCount returns the number of signing-audit delivery failures.
func (s *AuditedSigner) AuditFailureCount() uint64 {
	if s == nil {
		return 0
	}
	return s.failures.Load()
}

func (s *AuditedSigner) GetPublicKey(ctx context.Context) (nostr.PubKey, error) {
	return s.base.GetPublicKey(ctx)
}

func (s *AuditedSigner) SignEvent(ctx context.Context, evt *nostr.Event) error {
	if err := s.base.SignEvent(ctx, evt); err != nil {
		return err
	}
	if evt == nil || evt.Kind == KindCASAudit || evt.Kind == KindClientAuthentication {
		return nil
	}
	if err := s.audit.Publish(ctx, AuditInput{
		Action:  "event-signed",
		Subject: evt.ID.Hex(),
		Tags: map[string]string{
			"event_kind": strconv.Itoa(int(evt.Kind)),
		},
	}); err != nil {
		s.failures.Add(1)
		if s.failClosed {
			return fmt.Errorf("publish signing audit: %w", err)
		}
		s.log.Warn("failed to publish signing audit", "event_id", evt.ID.Hex(), "event_kind", int(evt.Kind), "error", err)
	}
	return nil
}

type encryptionCapableSigner interface {
	Encrypt(ctx context.Context, plaintext string, recipient nostr.PubKey) (string, error)
	Decrypt(ctx context.Context, base64ciphertext string, sender nostr.PubKey) (string, error)
}

func (s *AuditedSigner) Encrypt(ctx context.Context, plaintext string, recipient nostr.PubKey) (string, error) {
	enc, ok := s.base.(encryptionCapableSigner)
	if !ok {
		return "", fmt.Errorf("underlying signer does not support encryption")
	}
	return enc.Encrypt(ctx, plaintext, recipient)
}

func (s *AuditedSigner) Decrypt(ctx context.Context, base64ciphertext string, sender nostr.PubKey) (string, error) {
	enc, ok := s.base.(encryptionCapableSigner)
	if !ok {
		return "", fmt.Errorf("underlying signer does not support decryption")
	}
	return enc.Decrypt(ctx, base64ciphertext, sender)
}
