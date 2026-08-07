package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"drydock/internal/db"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
	"github.com/btcsuite/btcd/btcutil/bech32"
)

func (p *Processor) validateZapReceipt(event nostr.Event) (db.ZapReceiptRecord, error) {
	if event.Kind != 9735 {
		return db.ZapReceiptRecord{}, errors.New("wrong_kind")
	}
	recipient, err := singleTagValue(event, "p")
	if err != nil {
		return db.ZapReceiptRecord{}, fmt.Errorf("recipient_%w", err)
	}
	recipient = scope.NormalizePubkey(recipient)
	if _, err := nostr.PubKeyFromHex(recipient); err != nil {
		return db.ZapReceiptRecord{}, errors.New("invalid_recipient")
	}
	if recipient != p.servicePubkey {
		return db.ZapReceiptRecord{}, errors.New("wrong_recipient")
	}

	patchEventID, err := singleTagValue(event, "e")
	if err != nil {
		return db.ZapReceiptRecord{}, fmt.Errorf("patch_%w", err)
	}
	if _, err := nostr.IDFromHex(patchEventID); err != nil {
		return db.ZapReceiptRecord{}, errors.New("invalid_patch_event")
	}

	receiptAuthor := event.PubKey.Hex()
	if len(p.trustedZappers) == 0 {
		// Fail closed: without a trusted-zapper allowlist any key could mint
		// receipts. Zap-based payment requires explicit provider trust.
		return db.ZapReceiptRecord{}, errors.New("no_trusted_zappers_configured")
	}
	if _, ok := p.trustedZappers[receiptAuthor]; !ok {
		return db.ZapReceiptRecord{}, errors.New("untrusted_zapper")
	}

	amountMSat, err := zapAmountMSat(event)
	if err != nil {
		return db.ZapReceiptRecord{}, err
	}

	request, err := verifiedZapRequest(event)
	if err != nil {
		return db.ZapReceiptRecord{}, err
	}
	if err := correlateZapRequest(request, recipient, patchEventID, amountMSat); err != nil {
		return db.ZapReceiptRecord{}, err
	}

	return db.ZapReceiptRecord{
		EventID:       event.ID.Hex(),
		PatchEventID:  patchEventID,
		PayerPubkey:   request.PubKey.Hex(),
		ReceiptAuthor: receiptAuthor,
		AmountMSat:    amountMSat,
		CreatedAt:     int64(event.CreatedAt),
	}, nil
}

// verifiedZapRequest extracts the embedded kind-9734 zap request from the
// receipt's description tag and verifies its ID and signature (NIP-57).
func verifiedZapRequest(event nostr.Event) (nostr.Event, error) {
	description, err := optionalSingleTagValue(event, "description")
	if err != nil || description == "" {
		return nostr.Event{}, errors.New("description_missing")
	}
	var request nostr.Event
	if err := json.Unmarshal([]byte(description), &request); err != nil {
		return nostr.Event{}, errors.New("description_invalid")
	}
	if request.Kind != 9734 {
		return nostr.Event{}, errors.New("zap_request_wrong_kind")
	}
	if !request.CheckID() || !request.VerifySignature() {
		return nostr.Event{}, errors.New("zap_request_unverified")
	}
	return request, nil
}

// correlateZapRequest requires the verified zap request to name the same
// recipient, patch event, and (when specified) amount as the outer receipt.
func correlateZapRequest(request nostr.Event, recipient, patchEventID string, amountMSat int64) error {
	reqRecipient, err := singleTagValue(request, "p")
	if err != nil {
		return errors.New("zap_request_recipient_missing")
	}
	if scope.NormalizePubkey(reqRecipient) != recipient {
		return errors.New("zap_request_recipient_mismatch")
	}
	reqEventID, err := singleTagValue(request, "e")
	if err != nil {
		return errors.New("zap_request_event_missing")
	}
	if !strings.EqualFold(strings.TrimSpace(reqEventID), patchEventID) {
		return errors.New("zap_request_event_mismatch")
	}
	if raw, err := optionalSingleTagValue(request, "amount"); err != nil {
		return errors.New("zap_request_amount_duplicated")
	} else if raw != "" {
		reqAmount, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || reqAmount <= 0 {
			return errors.New("zap_request_amount_invalid")
		}
		if reqAmount != amountMSat {
			return errors.New("zap_request_amount_mismatch")
		}
	}
	return nil
}

func singleTagValue(event nostr.Event, name string) (string, error) {
	value := ""
	found := 0
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != name {
			continue
		}
		found++
		if strings.TrimSpace(tag[1]) != "" {
			value = strings.TrimSpace(tag[1])
		}
	}
	switch {
	case found == 0 || value == "":
		return "", errors.New("tag_missing")
	case found > 1:
		return "", errors.New("tag_duplicated")
	default:
		return value, nil
	}
}

func zapAmountMSat(event nostr.Event) (int64, error) {
	var tagAmount int64
	if raw, err := optionalSingleTagValue(event, "amount"); err != nil {
		return 0, fmt.Errorf("amount_%w", err)
	} else if raw != "" {
		amount, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || amount <= 0 {
			return 0, errors.New("invalid_amount")
		}
		tagAmount = amount
	}

	var invoiceAmount int64
	if invoice, err := optionalSingleTagValue(event, "bolt11"); err != nil {
		return 0, fmt.Errorf("bolt11_%w", err)
	} else if invoice != "" {
		amount, err := decodeBolt11AmountMSat(invoice)
		if err != nil {
			return 0, errors.New("invalid_bolt11")
		}
		invoiceAmount = amount
	}

	if invoiceAmount <= 0 {
		// Fail closed: a bare amount tag with no invoice is unfalsifiable —
		// require the BOLT11 invoice as evidence of an actual payment amount.
		return 0, errors.New("bolt11_missing")
	}
	if tagAmount > 0 && tagAmount != invoiceAmount {
		return 0, errors.New("conflicting_amount")
	}
	return invoiceAmount, nil
}

func optionalSingleTagValue(event nostr.Event, name string) (string, error) {
	value := ""
	found := 0
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != name {
			continue
		}
		found++
		value = strings.TrimSpace(tag[1])
	}
	if found > 1 {
		return "", errors.New("tag_duplicated")
	}
	return value, nil
}

func decodeBolt11AmountMSat(invoice string) (int64, error) {
	hrp, _, err := bech32.DecodeNoLimit(strings.TrimSpace(invoice))
	if err != nil || !strings.HasPrefix(hrp, "ln") {
		return 0, errors.New("invalid bolt11")
	}

	amountStart := -1
	for i, r := range hrp[2:] {
		if unicode.IsDigit(r) {
			amountStart = i + 2
			break
		}
	}
	if amountStart < 0 {
		return 0, errors.New("amountless bolt11")
	}
	amountText := hrp[amountStart:]
	multiplier := byte(0)
	last := amountText[len(amountText)-1]
	if last < '0' || last > '9' {
		multiplier = last
		amountText = amountText[:len(amountText)-1]
	}
	value, err := strconv.ParseInt(amountText, 10, 64)
	if err != nil || value <= 0 {
		return 0, errors.New("invalid bolt11 amount")
	}

	var unitMSat int64
	switch multiplier {
	case 0:
		unitMSat = 100_000_000_000
	case 'm':
		unitMSat = 100_000_000
	case 'u':
		unitMSat = 100_000
	case 'n':
		unitMSat = 100
	case 'p':
		if value%10 != 0 {
			return 0, errors.New("fractional millisatoshi")
		}
		return value / 10, nil
	default:
		return 0, errors.New("invalid bolt11 multiplier")
	}
	if value > (1<<63-1)/unitMSat {
		return 0, errors.New("bolt11 amount overflow")
	}
	return value * unitMSat, nil
}

