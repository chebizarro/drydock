package mcpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"drydock/internal/agenttools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	ErrUnauthorized = errors.New("mcp server: unauthorized")
	ErrForbidden    = errors.New("mcp server: external access is read-only")
)

// HTTPAuthorizer resolves authentication to a server-created frozen scope.
// Implementations must never derive role, roots, or capabilities from request
// bodies or MCP tool arguments.
type HTTPAuthorizer interface {
	AuthorizeHTTP(context.Context, *http.Request) (*agenttools.Scope, error)
}

// HTTPAuthorizerFunc adapts a function to HTTPAuthorizer.
type HTTPAuthorizerFunc func(context.Context, *http.Request) (*agenttools.Scope, error)

func (f HTTPAuthorizerFunc) AuthorizeHTTP(ctx context.Context, request *http.Request) (*agenttools.Scope, error) {
	return f(ctx, request)
}

// BearerAuthorizer validates one operator-configured bearer secret before
// resolving the pre-authorized server-side snapshot/session binding.
type BearerAuthorizer struct {
	Token   string
	Resolve func(context.Context) (*agenttools.Scope, error)
}

func (a BearerAuthorizer) AuthorizeHTTP(ctx context.Context, request *http.Request) (*agenttools.Scope, error) {
	token, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok || a.Token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(a.Token)) != 1 {
		return nil, ErrUnauthorized
	}
	if a.Resolve == nil {
		return nil, ErrUnscoped
	}
	return a.Resolve(ctx)
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" ||
		strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

// HTTPOptions controls transport-level resource limits. HTTP is deliberately
// stateless so every request is authenticated and bound before the SDK creates
// its temporary protocol session.
type HTTPOptions struct {
	MaxRequestBodyBytes int64
}

// NewHTTPHandler builds an authenticated Streamable HTTP endpoint. External
// access is deliberately restricted to external_readonly until an
// external_reviewer submission binding is specified.
func NewHTTPHandler(registry *agenttools.Registry, authorizer HTTPAuthorizer, options HTTPOptions) (http.Handler, error) {
	if registry == nil || authorizer == nil {
		return nil, fmt.Errorf("mcp server: registry and HTTP authorizer are required")
	}
	type scopeKey struct{}
	sdkHandler := mcp.NewStreamableHTTPHandler(func(request *http.Request) *mcp.Server {
		scope, _ := request.Context().Value(scopeKey{}).(*agenttools.Scope)
		bound, err := NewBoundServer(registry, scope)
		if err != nil {
			return nil
		}
		return bound.SDKServer()
	}, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		MaxRequestBodyBytes:          options.MaxRequestBodyBytes,
		PropagateRequestCancellation: true,
	})

	authenticated := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		scope, err := authorizer.AuthorizeHTTP(request.Context(), request)
		switch {
		case errors.Is(err, ErrUnauthorized):
			response.Header().Set("WWW-Authenticate", `Bearer realm="drydock-mcp"`)
			http.Error(response, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		case err != nil:
			http.Error(response, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		case validateScope(scope) != nil:
			http.Error(response, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		case scope.Role != agenttools.RoleExternalReadonly:
			http.Error(response, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		sdkHandler.ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), scopeKey{}, scope)))
	})

	// Apply Go's standard cross-origin protection in addition to authentication.
	return http.NewCrossOriginProtection().Handler(authenticated), nil
}
