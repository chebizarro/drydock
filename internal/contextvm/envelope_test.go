package contextvm

import (
	"strconv"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func signedEnvelope(t *testing.T, sk nostr.SecretKey, tags nostr.Tags) Request {
	t.Helper()
	evt := nostr.Event{Kind: KindContextVM, CreatedAt: nostr.Now(), Tags: tags, Content: "{}"}
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("sign envelope: %v", err)
	}
	return Request{Event: evt, Sender: evt.PubKey}
}

func TestValidateEnvelopeAddressingAndBinding(t *testing.T) {
	sk := nostr.Generate()
	service := nostr.GetPublicKey(sk).Hex()
	future := strconv.FormatInt(time.Now().Add(5*time.Minute).Unix(), 10)
	base := func() nostr.Tags {
		return nostr.Tags{
			{"p", service},
			{"method", "review/order"},
			{"e", "patch-1"},
			{"expiration", future},
		}
	}
	policy := EnvelopePolicy{ServicePubkey: service, Method: "review/order", RelatedID: "patch-1", ExpirationRequired: true, MaxLifetime: 15 * time.Minute}

	if rpcErr := ValidateEnvelope(signedEnvelope(t, sk, base()), policy); rpcErr != nil {
		t.Fatalf("valid envelope rejected: %+v", rpcErr)
	}

	// Unauthenticated envelope: zero sender/id.
	if rpcErr := ValidateEnvelope(Request{}, policy); rpcErr == nil || rpcErr.Code != ErrorInvalidRequest {
		t.Fatalf("unauthenticated envelope accepted: %+v", rpcErr)
	}

	for name, mutate := range map[string]func(nostr.Tags) nostr.Tags{
		"wrong p":      func(tags nostr.Tags) nostr.Tags { tags[0][1] = nostr.GetPublicKey(nostr.Generate()).Hex(); return tags },
		"two p":        func(tags nostr.Tags) nostr.Tags { return append(tags, nostr.Tag{"p", service}) },
		"wrong method": func(tags nostr.Tags) nostr.Tags { tags[1][1] = "other/method"; return tags },
		"wrong e":      func(tags nostr.Tags) nostr.Tags { tags[2][1] = "patch-2"; return tags },
	} {
		t.Run(name, func(t *testing.T) {
			if rpcErr := ValidateEnvelope(signedEnvelope(t, sk, mutate(base())), policy); rpcErr == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestValidateEnvelopeReplayProtectionPolicy(t *testing.T) {
	sk := nostr.Generate()
	tagsNoExpiry := func() nostr.Tags { return nostr.Tags{{"method", "m"}} }
	tagsWith := func(expiration string) nostr.Tags { return nostr.Tags{{"method", "m"}, {"expiration", expiration}} }

	required := EnvelopePolicy{Method: "m", ExpirationRequired: true, MaxLifetime: 15 * time.Minute}
	optional := EnvelopePolicy{Method: "m", ExpirationRequired: false, MaxLifetime: 15 * time.Minute}

	// Mandatory replay protection: missing expiration must be rejected.
	if rpcErr := ValidateEnvelope(signedEnvelope(t, sk, tagsNoExpiry()), required); rpcErr == nil {
		t.Fatal("required policy accepted an envelope with no expiration")
	}
	// Optional replay protection: missing expiration is allowed.
	if rpcErr := ValidateEnvelope(signedEnvelope(t, sk, tagsNoExpiry()), optional); rpcErr != nil {
		t.Fatalf("optional policy rejected a missing expiration: %+v", rpcErr)
	}

	// An expired tag is rejected under either policy.
	past := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	if rpcErr := ValidateEnvelope(signedEnvelope(t, sk, tagsWith(past)), optional); rpcErr == nil || rpcErr.Code != ErrorExpired {
		t.Fatalf("expired envelope accepted: %+v", rpcErr)
	}

	// An expiration beyond the lifetime ceiling is rejected even when present.
	tooFar := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	if rpcErr := ValidateEnvelope(signedEnvelope(t, sk, tagsWith(tooFar)), optional); rpcErr == nil {
		t.Fatal("over-lifetime expiration accepted")
	}

	// A valid in-window expiration is accepted.
	ok := strconv.FormatInt(time.Now().Add(5*time.Minute).Unix(), 10)
	if rpcErr := ValidateEnvelope(signedEnvelope(t, sk, tagsWith(ok)), required); rpcErr != nil {
		t.Fatalf("valid in-window expiration rejected: %+v", rpcErr)
	}
}
