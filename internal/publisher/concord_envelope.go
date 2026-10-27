package publisher

import (
	"encoding/json"
	"errors"
	"strconv"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
)

// concordEnvelope constructs CORD-01 + CORD-03 §3 at f84d55523593688c70c1b1a1b765992103157f1b.
// This is pure construction: time, entropy and the author signature are inputs.
// Call with nil signature to prepare the seal, sign it outside this function,
// then call with that signature to finish. No remote signer/IO enters this boundary.
type concordEnvelopeResult struct{ Seal, Wrap nostr.Event }

func concordEnvelope(rumor nostr.Event, stream nostr.SecretKey, epoch uint64, channel string,
	ephemeral nostr.PubKey, nonces [2][32]byte, signature *[64]byte) (concordEnvelopeResult, error) {
	if rumor.Kind != eventkind.ConcordMessage || rumor.PubKey == nostr.ZeroPK || stream.Public() == nostr.ZeroPK || ephemeral == nostr.ZeroPK {
		return concordEnvelopeResult{}, errors.New("invalid concord envelope input")
	}
	rumor.Tags = append(append(nostr.Tags{}, rumor.Tags...), nostr.Tag{"channel", channel}, nostr.Tag{"epoch", strconv.FormatUint(epoch, 10)})
	rumor.Sig = [64]byte{}
	rumor.ID = rumor.GetID()
	// Omit sig entirely: a rumor is unsigned, not an event with a zero signature.
	unsigned := struct {
		ID        nostr.ID        `json:"id"`
		PubKey    nostr.PubKey    `json:"pubkey"`
		CreatedAt nostr.Timestamp `json:"created_at"`
		Kind      nostr.Kind      `json:"kind"`
		Tags      nostr.Tags      `json:"tags"`
		Content   string          `json:"content"`
	}{rumor.ID, rumor.PubKey, rumor.CreatedAt, rumor.Kind, rumor.Tags, rumor.Content}
	raw, err := json.Marshal(unsigned)
	if err != nil {
		return concordEnvelopeResult{}, errors.New("encode concord rumor failed")
	}
	key, err := nip44.GenerateConversationKey(stream.Public(), stream)
	if err != nil {
		return concordEnvelopeResult{}, errors.New("derive concord conversation key failed")
	}
	encrypted, err := nip44.Encrypt(string(raw), key, nip44.WithCustomNonce(nonces[0][:]))
	if err != nil {
		return concordEnvelopeResult{}, errors.New("encrypt concord rumor failed")
	}
	seal := nostr.Event{Kind: eventkind.ConcordEncryptedSeal, PubKey: rumor.PubKey, CreatedAt: rumor.CreatedAt, Tags: nostr.Tags{}, Content: encrypted}
	seal.ID = seal.GetID()
	if signature == nil {
		return concordEnvelopeResult{Seal: seal}, nil
	}
	seal.Sig = *signature
	if !seal.VerifySignature() {
		return concordEnvelopeResult{}, errors.New("invalid concord seal signature")
	}
	raw, err = json.Marshal(seal)
	if err != nil {
		return concordEnvelopeResult{}, errors.New("encode concord seal failed")
	}
	encrypted, err = nip44.Encrypt(string(raw), key, nip44.WithCustomNonce(nonces[1][:]))
	if err != nil {
		return concordEnvelopeResult{}, errors.New("encrypt concord seal failed")
	}
	wrap := nostr.Event{Kind: eventkind.GiftWrap, CreatedAt: rumor.CreatedAt, Tags: nostr.Tags{{"p", ephemeral.Hex()}}, Content: encrypted}
	if err := wrap.Sign(stream); err != nil {
		return concordEnvelopeResult{}, errors.New("sign concord wrap failed")
	}
	return concordEnvelopeResult{Seal: seal, Wrap: wrap}, nil
}
