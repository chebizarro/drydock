package revieworder

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"drydock/internal/contextvm"
	"drydock/internal/db"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
)

const (
	MethodReviewOrder = "review/order"
	maxOrderLifetime  = 15 * time.Minute
)

// ReviewOrderParams is the session-independent ContextVM order contract.
type ReviewOrderParams struct {
	PatchEventID string `json:"patch_event_id"`
	RepoAddr     string `json:"repo_addr,omitempty"`
	Force        bool   `json:"force,omitempty"`
}

// ReviewOrderAccepted acknowledges durable intake, not review completion.
type ReviewOrderAccepted struct {
	Accepted       bool   `json:"accepted"`
	OrderID        string `json:"order_id"`
	RequestEventID string `json:"request_event_id"`
	PatchEventID   string `json:"patch_event_id"`
	RepoAddr       string `json:"repo_addr"`
	Forced         bool   `json:"forced"`
	State          string `json:"state"`
}

// OnDemandSubmitter is implemented by Service and kept narrow for handler tests.
type OnDemandSubmitter interface {
	SubmitOnDemand(context.Context, OnDemandRequest) (AcceptedOrder, error)
}

// Handler exposes generic stored-patch ordering over ContextVM.
type Handler struct {
	orders        OnDemandSubmitter
	servicePubkey string
	logger        *slog.Logger
	now           func() time.Time
}

func NewHandler(orders OnDemandSubmitter, servicePubkey string, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		orders:        orders,
		servicePubkey: scope.NormalizePubkey(servicePubkey),
		logger:        logger,
		now:           time.Now,
	}
}

func (h *Handler) RegisterContextVMMethods(router *contextvm.Router) error {
	if router == nil {
		return errors.New("contextvm router is required")
	}
	return router.Register(MethodReviewOrder, h.HandleReviewOrder)
}

func (h *Handler) HandleReviewOrder(ctx context.Context, request contextvm.Request) (any, *contextvm.Error) {
	if h.orders == nil {
		return nil, &contextvm.Error{Code: contextvm.ErrorInternal, Message: "review order service is not configured"}
	}
	if request.Msg.ID == "" {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidRequest, Message: "review/order requires a JSON-RPC id"}
	}
	params, rpcErr := contextvm.ParamsAs[ReviewOrderParams](request)
	if rpcErr != nil {
		return nil, rpcErr
	}
	params.PatchEventID = strings.TrimSpace(params.PatchEventID)
	params.RepoAddr = strings.TrimSpace(params.RepoAddr)
	if params.PatchEventID == "" {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "patch_event_id is required; repo_addr alone is not a deterministic target"}
	}
	eventRepository, rpcErr := h.validateEnvelope(request, params)
	if rpcErr != nil {
		return nil, rpcErr
	}

	repositoryAddress := params.RepoAddr
	if repositoryAddress == "" {
		repositoryAddress = eventRepository
	}
	accepted, err := h.orders.SubmitOnDemand(ctx, OnDemandRequest{
		PatchEventID:      params.PatchEventID,
		RepositoryAddress: repositoryAddress,
		RequesterPubkey:   request.Sender.Hex(),
		OrderID:           request.Msg.ID,
		RequestEventID:    request.Event.ID.Hex(),
		Force:             params.Force,
		Invocation:        db.ReviewInvocationContextVM,
	})
	if err != nil {
		h.logger.Warn("ContextVM review order rejected",
			"request_event_id", request.Event.ID.Hex(),
			"order_id", request.Msg.ID,
			"requester", request.Sender.Hex(),
			"patch_event_id", params.PatchEventID,
			"error", err,
		)
		return nil, reviewOrderRPCError(err)
	}
	if eventRepository != "" && accepted.Receipt.RepositoryAddress != eventRepository {
		return nil, &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "a tag does not match resolved repository"}
	}

	state := "queued"
	if accepted.RetryPending {
		state = "retry_pending"
	}
	return ReviewOrderAccepted{
		Accepted:       true,
		OrderID:        accepted.Receipt.OrderID,
		RequestEventID: accepted.Receipt.RequestEventID,
		PatchEventID:   accepted.Receipt.PatchEventID,
		RepoAddr:       accepted.Receipt.RepositoryAddress,
		Forced:         accepted.Receipt.Force,
		State:          state,
	}, nil
}

func (h *Handler) validateEnvelope(request contextvm.Request, params ReviewOrderParams) (string, *contextvm.Error) {
	if request.Event.ID == nostr.ZeroID || request.Sender == nostr.ZeroPK {
		return "", &contextvm.Error{Code: contextvm.ErrorInvalidRequest, Message: "authenticated request event and sender are required"}
	}
	recipients := tagValues(request.Event.Tags, "p")
	if len(recipients) != 1 {
		return "", &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "exactly one p tag is required"}
	}
	recipient, err := scope.ParsePubkey(recipients[0])
	if err != nil || h.servicePubkey == "" || recipient.Hex() != h.servicePubkey {
		return "", &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "p tag must address the Drydock service"}
	}
	methods := tagValues(request.Event.Tags, "method")
	if len(methods) != 1 || methods[0] != MethodReviewOrder {
		return "", &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "exactly one matching method tag is required"}
	}
	targets := tagValues(request.Event.Tags, "e")
	if len(targets) != 1 || targets[0] != params.PatchEventID {
		return "", &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "exactly one e tag matching patch_event_id is required"}
	}
	expirations := tagValues(request.Event.Tags, "expiration")
	if len(expirations) != 1 {
		return "", &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "exactly one expiration tag is required"}
	}
	expiresAt, err := strconv.ParseInt(expirations[0], 10, 64)
	if err != nil {
		return "", &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "expiration must be a Unix timestamp"}
	}
	now := h.now()
	if expiresAt <= now.Unix() {
		return "", &contextvm.Error{Code: contextvm.ErrorExpired, Message: "review order expired"}
	}
	if expiresAt > time.Unix(int64(request.Event.CreatedAt), 0).Add(maxOrderLifetime).Unix() {
		return "", &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "expiration exceeds the 15 minute order lifetime"}
	}

	addresses := tagValues(request.Event.Tags, "a")
	if len(addresses) > 1 {
		return "", &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "at most one a tag is allowed"}
	}
	if len(addresses) == 0 {
		return "", nil
	}
	repository, err := scope.ParseRepositoryRef(addresses[0])
	if err != nil {
		return "", &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: err.Error()}
	}
	if params.RepoAddr != "" {
		paramRepository, err := scope.ParseRepositoryRef(params.RepoAddr)
		if err != nil || paramRepository.Address != repository.Address {
			return "", &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: "repo_addr does not match a tag"}
		}
	}
	return repository.Address, nil
}

func reviewOrderRPCError(err error) *contextvm.Error {
	switch {
	case errors.Is(err, ErrInvalidTarget):
		return &contextvm.Error{Code: contextvm.ErrorInvalidParams, Message: err.Error()}
	case errors.Is(err, ErrSecurityCeiling), errors.Is(err, ErrForceDenied):
		return &contextvm.Error{Code: contextvm.ErrorUnauthorized, Message: err.Error()}
	case errors.Is(err, ErrTargetNotFound):
		return &contextvm.Error{Code: contextvm.ErrorNotFound, Message: err.Error()}
	case errors.Is(err, ErrOrderConflict):
		return &contextvm.Error{Code: contextvm.ErrorConflict, Message: err.Error()}
	case errors.Is(err, ErrRateLimited):
		return &contextvm.Error{Code: contextvm.ErrorRateLimited, Message: err.Error()}
	}
	var paymentErr *PaymentDeniedError
	if errors.As(err, &paymentErr) {
		data, _ := json.Marshal(map[string]any{
			"reason":    paymentErr.Reason,
			"retryable": paymentErr.Retryable,
		})
		return &contextvm.Error{Code: contextvm.ErrorPaymentRequired, Message: "payment required", Data: data}
	}
	return &contextvm.Error{Code: contextvm.ErrorInternal, Message: "review order failed"}
}

func tagValues(tags nostr.Tags, name string) []string {
	values := make([]string, 0, 1)
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == name {
			values = append(values, strings.TrimSpace(tag[1]))
		}
	}
	return values
}
