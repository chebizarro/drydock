// Package payment implements Cashu ecash payment gating for review access.
//
// The payment service gates reviews based on:
// - Configured free pubkeys and repository maintainers
// - Active subscription (author+repo)
// - Free-tier daily quota
// - NIP-57 zap receipt covering the repository price
// - One-off Cashu token payment attached to patch event
package payment

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"drydock/internal/db"
	"drydock/internal/repoconfig"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
)

// InvoiceProvider creates and checks Lightning invoices via NWC.
type InvoiceProvider interface {
	CreateInvoice(ctx context.Context, sats int64, memo string, expiry time.Duration) (Invoice, error)
	LookupInvoice(ctx context.Context, invoice Invoice) (InvoiceStatus, error)
}

// Invoice represents a Lightning invoice.
type Invoice struct {
	ID          string
	Request     string // BOLT11 invoice string
	AmountMSats int64
	ExpiresAt   int64
}

// InvoiceStatus represents the status of a Lightning invoice.
type InvoiceStatus struct {
	PaymentHash string
	AmountMSats int64
	SettledAt   int64
	Preimage    string
	Settled     bool
	Expired     bool
}

// PayoutEvidence is definitive or reconcilable evidence for an outbound payment.
type PayoutEvidence struct {
	PaymentHash string
	Preimage    string
	SettledAt   int64
	Settled     bool
	Failed      bool
}

// PayoutSubmissionError distinguishes definitive/pre-send failures from
// ambiguous outcomes that may have reached the wallet.
type PayoutSubmissionError struct {
	MayHaveSubmitted bool
	Err              error
}

func (e *PayoutSubmissionError) Error() string { return e.Err.Error() }
func (e *PayoutSubmissionError) Unwrap() error { return e.Err }

// PayoutMayHaveSubmitted reports whether retrying could double-pay.
func PayoutMayHaveSubmitted(err error) bool {
	var submissionErr *PayoutSubmissionError
	return !errors.As(err, &submissionErr) || submissionErr.MayHaveSubmitted
}

// OutboundPayer is implemented by invoice providers that support NIP-47 pay_invoice.
type OutboundPayer interface {
	PayInvoice(ctx context.Context, bolt11 string, amountSats int64) (PayoutEvidence, error)
	LookupPayment(ctx context.Context, bolt11 string, amountSats int64) (PayoutEvidence, error)
}

// MintClient handles Cashu token parsing and mint operations.
type MintClient interface {
	ParseToken(raw string) (ParsedToken, error)
	CreateMeltQuote(ctx context.Context, mintURL, bolt11 string) (MeltQuote, error)
	MeltToken(ctx context.Context, mintURL string, quote MeltQuote, token ParsedToken) error
	LookupMeltQuote(ctx context.Context, mintURL string, quote MeltQuote) (MeltQuoteStatus, error)
}

// ParsedToken represents a decoded Cashu token.
type ParsedToken struct {
	MintURL    string
	Unit       string
	AmountSats int64
	Raw        string
	Proofs     json.RawMessage
}

// MeltQuote represents a mint's quote for melting tokens to pay an invoice.
type MeltQuote struct {
	ID         string
	Amount     int64
	FeeReserve int64
}

// MeltQuoteStatus is a normalized NUT-05 quote state.
type MeltQuoteStatus struct {
	State string // unpaid, pending, paid
}

// MeltSubmissionError distinguishes local failures before http.Client.Do from
// failures after the request may have reached the mint.
type MeltSubmissionError struct {
	MayHaveSubmitted bool
	Err              error
}

func (e *MeltSubmissionError) Error() string { return e.Err.Error() }
func (e *MeltSubmissionError) Unwrap() error { return e.Err }

func meltMayHaveBeenSubmitted(err error) bool {
	var submissionErr *MeltSubmissionError
	return !errors.As(err, &submissionErr) || submissionErr.MayHaveSubmitted
}

// Config configures the payment service.
type Config struct {
	TrustedMints  []string
	FreePubkeys   []string
	Timeout       time.Duration
	InvoiceExpiry time.Duration
}

// Service orchestrates payment authorization for reviews.
type Service struct {
	cfg     Config
	store   *db.Store
	invoice InvoiceProvider
	mint    MintClient
	logger  *slog.Logger
}

// New creates a new payment service.
func New(cfg Config, store *db.Store, invoice InvoiceProvider, mint MintClient, logger *slog.Logger) *Service {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.InvoiceExpiry == 0 {
		cfg.InvoiceExpiry = 5 * time.Minute
	}
	return &Service{cfg: cfg, store: store, invoice: invoice, mint: mint, logger: logger}
}

// SubmitPayout pays a reviewer BOLT11 through the configured outbound provider.
func (s *Service) SubmitPayout(ctx context.Context, destination string, amountSats int64, idempotencyKey string) (PayoutEvidence, error) {
	payer, ok := s.invoice.(OutboundPayer)
	if !ok || payer == nil {
		return PayoutEvidence{}, &PayoutSubmissionError{MayHaveSubmitted: false, Err: errors.New("outbound NWC payer is not configured")}
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return PayoutEvidence{}, &PayoutSubmissionError{MayHaveSubmitted: false, Err: errors.New("payout idempotency key is required")}
	}
	return payer.PayInvoice(ctx, destination, amountSats)
}

// ReconcilePayout looks up an already-submitted outbound payment without resubmitting it.
func (s *Service) ReconcilePayout(ctx context.Context, destination string, amountSats int64) (PayoutEvidence, error) {
	payer, ok := s.invoice.(OutboundPayer)
	if !ok || payer == nil {
		return PayoutEvidence{}, errors.New("outbound NWC payer is not configured")
	}
	return payer.LookupPayment(ctx, destination, amountSats)
}

// AuthorizeResult describes the outcome of a payment authorization attempt.
type AuthorizeResult struct {
	Allowed          bool
	AccessKind       string // free_pubkey, free_maintainer, free_tier, subscription, zap, cashu_review, cashu_subscription
	Reason           string // machine-readable denial reason
	ZapReceiptCursor int64  // latest receipt row observed for race-safe payment blocking
}

// AuthorizePatch checks if a patch event is authorized for review based on
// the repo's payment policy.
func (s *Service) AuthorizePatch(
	ctx context.Context,
	patchEvent nostr.Event,
	repoID string,
	policy repoconfig.PaymentsConfig,
) (AuthorizeResult, error) {
	var zapReceiptCursor int64
	allow := func(kind string) (AuthorizeResult, error) {
		return AuthorizeResult{Allowed: true, AccessKind: kind}, nil
	}
	deny := func(reason string) (AuthorizeResult, error) {
		return AuthorizeResult{Allowed: false, Reason: reason, ZapReceiptCursor: zapReceiptCursor}, nil
	}

	// 1. If payments disabled, allow immediately.
	if !policy.Enabled {
		return allow("")
	}

	patchEventID := patchEvent.ID.Hex()
	authorPubkey := patchEvent.PubKey.Hex()

	// 2. Check existing authorization.
	existing, err := s.store.GetReviewPayment(ctx, patchEventID)
	if err == nil && existing.Status == "authorized" {
		return allow(existing.AccessKind)
	}

	// 3. Configured pubkeys bypass subscription, Cashu, and free-tier paths.
	if containsPubkey(policy.FreePubkeys, authorPubkey) || containsPubkey(s.cfg.FreePubkeys, authorPubkey) {
		s.logger.Info("review authorized via free pubkey allowlist",
			"patch_event_id", patchEventID,
			"author", authorPubkey)
		return allow("free_pubkey")
	}

	// 4. Repository owners and maintainers are free by default.
	if policy.MaintainersAreFree() {
		maintainer, err := s.store.CanMaintainRepository(ctx, repoID, patchEvent.PubKey)
		if err != nil {
			return AuthorizeResult{}, fmt.Errorf("check repository maintainer: %w", err)
		}
		if maintainer {
			s.logger.Info("review authorized for repository maintainer",
				"patch_event_id", patchEventID,
				"author", authorPubkey)
			return allow("free_maintainer")
		}
	}

	// 5. Extract payment tag from patch event.
	token, mode, tagErr := extractPaymentTag(patchEvent)

	// 6. If explicit subscription mode, require subscription config.
	if mode == "subscription" && tagErr == nil {
		if policy.SubscriptionPriceSats <= 0 || policy.SubscriptionDays <= 0 {
			return deny("subscription_not_configured")
		}
	}

	// 7. Check active subscription (unless explicitly requesting subscription renewal).
	if mode != "subscription" {
		sub, hasActive, err := s.store.GetActiveSubscription(ctx, authorPubkey, repoID, time.Now().Unix())
		if err != nil {
			return AuthorizeResult{}, fmt.Errorf("check subscription: %w", err)
		}
		if hasActive {
			if err := s.store.AuthorizeReviewFromSubscription(ctx, patchEventID, repoID, authorPubkey); err != nil {
				return AuthorizeResult{}, fmt.Errorf("authorize from subscription: %w", err)
			}
			s.logger.Info("review authorized via subscription",
				"patch_event_id", patchEventID,
				"author", authorPubkey,
				"subscription_expires", sub.ExpiresAt)
			return allow("subscription")
		}
	}

	// 8. A per-review NIP-57 zap may cover the configured repository price.
	if mode != "subscription" && policy.AcceptsZaps() {
		minimumMSat := policy.PriceSats * 1000
		receipt, found, cursor, err := s.store.FindZapReceiptAtLeast(ctx, patchEventID, minimumMSat)
		zapReceiptCursor = cursor
		if err != nil {
			return AuthorizeResult{}, fmt.Errorf("check zap receipts: %w", err)
		}
		if found {
			s.logger.Info("review authorized via zap",
				"patch_event_id", patchEventID,
				"receipt_event_id", receipt.EventID,
				"amount_msat", receipt.AmountMSat)
			return allow("zap")
		}
	}

	// 9. Process Cashu token if present.
	if tagErr == nil && token != "" {
		return s.processTokenPayment(ctx, patchEventID, repoID, authorPubkey, token, mode, policy)
	}

	// 10. Try free tier.
	if policy.FreeReviewsPerDay > 0 {
		usageDay := time.Now().UTC().Format("2006-01-02")
		authorized, err := s.store.TryAuthorizeFreeReview(ctx, patchEventID, repoID, authorPubkey, policy.FreeReviewsPerDay, usageDay)
		if err != nil {
			return AuthorizeResult{}, fmt.Errorf("try free tier: %w", err)
		}
		if authorized {
			s.logger.Info("review authorized via free tier",
				"patch_event_id", patchEventID,
				"author", authorPubkey,
				"usage_day", usageDay)
			return allow("free_tier")
		}
	}

	// 11. No payment, no free tier quota.
	if tagErr != nil {
		return deny(tagErr.Error())
	}
	return deny("no_payment")
}

func containsPubkey(configured []string, author string) bool {
	author = scope.NormalizePubkey(author)
	for _, pubkey := range configured {
		if scope.NormalizePubkey(pubkey) == author {
			return true
		}
	}
	return false
}

func (s *Service) processTokenPayment(
	ctx context.Context,
	patchEventID, repoID, authorPubkey, token, mode string,
	policy repoconfig.PaymentsConfig,
) (AuthorizeResult, error) {
	deny := func(reason string) (AuthorizeResult, error) {
		return AuthorizeResult{Allowed: false, Reason: reason}, nil
	}
	if s.invoice == nil || s.mint == nil {
		return deny("payment_service_not_configured")
	}

	parsed, err := s.mint.ParseToken(token)
	if err != nil {
		return deny("invalid_cashu_token")
	}
	if !s.isTrustedMint(parsed.MintURL) {
		return deny("untrusted_mint")
	}
	if parsed.Unit != "sat" {
		return deny("unsupported_token_unit")
	}
	tokenHash := hashToken(token)
	targetPrice := paymentTargetPrice(mode, policy)
	if targetPrice <= 0 || parsed.AmountSats < targetPrice {
		return deny("insufficient_after_fees")
	}

	if existing, err := s.store.GetReviewPayment(ctx, patchEventID); err == nil {
		if existing.TokenHash != tokenHash {
			return deny("token_already_used")
		}
		return s.reconcileReservedPayment(ctx, existing)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return AuthorizeResult{}, fmt.Errorf("get existing payment: %w", err)
	}

	reservation := db.ReviewPaymentRecord{
		PatchEventID: patchEventID, RepoID: repoID, AuthorPubkey: authorPubkey,
		RequestedMode: mode, TokenHash: tokenHash, MintURL: parsed.MintURL,
		TokenAmountSats: parsed.AmountSats, ExpectedAmountSats: targetPrice,
		SubscriptionDays: policy.SubscriptionDays,
	}
	if err := s.store.ReserveReviewPaymentToken(ctx, reservation); err != nil {
		if errors.Is(err, db.ErrTokenHashAlreadyReserved) {
			reserved, lookupErr := s.store.GetReviewPaymentByTokenHash(ctx, tokenHash)
			if lookupErr == nil && reserved.PatchEventID == patchEventID {
				return s.reconcileReservedPayment(ctx, reserved)
			}
			return deny("token_already_used")
		}
		return AuthorizeResult{}, err
	}

	memo := fmt.Sprintf("drydock review %s", patchEventID[:12])
	invoice, err := s.invoice.CreateInvoice(ctx, targetPrice, memo, s.cfg.InvoiceExpiry)
	if err != nil {
		s.deletePendingAfterPreMeltFailure(ctx, patchEventID, "invoice creation failure")
		return AuthorizeResult{}, fmt.Errorf("create invoice: %w", err)
	}
	if err := validateInvoiceEvidence(invoice, targetPrice, time.Now().Unix()); err != nil {
		s.deletePendingAfterPreMeltFailure(ctx, patchEventID, "invalid invoice response")
		return AuthorizeResult{}, fmt.Errorf("validate created invoice: %w", err)
	}
	reservation.InvoiceID, reservation.InvoiceRequest = invoice.ID, invoice.Request
	reservation.InvoiceAmountMSats, reservation.InvoiceExpiresAt = invoice.AmountMSats, invoice.ExpiresAt
	if err := s.store.UpsertPendingReviewPayment(ctx, reservation); err != nil {
		s.deletePendingAfterPreMeltFailure(ctx, patchEventID, "persist pending payment failure")
		return AuthorizeResult{}, fmt.Errorf("persist pending payment: %w", err)
	}

	quote, err := s.mint.CreateMeltQuote(ctx, parsed.MintURL, invoice.Request)
	if err != nil {
		s.deletePendingAfterPreMeltFailure(ctx, patchEventID, "melt quote failure")
		return AuthorizeResult{}, fmt.Errorf("create melt quote: %w", err)
	}
	if quote.ID == "" || quote.Amount != targetPrice || quote.FeeReserve < 0 ||
		parsed.AmountSats < quote.Amount || quote.FeeReserve > parsed.AmountSats-quote.Amount {
		s.deletePendingAfterPreMeltFailure(ctx, patchEventID, "invalid or insufficient melt quote")
		return deny("insufficient_after_fees")
	}
	if err := s.store.MarkReviewPaymentMeltSubmitted(ctx, patchEventID, tokenHash, quote.ID, quote.Amount, quote.FeeReserve); err != nil {
		return AuthorizeResult{}, fmt.Errorf("persist melt submission intent: %w", err)
	}

	if err := s.mint.MeltToken(ctx, parsed.MintURL, quote, parsed); err != nil {
		if !meltMayHaveBeenSubmitted(err) {
			s.deletePendingAfterPreMeltFailure(ctx, patchEventID, "provably not submitted melt")
			return deny("payment_not_submitted")
		}
		s.logger.Warn("melt outcome unknown; preserving payment for reconciliation", "patch_event_id", patchEventID, "quote_id", quote.ID, "error", err)
		return deny("payment_pending")
	}
	if err := s.store.MarkReviewPaymentTokenSpent(ctx, patchEventID); err != nil {
		return AuthorizeResult{}, fmt.Errorf("persist paid melt result: %w", err)
	}
	rec, err := s.store.GetReviewPayment(ctx, patchEventID)
	if err != nil {
		return AuthorizeResult{}, err
	}
	return s.reconcileReservedPayment(ctx, rec)
}

func (s *Service) reconcileReservedPayment(ctx context.Context, rec db.ReviewPaymentRecord) (AuthorizeResult, error) {
	if rec.Status == "authorized" {
		return AuthorizeResult{Allowed: true, AccessKind: rec.AccessKind}, nil
	}
	expected := Invoice{ID: rec.InvoiceID, Request: rec.InvoiceRequest, AmountMSats: rec.InvoiceAmountMSats, ExpiresAt: rec.InvoiceExpiresAt}
	if err := validateInvoiceEvidence(expected, rec.ExpectedAmountSats, time.Now().Unix()); err != nil {
		return AuthorizeResult{Allowed: false, Reason: "payment_pending"}, nil
	}
	quote := MeltQuote{ID: rec.MeltQuoteID, Amount: rec.MeltQuoteAmountSats, FeeReserve: rec.MeltFeeReserveSats}

	mintPaid := rec.Status == "token_spent" && rec.MeltState == "paid"
	if !mintPaid {
		if rec.MeltState != "submitted" && rec.MeltState != "unpaid" {
			return AuthorizeResult{Allowed: false, Reason: "payment_pending"}, nil
		}
		status, err := s.mint.LookupMeltQuote(ctx, rec.MintURL, quote)
		if err != nil {
			return AuthorizeResult{}, fmt.Errorf("lookup melt quote: %w", err)
		}
		switch status.State {
		case "paid":
			if err := s.store.MarkReviewPaymentTokenSpent(ctx, rec.PatchEventID); err != nil {
				return AuthorizeResult{}, fmt.Errorf("persist reconciled paid quote: %w", err)
			}
			mintPaid = true
			rec.Status, rec.MeltState = "token_spent", "paid"
		case "unpaid":
			if err := s.store.MarkReviewPaymentMeltUnpaid(ctx, rec.PatchEventID); err != nil {
				return AuthorizeResult{}, fmt.Errorf("persist unpaid quote: %w", err)
			}
			rec.MeltState = "unpaid"
		case "pending":
		default:
			return AuthorizeResult{}, fmt.Errorf("unsupported melt quote state %q", status.State)
		}
	}

	settlement, err := s.invoice.LookupInvoice(ctx, expected)
	if err != nil {
		return AuthorizeResult{}, fmt.Errorf("lookup invoice settlement: %w", err)
	}
	if err := validateInvoiceStatus(expected, settlement); err != nil {
		return AuthorizeResult{}, fmt.Errorf("validate invoice settlement: %w", err)
	}
	if settlement.Settled && !mintPaid {
		return AuthorizeResult{}, errors.New("wallet reports settlement but mint quote is not paid")
	}
	if !mintPaid || !settlement.Settled {
		if !mintPaid && settlement.Expired && rec.MeltState == "unpaid" {
			return AuthorizeResult{Allowed: false, Reason: "payment_failed"}, nil
		}
		return AuthorizeResult{Allowed: false, Reason: "payment_pending"}, nil
	}

	accessKind, err := s.store.FinalizePaidReview(ctx, rec.PatchEventID, rec.TokenHash)
	if err != nil {
		return AuthorizeResult{}, fmt.Errorf("finalize paid review: %w", err)
	}
	s.logger.Info("review authorized via cashu payment",
		"patch_event_id", rec.PatchEventID, "author", rec.AuthorPubkey,
		"mode", rec.RequestedMode, "paid_amount_sats", rec.ExpectedAmountSats,
		"token_amount_sats", rec.TokenAmountSats, "mint", rec.MintURL)
	return AuthorizeResult{Allowed: true, AccessKind: accessKind}, nil
}

func validateInvoiceEvidence(invoice Invoice, expectedSats, now int64) error {
	if _, err := parsePaymentHash(invoice.ID); err != nil {
		return err
	}
	if invoice.Request == "" || !strings.HasPrefix(strings.ToLower(invoice.Request), "ln") {
		return errors.New("empty or malformed BOLT11")
	}
	if expectedSats <= 0 || expectedSats > (1<<63-1)/1000 || invoice.AmountMSats != expectedSats*1000 {
		return errors.New("invoice amount does not match persisted price")
	}
	if invoice.ExpiresAt <= now {
		return errors.New("invoice is expired")
	}
	return nil
}

func validateInvoiceStatus(expected Invoice, status InvoiceStatus) error {
	if !strings.EqualFold(status.PaymentHash, expected.ID) {
		return errors.New("invoice lookup payment hash mismatch")
	}
	if status.AmountMSats != expected.AmountMSats {
		return errors.New("invoice lookup amount mismatch")
	}
	if status.Settled && status.SettledAt <= 0 && status.Preimage == "" {
		return errors.New("invoice lookup lacks settlement evidence")
	}
	if !status.Settled && (status.SettledAt > 0 || status.Preimage != "") {
		return errors.New("invoice lookup has contradictory settlement evidence")
	}
	if status.SettledAt < 0 {
		return errors.New("invoice lookup has invalid settled_at")
	}
	if status.Preimage != "" {
		preimage, err := hex.DecodeString(status.Preimage)
		if err != nil || len(preimage) != 32 {
			return errors.New("invoice lookup has malformed preimage")
		}
		hash := sha256.Sum256(preimage)
		if !strings.EqualFold(hex.EncodeToString(hash[:]), expected.ID) {
			return errors.New("invoice lookup preimage mismatch")
		}
	}
	return nil
}

func (s *Service) deletePendingAfterPreMeltFailure(ctx context.Context, patchEventID, reason string) {
	if err := s.store.DeletePendingReviewPayment(ctx, patchEventID); err != nil {
		s.logger.Warn("failed to delete pending payment after pre-melt failure", "patch_event_id", patchEventID, "reason", reason, "error", err)
	}
}

func paymentTargetPrice(mode string, policy repoconfig.PaymentsConfig) int64 {
	if mode == "subscription" {
		return policy.SubscriptionPriceSats
	}
	return policy.PriceSats
}

func (s *Service) isTrustedMint(mintURL string) bool {
	normalized := strings.TrimRight(strings.ToLower(mintURL), "/")
	for _, trusted := range s.cfg.TrustedMints {
		if strings.TrimRight(strings.ToLower(trusted), "/") == normalized {
			return true
		}
	}
	return false
}

// extractPaymentTag extracts the cashu payment tag from a patch event.
// Returns (token, mode, error). Mode is "review" or "subscription".
func extractPaymentTag(event nostr.Event) (string, string, error) {
	var cashuTags []nostr.Tag
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "cashu" {
			cashuTags = append(cashuTags, tag)
		}
	}

	if len(cashuTags) == 0 {
		return "", "", errors.New("no_payment")
	}
	if len(cashuTags) > 1 {
		return "", "", errors.New("multiple_cashu_tags")
	}

	tag := cashuTags[0]
	token := strings.TrimSpace(tag[1])
	if token == "" {
		return "", "", errors.New("empty_cashu_token")
	}

	mode := "review"
	if len(tag) >= 3 {
		m := strings.ToLower(strings.TrimSpace(tag[2]))
		if m == "subscription" {
			mode = "subscription"
		} else if m != "" && m != "review" {
			return "", "", errors.New("unsupported_mode")
		}
	}

	return token, mode, nil
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
