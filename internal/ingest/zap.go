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
	if len(p.trustedZappers) > 0 {
		if _, ok := p.trustedZappers[receiptAuthor]; !ok {
			return db.ZapReceiptRecord{}, errors.New("untrusted_zapper")
		}
	}

	amountMSat, err := zapAmountMSat(event)
	if err != nil {
		return db.ZapReceiptRecord{}, err
	}

	return db.ZapReceiptRecord{
		EventID:       event.ID.Hex(),
		PatchEventID:  patchEventID,
		PayerPubkey:   zapPayerPubkey(event),
		ReceiptAuthor: receiptAuthor,
		AmountMSat:    amountMSat,
		CreatedAt:     int64(event.CreatedAt),
	}, nil
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

	if tagAmount > 0 && invoiceAmount > 0 && tagAmount != invoiceAmount {
		return 0, errors.New("conflicting_amount")
	}
	if tagAmount > 0 {
		return tagAmount, nil
	}
	if invoiceAmount > 0 {
		return invoiceAmount, nil
	}
	return 0, errors.New("amount_missing")
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

func zapPayerPubkey(event nostr.Event) string {
	description, err := optionalSingleTagValue(event, "description")
	if err != nil || description == "" {
		return ""
	}
	var request nostr.Event
	if err := json.Unmarshal([]byte(description), &request); err != nil {
		return ""
	}
	return request.PubKey.Hex()
}
