package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"drydock/internal/agenttools"
	"drydock/internal/workspacesnapshot"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBoundServerFiltersAndDispatchesRegistryTools(t *testing.T) {
	scope := testScope(t, agenttools.RoleExternalReadonly)
	bound, err := NewBoundServer(agenttools.NewRegistry(), scope)
	if err != nil {
		t.Fatal(err)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- bound.SDKServer().Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	if !names[agenttools.ToolCodeRead] {
		t.Fatalf("code.read missing from tools/list: %v", names)
	}
	for _, unavailable := range []string{
		agenttools.ToolCodeReferences,
		agenttools.ToolContextLayer,
		agenttools.ToolGitRead,
		agenttools.ToolReviewSubmit,
		agenttools.ToolSelectionFinalize,
	} {
		if names[unavailable] {
			t.Fatalf("unavailable tool %s was advertised", unavailable)
		}
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      agenttools.ToolCodeRead,
		Arguments: map[string]any{"path": "main.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("code.read returned tool error: %#v", result.Content)
	}
	textContent, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content type %T", result.Content[0])
	}
	var envelope agenttools.Result
	if err := json.Unmarshal([]byte(textContent.Text), &envelope); err != nil {
		t.Fatalf("decode canonical envelope: %v", err)
	}
	if envelope.Content != "package example\n" {
		t.Fatalf("unexpected content %q", envelope.Content)
	}
}

func TestBoundServerFailsClosedWithoutScope(t *testing.T) {
	_, err := NewBoundServer(agenttools.NewRegistry(), nil)
	if !errors.Is(err, ErrUnscoped) {
		t.Fatalf("error = %v, want ErrUnscoped", err)
	}
}

func TestHTTPHandlerAuthenticatesBeforeBindingReadonlyScope(t *testing.T) {
	scope := testScope(t, agenttools.RoleExternalReadonly)
	resolutions := 0
	handler, err := NewHTTPHandler(agenttools.NewRegistry(), BearerAuthorizer{
		Token: "secret",
		Resolve: func(context.Context) (*agenttools.Scope, error) {
			resolutions++
			return scope, nil
		},
	}, HTTPOptions{MaxRequestBodyBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	unauthorized, err := http.Post(httpServer.URL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.StatusCode)
	}
	if resolutions != 0 {
		t.Fatalf("scope resolved before authentication: %d", resolutions)
	}

	httpClient := &http.Client{Transport: bearerTransport{token: "secret", base: http.DefaultTransport}}
	client := mcp.NewClient(&mcp.Implementation{Name: "http-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) == 0 || resolutions == 0 {
		t.Fatalf("authenticated MCP binding was not resolved: tools=%d resolutions=%d", len(listed.Tools), resolutions)
	}
}

func TestHTTPHandlerRejectsNonExternalRole(t *testing.T) {
	handler, err := NewHTTPHandler(agenttools.NewRegistry(), HTTPAuthorizerFunc(
		func(context.Context, *http.Request) (*agenttools.Scope, error) {
			return testScope(t, agenttools.RoleCodeReviewer), nil
		},
	), HTTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

func testScope(t *testing.T, role agenttools.Role) *agenttools.Scope {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(workspace+"/main.go", []byte("package example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := workspacesnapshot.NewManager(workspacesnapshot.Config{
		StorageRoot:     t.TempDir(),
		SnapshotTTL:     time.Hour,
		LeaseTTL:        time.Hour,
		SessionLifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.CreateMutable(context.Background(), workspacesnapshot.MutableCopyOptions{
		WorkspacePath: workspace,
		Allowlist:     []string{"."},
		TTL:           time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return agenttools.NewScope("test:"+snapshot.ID, snapshot, role)
}
