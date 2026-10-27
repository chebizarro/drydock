package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/contextvm"
	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
)

const (
	MethodEnroll   = "monitoring/enroll"
	MethodUnenroll = "monitoring/unenroll"
	// maxRequestLifetime bounds the mandatory expiration tag so a captured
	// request cannot be replayed later.
	maxRequestLifetime = 15 * time.Minute
)

// EnrollParams is the monitoring/enroll contract.
type EnrollParams struct {
	RepoAddr  string `json:"repo_addr"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// UnenrollParams is the monitoring/unenroll contract.
type UnenrollParams struct {
	RepoAddr string `json:"repo_addr"`
}

// Enroller is implemented by Service and kept narrow for handler tests.
type Enroller interface {
	Enroll(context.Context, Request) (Receipt, error)
	Unenroll(context.Context, Request) (Receipt, error)
}

// Handler exposes enrollment over ContextVM.
type Handler struct {
	service       Enroller
	servicePubkey string
	logger        *slog.Logger
	now           func() time.Time
}

func NewHandler(service Enroller, servicePubkey string, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{service: service, servicePubkey: scope.NormalizePubkey(servicePubkey), logger: logger, now: time.Now}
}

func (h *Handler) RegisterContextVMMethods(router *contextvm.Router) error {
	if router == nil {
		return errors.New("contextvm router is required")
	}
	if err := router.Register(MethodEnroll, h.HandleEnroll); err != nil {
		return err
	}
	return router.Register(MethodUnenroll, h.HandleUnenroll)
}

func (h *Handler) HandleEnroll(ctx context.Context, request contextvm.Request) (any, *contextvm.Error) {
	params, rpcErr := contextvm.ParamsAs[EnrollParams](request)
	if rpcErr != nil {
		return nil, rpcErr
	}
	req, rpcErr := h.validate(request, MethodEnroll, params.RepoAddr)
	if rpcErr != nil {
		return nil, rpcErr
	}
	req.ExpiresAt = params.ExpiresAt
	receipt, err := h.service.Enroll(ctx, req)
	if err != nil {
		h.logger.Warn("ContextVM enrollment rejected", "request_event_id", request.Event.ID.Hex(), "sender", request.Sender.Hex(), "repo", req.Ref.Address, "error", err)
		return nil, RPCError(err)
	}
	return receipt, nil
}

func (h *Handler) HandleUnenroll(ctx context.Context, request contextvm.Request) (any, *contextvm.Error) {
	params, rpcErr := contextvm.ParamsAs[UnenrollParams](request)
	if rpcErr != nil {
		return nil, rpcErr
	}
	req, rpcErr := h.validate(request, MethodUnenroll, params.RepoAddr)
	if rpcErr != nil {
		return nil, rpcErr
	}
	receipt, err := h.service.Unenroll(ctx, req)
	if err != nil {
		h.logger.Warn("ContextVM unenrollment rejected", "request_event_id", request.Event.ID.Hex(), "sender", request.Sender.Hex(), "repo", req.Ref.Address, "error", err)
		return nil, RPCError(err)
	}
	return receipt, nil
}

// validate applies the shared envelope policy (service binding, method tag,
// mandatory in-window expiration) and resolves the repository target from the
// params and the optional a tag, which must agree.
func (h *Handler) validate(request contextvm.Request, method, repoAddr string) (Request, *contextvm.Error) {
	if h.service == nil {
		return Request{}, &contextvm.Error{Code: contextvm.ErrorInternal, Message: "enrollment service is not configured"}
	}
	if request.Msg.ID == "" {
		return Request{}, &contextvm.Error{Code: contextvm.ErrorInvalidRequest, Message: method + " requires a JSON-RPC id"}
	}
	if h.servicePubkey == "" {
		return Request{}, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "p tag must address the Drydock service"}
	}
	if rpcErr := contextvm.ValidateEnvelope(request, contextvm.EnvelopePolicy{
		ServicePubkey:      h.servicePubkey,
		Method:             method,
		ExpirationRequired: true,
		MaxLifetime:        maxRequestLifetime,
		Now:                h.now,
	}); rpcErr != nil {
		return Request{}, rpcErr
	}
	repoAddr = strings.TrimSpace(repoAddr)
	if repoAddr == "" {
		return Request{}, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "repo_addr is required"}
	}
	ref, err := scope.ParseRepositoryRef(repoAddr)
	if err != nil {
		return Request{}, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: err.Error()}
	}
	var addresses []string
	for _, tag := range request.Event.Tags {
		if len(tag) >= 2 && tag[0] == "a" {
			addresses = append(addresses, strings.TrimSpace(tag[1]))
		}
	}
	if len(addresses) > 1 {
		return Request{}, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "at most one a tag is allowed"}
	}
	if len(addresses) == 1 {
		tagged, err := scope.ParseRepositoryRef(addresses[0])
		if err != nil || tagged.Address != ref.Address {
			return Request{}, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "repo_addr does not match a tag"}
		}
	}
	if request.Sender == nostr.ZeroPK || request.Sender != request.Event.PubKey {
		return Request{}, &contextvm.Error{Code: contextvm.ErrorUnauthorized, Message: "sender must be the event signer"}
	}
	return Request{Event: request.Event, Sender: request.Sender, Ref: ref}, nil
}

// RPCError maps service errors to ContextVM protocol errors.
func RPCError(err error) *contextvm.Error {
	switch {
	case errors.Is(err, ErrInvalidTarget), errors.Is(err, ErrInvalidExpiry):
		return &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: err.Error()}
	case errors.Is(err, ErrSecurityCeiling), errors.Is(err, ErrNotMaintainer):
		return &contextvm.Error{Code: contextvm.ErrorUnauthorized, Message: err.Error()}
	case errors.Is(err, ErrAnnouncementUnknown):
		return &contextvm.Error{Code: contextvm.ErrorNotFound, Message: err.Error()}
	case errors.Is(err, ErrPaymentsUnavailable):
		return &contextvm.Error{Code: contextvm.ErrorInternal, Message: err.Error()}
	}
	var denied *PaymentDeniedError
	if errors.As(err, &denied) {
		data, _ := json.Marshal(map[string]any{"reason": denied.Reason, "retryable": denied.Retryable})
		return &contextvm.Error{Code: contextvm.ErrorPaymentRequired, Message: "payment required", Data: data}
	}
	return &contextvm.Error{Code: contextvm.ErrorInternal, Message: "enrollment failed"}
}
