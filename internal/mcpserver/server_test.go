package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

	for _, forbidden := range []string{agenttools.ToolSelectionAdd, agenttools.ToolSelectionFinalize, agenttools.ToolReviewSubmit} {
		result, callErr := session.CallTool(ctx, &mcp.CallToolParams{
			Name: forbidden, Arguments: map[string]any{},
		})
		if callErr == nil && (result == nil || !result.IsError) {
			t.Fatalf("external_readonly invoked hidden tool %s successfully", forbidden)
		}
	}
	for _, path := range []string{"../outside.txt", "/etc/passwd"} {
		result, callErr := session.CallTool(ctx, &mcp.CallToolParams{
			Name: agenttools.ToolCodeRead, Arguments: map[string]any{"path": path},
		})
		if callErr == nil && (result == nil || !result.IsError) {
			t.Fatalf("MCP traversal %q succeeded", path)
		}
		if result != nil {
			encoded, _ := json.Marshal(result.Content)
			if strings.Contains(string(encoded), "outside secret") {
				t.Fatalf("MCP traversal %q leaked outside content", path)
			}
		}
	}
}

type mcpByteCounter struct{}

func (mcpByteCounter) Count(text string) int { return len(text) }

func TestBoundServerToolsListIsRoleFiltered(t *testing.T) {
	registry := agenttools.NewRegistry()
	external := testScope(t, agenttools.RoleExternalReadonly)
	discovery := testScope(t, agenttools.RoleContextDiscovery)
	selection, err := agenttools.NewSelection(agenttools.SelectionConfig{
		Snapshot: discovery.Snapshot, ChangedFiles: []string{"main.go"},
		Counter: mcpByteCounter{}, TokenBudget: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	discovery.Selection = selection

	externalNames := listBoundTools(t, registry, external)
	discoveryNames := listBoundTools(t, registry, discovery)
	if externalNames[agenttools.ToolSelectionFinalize] || externalNames[agenttools.ToolReviewSubmit] {
		t.Fatalf("external tools/list = %v", externalNames)
	}
	if !discoveryNames[agenttools.ToolSelectionFinalize] || discoveryNames[agenttools.ToolReviewSubmit] {
		t.Fatalf("discovery tools/list = %v", discoveryNames)
	}
}

func listBoundTools(t *testing.T, registry *agenttools.Registry, scope *agenttools.Scope) map[string]bool {
	t.Helper()
	bound, err := NewBoundServer(registry, scope)
	if err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = bound.SDKServer().Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "list-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(listed.Tools))
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	return names
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
