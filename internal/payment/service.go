// Package payment implements Cashu ecash payment gating for review access.
//
// The payment service gates reviews based on:
// - Active subscription (author+repo)
// - Free-tier daily quota
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

	"fiatjaf.com/nostr"
)

// InvoiceProvider creates and checks Lightning invoices via NWC.
type InvoiceProvider interface {
	CreateInvoice(ctx context.Context, sats int64, memo string, expiry time.Duration) (Invoice, error)
	LookupInvoice(ctx context.Context, invoiceID string) (InvoiceStatus, error)
}

// Invoice represents a Lightning invoice.
type Invoice struct {
	ID      string
	Request string // BOLT11 invoice string
}

// InvoiceStatus represents the status of a Lightning invoice.
type InvoiceStatus struct {
	Settled bool
	Expired bool
}

// MintClient handles Cashu token parsing and mint operations.
type MintClient interface {
	ParseToken(raw string) (ParsedToken, error)
	CreateMeltQuote(ctx context.Context, mintURL, bolt11 string) (MeltQuote, error)
	MeltToken(ctx context.Context, mintURL string, quote MeltQuote, token ParsedToken) error
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

// Config configures the payment service.
type Config struct {
	TrustedMints  []string
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

// AuthorizeResult describes the outcome of a payment authorization attempt.
type AuthorizeResult struct {
	Allowed    bool
	AccessKind string // free_tier, subscription, cashu_review, cashu_subscription
	Reason     string // machine-readable denial reason
}

// AuthorizePatch checks if a patch event is authorized for review based on
// the repo's payment policy.
func (s *Service) AuthorizePatch(
	ctx context.Context,
	patchEvent nostr.Event,
	repoID string,
	policy repoconfig.PaymentsConfig,
) (AuthorizeResult, error) {
	allow := func(kind string) (AuthorizeResult, error) {
		return AuthorizeResult{Allowed: true, AccessKind: kind}, nil
	}
	deny := func(reason string) (AuthorizeResult, error) {
		return AuthorizeResult{Allowed: false, Reason: reason}, nil
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

	// 3. Extract payment tag from patch event.
	token, mode, tagErr := extractPaymentTag(patchEvent)

	// 4. If explicit subscription mode, require subscription config.
	if mode == "subscription" && tagErr == nil {
		if policy.SubscriptionPriceSats <= 0 || policy.SubscriptionDays <= 0 {
			return deny("subscription_not_configured")
		}
	}

	// 5. Check active subscription (unless explicitly requesting subscription renewal).
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

	// 6. Process Cashu token if present.
	if tagErr == nil && token != "" {
		return s.processTokenPayment(ctx, patchEventID, repoID, authorPubkey, token, mode, policy)
	}

	// 7. Try free tier.
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

	// 8. No payment, no free tier quota.
	if tagErr != nil {
		return deny(tagErr.Error())
	}
	return deny("no_payment")
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
	if parsed.AmountSats < targetPrice {
		return deny("insufficient_after_fees")
	}

	if existing, err := s.store.GetReviewPayment(ctx, patchEventID); err == nil {
		if existing.TokenHash != tokenHash {
			return deny("token_already_used")
		}
		return s.reconcileReservedPayment(ctx, existing, policy)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return AuthorizeResult{}, fmt.Errorf("get existing payment: %w", err)
	}

	reservation := db.ReviewPaymentRecord{
		PatchEventID:    patchEventID,
		RepoID:          repoID,
		AuthorPubkey:    authorPubkey,
		RequestedMode:   mode,
		TokenHash:       tokenHash,
		MintURL:         parsed.MintURL,
		TokenAmountSats: parsed.AmountSats,
	}
	if err := s.store.ReserveReviewPaymentToken(ctx, reservation); err != nil {
		if errors.Is(err, db.ErrTokenHashAlreadyReserved) {
			reserved, lookupErr := s.store.GetReviewPaymentByTokenHash(ctx, tokenHash)
			if lookupErr == nil && reserved.PatchEventID == patchEventID {
				return s.reconcileReservedPayment(ctx, reserved, policy)
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

	reservation.InvoiceID = invoice.ID
	reservation.InvoiceRequest = invoice.Request
	reservation.InvoiceExpiresAt = time.Now().Add(s.cfg.InvoiceExpiry).Unix()
	if err := s.store.UpsertPendingReviewPayment(ctx, reservation); err != nil {
		s.deletePendingAfterPreMeltFailure(ctx, patchEventID, "persist pending payment failure")
		return AuthorizeResult{}, fmt.Errorf("persist pending payment: %w", err)
	}

	quote, err := s.mint.CreateMeltQuote(ctx, parsed.MintURL, invoice.Request)
	if err != nil {
		s.deletePendingAfterPreMeltFailure(ctx, patchEventID, "melt quote failure")
		return AuthorizeResult{}, fmt.Errorf("create melt quote: %w", err)
	}
	if parsed.AmountSats < quote.Amount+quote.FeeReserve {
		s.deletePendingAfterPreMeltFailure(ctx, patchEventID, "insufficient funds after fees")
		return deny("insufficient_after_fees")
	}

	if err := s.mint.MeltToken(ctx, parsed.MintURL, quote, parsed); err != nil {
		s.deletePendingAfterPreMeltFailure(ctx, patchEventID, "melt failure")
		return deny("token_spent")
	}

	settlement, err := s.invoice.LookupInvoice(ctx, invoice.ID)
	if err != nil {
		if markErr := s.store.MarkReviewPaymentTokenSpent(ctx, patchEventID); markErr != nil {
			return AuthorizeResult{}, fmt.Errorf("mark token spent after settlement lookup failure: %w", markErr)
		}
		return AuthorizeResult{}, fmt.Errorf("lookup invoice settlement: %w", err)
	}
	if !settlement.Settled {
		if err := s.store.MarkReviewPaymentTokenSpent(ctx, patchEventID); err != nil {
			return AuthorizeResult{}, fmt.Errorf("mark token spent for unsettled invoice: %w", err)
		}
		s.logger.Warn("cashu token melted but invoice not settled; authorization withheld",
			"patch_event_id", patchEventID,
			"author", authorPubkey,
			"invoice_id", invoice.ID,
			"expired", settlement.Expired)
		return deny("payment_pending")
	}

	if err := s.store.MarkReviewPaymentTokenSpent(ctx, patchEventID); err != nil {
		return AuthorizeResult{}, fmt.Errorf("mark token spent: %w", err)
	}
	reservation.Status = "token_spent"
	reservation.InvoiceID = invoice.ID
	return s.finalizePaidAuthorization(ctx, reservation, targetPrice, policy.SubscriptionDays)
}

func (s *Service) reconcileReservedPayment(ctx context.Context, rec db.ReviewPaymentRecord, policy repoconfig.PaymentsConfig) (AuthorizeResult, error) {
	if rec.Status == "authorized" {
		return AuthorizeResult{Allowed: true, AccessKind: rec.AccessKind}, nil
	}
	if rec.InvoiceID == "" {
		return AuthorizeResult{Allowed: false, Reason: "payment_pending"}, nil
	}

	settlement, err := s.invoice.LookupInvoice(ctx, rec.InvoiceID)
	if err != nil {
		return AuthorizeResult{}, fmt.Errorf("lookup invoice settlement: %w", err)
	}
	if !settlement.Settled {
		return AuthorizeResult{Allowed: false, Reason: "payment_pending"}, nil
	}

	if rec.Status == "pending" {
		if err := s.store.MarkReviewPaymentTokenSpent(ctx, rec.PatchEventID); err != nil {
			return AuthorizeResult{}, fmt.Errorf("mark token spent during reconciliation: %w", err)
		}
		rec.Status = "token_spent"
	}
	return s.finalizePaidAuthorization(ctx, rec, paymentTargetPrice(rec.RequestedMode, policy), policy.SubscriptionDays)
}

func (s *Service) finalizePaidAuthorization(ctx context.Context, rec db.ReviewPaymentRecord, paidAmountSats int64, subscriptionDays int) (AuthorizeResult, error) {
	accessKind := "cashu_review"
	if rec.RequestedMode == "subscription" {
		accessKind = "cashu_subscription"
		if err := s.store.UpsertSubscription(ctx, rec.AuthorPubkey, rec.RepoID, rec.PatchEventID, rec.TokenHash, paidAmountSats, subscriptionDays); err != nil {
			return AuthorizeResult{}, fmt.Errorf("create subscription: %w", err)
		}
	}

	if err := s.store.MarkReviewPaymentAuthorized(ctx, rec.PatchEventID, accessKind); err != nil {
		return AuthorizeResult{}, fmt.Errorf("mark payment authorized: %w", err)
	}

	s.logger.Info("review authorized via cashu payment",
		"patch_event_id", rec.PatchEventID,
		"author", rec.AuthorPubkey,
		"mode", rec.RequestedMode,
		"paid_amount_sats", paidAmountSats,
		"token_amount_sats", rec.TokenAmountSats,
		"mint", rec.MintURL)

	return AuthorizeResult{Allowed: true, AccessKind: accessKind}, nil
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
