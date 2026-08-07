package contextvm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"fiatjaf.com/nostr"
)

// Signer signs outbound ContextVM events.
type Signer interface {
	GetPublicKey(ctx context.Context) (nostr.PubKey, error)
	SignEvent(ctx context.Context, evt *nostr.Event) error
}

// Pool is the subset of nostr.Pool used by Transport.
type Pool interface {
	PublishMany(ctx context.Context, urls []string, evt nostr.Event) chan nostr.PublishResult
	SubscribeManyNotifyClosed(ctx context.Context, urls []string, filter nostr.Filter, opts nostr.SubscriptionOptions) (chan nostr.RelayEvent, chan nostr.RelayClosed)
}

// Transport publishes and subscribes ContextVM JSON-RPC messages over Nostr.
type Transport struct {
	pool        Pool
	signer      Signer
	readRelays  []string
	writeRelays []string
	logger      *slog.Logger
}

func NewTransport(pool Pool, signer Signer, readRelays, writeRelays []string, logger *slog.Logger) *Transport {
	if logger == nil {
		logger = slog.Default()
	}
	return &Transport{
		pool:        pool,
		signer:      signer,
		readRelays:  append([]string(nil), readRelays...),
		writeRelays: append([]string(nil), writeRelays...),
		logger:      logger,
	}
}

// Send publishes a kind 25910 JSON-RPC request with a generated correlation
// ID. The signed Nostr event ID is returned to callers.
func (t *Transport) Send(ctx context.Context, method string, params any, recipients ...nostr.PubKey) (string, error) {
	id, err := randomRequestID()
	if err != nil {
		return "", err
	}
	return t.SendWithID(ctx, id, method, params, recipients...)
}

// SendWithID publishes a kind 25910 JSON-RPC request with an explicit
// request/response correlation id.
func (t *Transport) SendWithID(ctx context.Context, id, method string, params any, recipients ...nostr.PubKey) (string, error) {
	if id == "" {
		return "", errors.New("contextvm request id is required")
	}
	if t.pool == nil {
		return "", errors.New("contextvm transport requires pool")
	}
	if t.signer == nil {
		return "", errors.New("contextvm transport requires signer")
	}
	if method == "" {
		return "", errors.New("contextvm method is required")
	}
	if len(t.writeRelays) == 0 {
		return "", errors.New("contextvm transport requires write relays")
	}
	recipients, err := validRecipients(recipients)
	if err != nil {
		return "", err
	}

	evt := nostr.Event{
		CreatedAt: nostr.Now(),
		Kind:      KindContextVM,
		Tags:      append(recipientTags(recipients), nostr.Tag{"method", method}),
	}

	msg, err := newRequest(id, method, params)
	if err != nil {
		return "", fmt.Errorf("marshal contextvm params: %w", err)
	}
	content, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("marshal contextvm request: %w", err)
	}
	evt.Content = string(content)
	if err := t.signer.SignEvent(ctx, &evt); err != nil {
		return "", fmt.Errorf("sign contextvm event: %w", err)
	}

	if err := t.publish(ctx, evt); err != nil {
		return "", err
	}
	return evt.ID.Hex(), nil
}

// Notification describes a one-way ContextVM message. Notifications carry no
// JSON-RPC ID and therefore never receive an application-level response.
type Notification struct {
	Method         string
	Params         any
	Recipients     []nostr.PubKey
	RelatedEventID string
	Relays         []string
	Expiration     int64
}

// Notify publishes a kind 25910 JSON-RPC notification.
func (t *Transport) Notify(ctx context.Context, notification Notification) (string, error) {
	if t.pool == nil {
		return "", errors.New("contextvm transport requires pool")
	}
	if t.signer == nil {
		return "", errors.New("contextvm transport requires signer")
	}
	if notification.Method == "" {
		return "", errors.New("contextvm method is required")
	}
	relays := notification.Relays
	if len(relays) == 0 {
		relays = t.writeRelays
	}
	if len(relays) == 0 {
		return "", errors.New("contextvm transport requires write relays")
	}
	recipients, err := validRecipients(notification.Recipients)
	if err != nil {
		return "", err
	}
	msg, err := newNotification(notification.Method, notification.Params)
	if err != nil {
		return "", fmt.Errorf("marshal contextvm notification params: %w", err)
	}
	content, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("marshal contextvm notification: %w", err)
	}
	tags := append(recipientTags(recipients), nostr.Tag{"method", notification.Method})
	if notification.RelatedEventID != "" {
		tags = append(tags, nostr.Tag{"e", notification.RelatedEventID})
	}
	if notification.Expiration > 0 {
		tags = append(tags, nostr.Tag{"expiration", strconv.FormatInt(notification.Expiration, 10)})
	}
	evt := nostr.Event{CreatedAt: nostr.Now(), Kind: KindContextVM, Tags: tags, Content: string(content)}
	if err := t.signer.SignEvent(ctx, &evt); err != nil {
		return "", fmt.Errorf("sign contextvm notification: %w", err)
	}
	if err := t.publishTo(ctx, relays, evt); err != nil {
		return "", err
	}
	return evt.ID.Hex(), nil
}

// SendResponse publishes a kind 25910 JSON-RPC response addressed to recipients.
func (t *Transport) SendResponse(ctx context.Context, id string, result any, rpcErr *Error, recipients ...nostr.PubKey) error {
	return t.SendResponseToEvent(ctx, "", id, result, rpcErr, recipients...)
}

// SendResponseToEvent publishes a kind 25910 JSON-RPC response with an "e" tag
// referencing the request event id.
func (t *Transport) SendResponseToEvent(ctx context.Context, requestEventID, id string, result any, rpcErr *Error, recipients ...nostr.PubKey) error {
	if id == "" {
		return errors.New("contextvm response id is required")
	}
	if t.pool == nil {
		return errors.New("contextvm transport requires pool")
	}
	if t.signer == nil {
		return errors.New("contextvm transport requires signer")
	}
	if len(t.writeRelays) == 0 {
		return errors.New("contextvm transport requires write relays")
	}
	recipients, err := validRecipients(recipients)
	if err != nil {
		return err
	}

	var msg Message
	if rpcErr != nil {
		msg = Message{JSONRPC: jsonRPCVersion, ID: id, Error: rpcErr}
	} else {
		msg, err = newResult(id, result)
		if err != nil {
			return fmt.Errorf("marshal contextvm result: %w", err)
		}
	}
	content, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal contextvm response: %w", err)
	}
	evt := nostr.Event{
		CreatedAt: nostr.Now(),
		Kind:      KindContextVM,
		Tags:      responseTags(requestEventID, recipients),
		Content:   string(content),
	}
	if err := t.signer.SignEvent(ctx, &evt); err != nil {
		return fmt.Errorf("sign contextvm response: %w", err)
	}
	return t.publish(ctx, evt)
}

func responseTags(requestEventID string, recipients []nostr.PubKey) nostr.Tags {
	tags := nostr.Tags{}
	if requestEventID != "" {
		tags = append(tags, nostr.Tag{"e", requestEventID})
	}
	return append(tags, recipientTags(recipients)...)
}

// Subscribe opens a long-lived subscription for kind 25910 messages addressed to
// our pubkey and returns decoded requests/responses.
func (t *Transport) Subscribe(ctx context.Context) (<-chan Request, <-chan error, error) {
	if t.pool == nil {
		return nil, nil, errors.New("contextvm transport requires pool")
	}
	if t.signer == nil {
		return nil, nil, errors.New("contextvm transport requires signer")
	}
	if len(t.readRelays) == 0 {
		return nil, nil, errors.New("contextvm transport requires read relays")
	}
	pubkey, err := t.signer.GetPublicKey(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get contextvm pubkey: %w", err)
	}

	filter := nostr.Filter{
		Kinds: []nostr.Kind{KindContextVM},
		Tags:  nostr.TagMap{"p": {pubkey.Hex()}},
	}
	stream, closed := t.pool.SubscribeManyNotifyClosed(ctx, t.readRelays, filter, nostr.SubscriptionOptions{Label: "contextvm"})
	out := make(chan Request)
	errs := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errs)
		seen := make(map[string]struct{})
		for {
			select {
			case <-ctx.Done():
				return
			case c, ok := <-closed:
				if !ok {
					closed = nil
					continue
				}
				relay := ""
				if c.Relay != nil {
					relay = c.Relay.URL
				}
				t.logger.Warn("contextvm subscription closed", "relay", relay, "reason", c.Reason, "handled_auth", c.HandledAuth)
			case re, ok := <-stream:
				if !ok {
					return
				}
				id := re.Event.ID.Hex()
				if _, exists := seen[id]; exists {
					continue
				}
				seen[id] = struct{}{}
				if !re.Event.CheckID() || !re.Event.VerifySignature() {
					select {
					case errs <- fmt.Errorf("invalid contextvm event: %s", id):
					default:
					}
					continue
				}
				var msg Message
				if err := json.Unmarshal([]byte(re.Event.Content), &msg); err != nil {
					select {
					case errs <- fmt.Errorf("decode contextvm event %s: %w", id, err):
					default:
					}
					continue
				}
				if msg.JSONRPC != jsonRPCVersion {
					select {
					case errs <- fmt.Errorf("invalid contextvm jsonrpc version for event %s", id):
					default:
					}
					continue
				}
				relay := ""
				if re.Relay != nil {
					relay = re.Relay.URL
				}
				select {
				case <-ctx.Done():
					return
				case out <- Request{Event: re.Event, Relay: relay, Sender: re.Event.PubKey, Msg: msg}:
				}
			}
		}
	}()
	return out, errs, nil
}

func (t *Transport) publish(ctx context.Context, evt nostr.Event) error {
	return t.publishTo(ctx, t.writeRelays, evt)
}

func (t *Transport) publishTo(ctx context.Context, relays []string, evt nostr.Event) error {
	success := 0
	var errs []error
	for res := range t.pool.PublishMany(ctx, relays, evt) {
		if res.Error != nil {
			errs = append(errs, fmt.Errorf("%s: %w", res.RelayURL, res.Error))
			continue
		}
		success++
	}
	if success > 0 {
		return nil
	}
	return fmt.Errorf("publish contextvm event %s failed: %w", evt.ID.Hex(), errors.Join(errs...))
}

func randomRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate contextvm request id: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

func validRecipients(recipients []nostr.PubKey) ([]nostr.PubKey, error) {
	if len(recipients) == 0 {
		return nil, errors.New("contextvm recipient is required")
	}
	seen := make(map[nostr.PubKey]struct{}, len(recipients))
	valid := make([]nostr.PubKey, 0, len(recipients))
	for _, recipient := range recipients {
		if recipient == nostr.ZeroPK {
			return nil, errors.New("contextvm recipient must be nonzero")
		}
		if _, exists := seen[recipient]; exists {
			continue
		}
		seen[recipient] = struct{}{}
		valid = append(valid, recipient)
	}
	return valid, nil
}

func recipientTags(recipients []nostr.PubKey) nostr.Tags {
	tags := make(nostr.Tags, 0, len(recipients))
	for _, recipient := range recipients {
		tags = append(tags, nostr.Tag{"p", recipient.Hex()})
	}
	return tags
}
