// Package enrollment implements the paid ContextVM handshake through which a
// repository maintainer opts a repository into reactive review.
//
// An enrollment is the ContextVM counterpart of a maintainer-authored NIP-51
// list entry: the same authority rule applies (the sender must be the owner of,
// or a maintainer in, the latest stored kind-30617 announcement), and the
// signed request event is the provenance. Identity comes from the signed
// envelope only; nothing in the parameters can confer authority.
package enrollment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/monitoring"
	"git.sharegap.net/cascadia/drydock/internal/payment"
	"git.sharegap.net/cascadia/drydock/internal/repoconfig"
	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
)

// AccessFree is the access kind recorded when the enrollment policy charges
// nothing.
const AccessFree = "free"

var (
	ErrInvalidTarget       = errors.New("invalid repository target")
	ErrSecurityCeiling     = errors.New("repository is outside the configured security ceiling")
	ErrAnnouncementUnknown = errors.New("repository announcement is not known; publish kind 30617 to a relay Drydock reads")
	ErrNotMaintainer       = errors.New("sender is not the owner or a maintainer of the repository")
	ErrInvalidExpiry       = errors.New("expires_at must be in the future and within the maximum enrollment lifetime")
	ErrPaymentsUnavailable = errors.New("enrollment requires payment but no payment service is configured")
)

// PaymentDeniedError carries the structured denial the ContextVM client needs
// to retry or pay.
type PaymentDeniedError struct {
	Reason    string
	Retryable bool
}

func (e *PaymentDeniedError) Error() string {
	if e == nil || e.Reason == "" {
		return "payment required"
	}
	return "payment required: " + e.Reason
}

// Store is the persistence surface the service needs. *db.Store satisfies it.
type Store interface {
	LoadRepositoryAnnouncement(ctx context.Context, repoID string) (nostr.Event, bool, error)
	CanMaintainRepository(ctx context.Context, repoID string, author nostr.PubKey) (bool, error)
	ApplyMonitoringEnrollment(ctx context.Context, e db.MonitoringEnrollment) (bool, error)
	RevokeMonitoringEnrollment(ctx context.Context, repositoryAddress, authorPubkey, revokeEventID string, revokedAt int64) (bool, error)
	GetMonitoringEnrollment(ctx context.Context, repositoryAddress, authorPubkey string) (db.MonitoringEnrollment, bool, error)
}

// Registry is the live membership the service feeds. *monitoring.Registry
// satisfies it.
type Registry interface {
	ApplyEnrollment(ctx context.Context, e monitoring.Enrollment) error
	RevokeEnrollment(repositoryAddress string, author nostr.PubKey)
	Contains(repositoryAddress string) bool
}

// Policy is the operator-level enrollment price and lifetime.
type Policy struct {
	// PriceSats charged per enrollment; 0 means free.
	PriceSats int64
	// TTL applied when the request names no expiry; 0 means no expiry.
	TTL time.Duration
	// MaxTTL bounds a requested expiry. Zero means one year.
	MaxTTL time.Duration
}

func (p Policy) maxTTL() time.Duration {
	if p.MaxTTL <= 0 {
		return 365 * 24 * time.Hour
	}
	return p.MaxTTL
}

// Request is a validated enrollment or revocation intent.
type Request struct {
	Event     nostr.Event // the signed ContextVM request event
	Sender    nostr.PubKey
	Ref       scope.RepositoryRef
	ExpiresAt int64 // requested expiry; 0 = policy default
}

// Receipt is the durable answer to an enrollment request.
type Receipt struct {
	EnrollmentID        string `json:"enrollment_id"`
	RepoAddr            string `json:"repo_addr"`
	Author              string `json:"author"`
	AnnouncementEventID string `json:"announcement_event_id"`
	AccessKind          string `json:"access_kind"`
	ExpiresAt           int64  `json:"expires_at,omitempty"`
	// Monitored reports effective membership after every ceiling. An accepted
	// enrollment outside the operator ceiling is stored but not monitored.
	Monitored bool   `json:"monitored"`
	State     string `json:"state"`
}

// Service validates authority and payment, persists enrollments, and feeds the
// registry.
type Service struct {
	store      Store
	registry   Registry
	scope      scope.Matcher
	authorizer payment.PatchAuthorizer // nil when no payment service exists
	policy     Policy
	logger     *slog.Logger
	now        func() time.Time

	// keys serializes persist+project per (repository, author) so the live
	// registry always reflects the stored winning command.
	keysMu sync.Mutex
	keys   map[string]*sync.Mutex
}

func New(store Store, registry Registry, ceiling scope.Matcher, authorizer payment.PatchAuthorizer, policy Policy, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, registry: registry, scope: ceiling, authorizer: authorizer, policy: policy, logger: logger, now: time.Now, keys: map[string]*sync.Mutex{}}
}

func (s *Service) lock(repositoryAddress, author string) func() {
	key := repositoryAddress + "|" + author
	s.keysMu.Lock()
	mu, ok := s.keys[key]
	if !ok {
		mu = &sync.Mutex{}
		s.keys[key] = mu
	}
	s.keysMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// project makes the live registry match the stored winning command for
// (repository, author): an active enrollment is applied, anything else
// (tombstone, expired, absent) is revoked live. It is the only path that
// mutates the registry, so a submitted operation can never contradict the
// durable winner.
func (s *Service) project(ctx context.Context, ref scope.RepositoryRef, author nostr.PubKey) (db.MonitoringEnrollment, bool, error) {
	row, found, err := s.store.GetMonitoringEnrollment(ctx, ref.Address, author.Hex())
	if err != nil {
		return db.MonitoringEnrollment{}, false, err
	}
	if found && row.Active(s.now().Unix()) {
		if err := s.registry.ApplyEnrollment(ctx, monitoring.Enrollment{
			Author: author, Ref: ref, RequestEventID: row.RequestEventID, CreatedAt: row.RequestCreatedAt, ExpiresAt: row.ExpiresAt,
		}); err != nil {
			return row, found, fmt.Errorf("activate enrollment: %w", err)
		}
		return row, found, nil
	}
	s.registry.RevokeEnrollment(ref.Address, author)
	return row, found, nil
}

func stateOf(row db.MonitoringEnrollment, found bool) string {
	switch {
	case !found:
		return "not_enrolled"
	case row.RevokedAt != 0:
		return "revoked"
	default:
		return "already_enrolled"
	}
}

// authorize resolves the announcement and checks the sender's authority and
// the static ceiling. It is shared by enroll and unenroll so a revocation is
// held to the same standard as the enrollment it undoes.
func (s *Service) authorize(ctx context.Context, req Request) (nostr.Event, error) {
	if req.Ref.Address == "" || req.Sender == nostr.ZeroPK {
		return nostr.Event{}, ErrInvalidTarget
	}
	if !s.scope.Allows(req.Ref.RepositoryID, req.Ref.OwnerPubkey) {
		return nostr.Event{}, ErrSecurityCeiling
	}
	announcement, ok, err := s.store.LoadRepositoryAnnouncement(ctx, req.Ref.RepositoryID)
	if err != nil {
		return nostr.Event{}, fmt.Errorf("load repository announcement: %w", err)
	}
	if !ok || !db.ValidRepositoryAnnouncement(announcement) || announcement.PubKey.Hex() != req.Ref.OwnerPubkey {
		return nostr.Event{}, ErrAnnouncementUnknown
	}
	maintainer, err := s.store.CanMaintainRepository(ctx, req.Ref.RepositoryID, req.Sender)
	if err != nil {
		return nostr.Event{}, fmt.Errorf("check repository maintainer: %w", err)
	}
	if !maintainer {
		return nostr.Event{}, ErrNotMaintainer
	}
	return announcement, nil
}

// Enroll processes a validated enrollment request idempotently.
func (s *Service) Enroll(ctx context.Context, req Request) (Receipt, error) {
	announcement, err := s.authorize(ctx, req)
	if err != nil {
		return Receipt{}, err
	}
	now := s.now()
	requestCreated := int64(req.Event.CreatedAt)
	author := req.Sender.Hex()
	unlock := s.lock(req.Ref.Address, author)
	defer unlock()

	// Idempotency and replay: if the stored command (enrollment or revocation)
	// is this request or newer, nothing changes; answer from the stored row
	// after making sure the live registry matches it, so a retry after a
	// failed activation repairs coverage without charging again.
	existing, found, err := s.store.GetMonitoringEnrollment(ctx, req.Ref.Address, author)
	if err != nil {
		return Receipt{}, err
	}
	if found && !existing.Supersedes(req.Event.ID.Hex(), requestCreated) {
		row, found, err := s.project(ctx, req.Ref, req.Sender)
		if err != nil {
			return Receipt{}, err
		}
		return s.receipt(row, stateOf(row, found)), nil
	}

	expiresAt, err := s.resolveExpiry(now, req.ExpiresAt)
	if err != nil {
		return Receipt{}, err
	}
	accessKind, err := s.charge(ctx, req)
	if err != nil {
		return Receipt{}, err
	}

	row := db.MonitoringEnrollment{
		RepositoryAddress:   req.Ref.Address,
		AuthorPubkey:        author,
		RequestEventID:      req.Event.ID.Hex(),
		RequestCreatedAt:    requestCreated,
		AnnouncementEventID: announcement.ID.Hex(),
		AccessKind:          accessKind,
		ExpiresAt:           expiresAt,
	}
	applied, err := s.store.ApplyMonitoringEnrollment(ctx, row)
	if err != nil {
		return Receipt{}, err
	}
	stored, found, err := s.project(ctx, req.Ref, req.Sender)
	if err != nil {
		return Receipt{}, err
	}
	state := "enrolled"
	if !applied {
		state = stateOf(stored, found)
	}
	s.logger.Info("monitoring enrollment processed",
		"repo", req.Ref.Address, "author", author, "request_event_id", row.RequestEventID,
		"applied", applied, "access_kind", accessKind, "expires_at", expiresAt, "monitored", s.registry.Contains(req.Ref.Address))
	return s.receipt(stored, state), nil
}

// Unenroll records the sender's revocation for the repository and projects
// the stored winner, so a stale revocation changes nothing.
func (s *Service) Unenroll(ctx context.Context, req Request) (Receipt, error) {
	if _, err := s.authorize(ctx, req); err != nil {
		return Receipt{}, err
	}
	author := req.Sender.Hex()
	unlock := s.lock(req.Ref.Address, author)
	defer unlock()

	applied, err := s.store.RevokeMonitoringEnrollment(ctx, req.Ref.Address, author, req.Event.ID.Hex(), int64(req.Event.CreatedAt))
	if err != nil {
		return Receipt{}, err
	}
	stored, found, err := s.project(ctx, req.Ref, req.Sender)
	if err != nil {
		return Receipt{}, err
	}
	state := stateOf(stored, found)
	if applied {
		state = "revoked"
	}
	receipt := s.receipt(stored, state)
	receipt.RepoAddr = req.Ref.Address
	receipt.Author = author
	if applied {
		receipt.EnrollmentID = req.Event.ID.Hex()
	}
	return receipt, nil
}

func (s *Service) resolveExpiry(now time.Time, requested int64) (int64, error) {
	if requested > 0 {
		if requested <= now.Unix() || requested > now.Add(s.policy.maxTTL()).Unix() {
			return 0, ErrInvalidExpiry
		}
		return requested, nil
	}
	if s.policy.TTL > 0 {
		return now.Add(s.policy.TTL).Unix(), nil
	}
	return 0, nil
}

// charge applies the enrollment price through the shared payment authorizer.
// The request event is the paying event: it carries the payment tag exactly
// like a patch would. Maintainers are not free here; being a maintainer is the
// authority precondition, not a discount.
func (s *Service) charge(ctx context.Context, req Request) (string, error) {
	if s.policy.PriceSats <= 0 {
		return AccessFree, nil
	}
	if s.authorizer == nil {
		return "", ErrPaymentsUnavailable
	}
	notFree := false
	result, err := s.authorizer.AuthorizePatch(ctx, req.Event, req.Ref.RepositoryID, repoconfig.PaymentsConfig{
		Enabled:            true,
		PriceSats:          s.policy.PriceSats,
		FreeForMaintainers: &notFree,
	})
	if err != nil {
		return "", fmt.Errorf("authorize enrollment payment: %w", err)
	}
	if !result.Allowed {
		return "", &PaymentDeniedError{Reason: result.Reason, Retryable: result.Retryable}
	}
	if result.AccessKind == "" {
		return AccessFree, nil
	}
	return result.AccessKind, nil
}

func (s *Service) receipt(row db.MonitoringEnrollment, state string) Receipt {
	return Receipt{
		EnrollmentID:        row.RequestEventID,
		RepoAddr:            row.RepositoryAddress,
		Author:              row.AuthorPubkey,
		AnnouncementEventID: row.AnnouncementEventID,
		AccessKind:          row.AccessKind,
		ExpiresAt:           row.ExpiresAt,
		Monitored:           s.registry.Contains(row.RepositoryAddress),
		State:               state,
	}
}
