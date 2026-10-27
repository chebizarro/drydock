package publisher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
	"git.sharegap.net/cascadia/drydock/internal/relayroute"
	"git.sharegap.net/cascadia/drydock/internal/repoconfig"
	"git.sharegap.net/cascadia/drydock/internal/signing"
)

// ConcordDelivery is best-effort and bounded: no outbox/retry guarantee. Secrets
// are loaded only from the operator environment by the composition root.
type ConcordDelivery struct {
	ctx     context.Context
	keys    map[string]nostr.SecretKey
	signer  signing.Signer
	publish RelayPublisher
	// policy is the egress gate for repository-declared Concord relays; the
	// zero value is the strict production policy (wss only, no private hosts).
	policy  relayroute.Policy
	logger  *slog.Logger
	missing sync.Map
	slots   chan struct{}
}

func (*ConcordDelivery) String() string { return "ConcordDelivery{credentials:redacted}" }

// NewConcordDelivery accepts DRYDOCK_CONCORD_STREAM_KEYS: a JSON object keyed by
// community_id/channel_id/epoch, values being pre-derived stream secret keys.
// Parse errors intentionally never include input or library errors (which can echo secrets).
func NewConcordDelivery(ctx context.Context, raw string, signer signing.Signer, publish RelayPublisher, logger *slog.Logger) (*ConcordDelivery, error) {
	keys := map[string]nostr.SecretKey{}
	if strings.TrimSpace(raw) != "" {
		var entries map[string]string
		if err := json.Unmarshal([]byte(raw), &entries); err != nil || entries == nil {
			return nil, errors.New("invalid DRYDOCK_CONCORD_STREAM_KEYS JSON object")
		}
		for id, value := range entries {
			parts := strings.Split(id, "/")
			if len(parts) != 3 {
				return nil, errors.New("invalid Concord credential locator")
			}
			epoch, err := strconv.ParseUint(parts[2], 10, 64)
			if err != nil || strconv.FormatUint(epoch, 10) != parts[2] {
				return nil, errors.New("invalid Concord credential epoch")
			}
			for _, part := range parts[:2] {
				if len(part) != 64 || part != strings.ToLower(part) {
					return nil, errors.New("invalid Concord credential locator")
				}
				if _, err := hex.DecodeString(part); err != nil {
					return nil, errors.New("invalid Concord credential locator")
				}
			}
			if len(value) != 64 {
				return nil, errors.New("invalid Concord credential shape")
			}
			key, err := nostr.SecretKeyFromHex(value)
			if err != nil {
				return nil, errors.New("invalid Concord stream credential")
			}
			keys[id] = key
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ConcordDelivery{ctx: ctx, keys: keys, signer: signer, publish: publish, logger: logger, slots: make(chan struct{}, 4)}, nil
}

// WithRelayPolicy installs the operator egress policy (the same one relay
// routing uses) so a repository's notification relays cannot reach anything
// routing would refuse.
func (d *ConcordDelivery) WithRelayPolicy(policy relayroute.Policy) *ConcordDelivery {
	d.policy = policy
	return d
}

// Notify never waits for signing, relay IO or delivery. Missing credentials warn
// once per public channel/epoch locator; they cannot fail the original review.
func (d *ConcordDelivery) Notify(config *repoconfig.ConcordConfig, reviewID string, reviewRelays ...string) {
	if d == nil || config == nil {
		return
	}
	if (repoconfig.NotificationsConfig{Concord: config}).Validate() != nil {
		d.logger.Warn("skipping invalid concord notification routing")
		return
	}
	id := config.CredentialID()
	stream, ok := d.keys[id]
	if !ok {
		if _, loaded := d.missing.LoadOrStore(id, struct{}{}); !loaded {
			d.logger.Warn("concord channel credential missing; skipping delivery", "channel", config.Channel, "epoch", uint64(*config.Epoch))
		}
		return
	}
	select {
	case d.slots <- struct{}{}:
	default:
		d.logger.Warn("concord delivery capacity reached; skipping notice")
		return
	}
	// Snapshot the public configuration before crossing the goroutine boundary.
	channel, epoch := config.Channel, uint64(*config.Epoch)
	relays, rejected := d.policy.Admit(config.Relays)
	if len(rejected) > 0 {
		d.logger.Warn("concord relays rejected by egress policy", "rejected", len(rejected), "reason", rejected[0].Reason, "channel", config.Channel)
	}
	if len(relays) == 0 {
		d.logger.Warn("concord notice skipped: no admitted relays", "channel", config.Channel)
		return
	}
	reviewRelays = append([]string(nil), reviewRelays...)
	go func() {
		defer func() { <-d.slots }()
		author, err := d.signer.GetPublicKey(d.ctx)
		if err != nil {
			d.logger.Warn("concord author unavailable; skipping notice")
			return
		}
		eventID, err := nostr.IDFromHex(reviewID)
		if err != nil {
			d.logger.Warn("invalid Concord review reference; skipping notice")
			return
		}
		now := time.Now()
		rumor := nostr.Event{Kind: eventkind.ConcordMessage, PubKey: author, CreatedAt: nostr.Timestamp(now.Unix()),
			Tags:    nostr.Tags{{"ms", strconv.Itoa(now.Nanosecond() / int(time.Millisecond))}},
			Content: fmt.Sprintf("Drydock review published: nostr:%s", nip19.EncodeNevent(eventID, reviewRelays, author))}
		var nonces [2][32]byte
		if _, err := rand.Read(nonces[0][:]); err != nil {
			d.logger.Warn("concord entropy unavailable")
			return
		}
		if _, err := rand.Read(nonces[1][:]); err != nil {
			d.logger.Warn("concord entropy unavailable")
			return
		}
		ephemeral := nostr.Generate().Public()
		prepared, err := concordEnvelope(rumor, stream, epoch, channel, ephemeral, nonces, nil)
		if err != nil {
			d.logger.Warn("concord seal preparation failed; skipping notice")
			return
		}
		if err := d.signer.SignEvent(d.ctx, &prepared.Seal); err != nil {
			d.logger.Warn("concord author signing failed; skipping notice")
			return
		}
		envelope, err := concordEnvelope(rumor, stream, epoch, channel, ephemeral, nonces, &prepared.Seal.Sig)
		if err != nil {
			d.logger.Warn("concord envelope construction failed; skipping notice")
			return
		}
		if err := d.publish.Publish(d.ctx, relays, envelope.Wrap); err != nil {
			// Do not propagate arbitrary signer/transport errors into logs: credentials
			// must never be echoed, even by an injected implementation.
			d.logger.Warn("concord review notice delivery failed", "review_event_id", reviewID)
		}
	}()
}
