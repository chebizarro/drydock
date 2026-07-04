// Package server implements the LSP bridge HTTP server and language server process manager.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"drydock/internal/lspbridge"
)

// jsonRPCRequest is a JSON-RPC 2.0 request.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"` // nil for notifications
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonRPCResponse is a JSON-RPC 2.0 response.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// lspConn wraps a language server's stdio for JSON-RPC communication.
type lspConn struct {
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex // serializes writes
	seq    atomic.Int64

	pending   map[int64]chan jsonRPCResponse
	pendingMu sync.Mutex

	diagnostics   map[string][]lspbridge.Diagnostic // keyed by document URI
	diagnosticsMu sync.Mutex
}

func newLSPConn(stdin io.WriteCloser, stdout io.Reader) *lspConn {
	c := &lspConn{
		stdin:       stdin,
		stdout:      bufio.NewReaderSize(stdout, 64*1024),
		pending:     make(map[int64]chan jsonRPCResponse),
		diagnostics: make(map[string][]lspbridge.Diagnostic),
	}
	go c.readLoop()
	return c
}

// call sends a request and waits for the response.
func (c *lspConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.seq.Add(1)
	ch := make(chan jsonRPCResponse, 1)

	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  params,
	}
	if err := c.writeMessage(req); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("LSP error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// notify sends a notification (no response expected).
func (c *lspConn) notify(method string, params any) error {
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	return c.writeMessage(req)
}

// writeMessage sends a Content-Length framed JSON-RPC message.
func (c *lspConn) writeMessage(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := io.WriteString(c.stdin, header); err != nil {
		return err
	}
	_, err = c.stdin.Write(data)
	return err
}

// readLoop reads JSON-RPC responses from the language server stdout.
func (c *lspConn) readLoop() {
	for {
		data, err := c.readMessage()
		if err != nil {
			return // connection closed
		}

		var resp jsonRPCResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			continue
		}

		if resp.ID == nil {
			c.handleNotification(resp.Method, resp.Params)
			continue
		}

		c.pendingMu.Lock()
		ch, ok := c.pending[*resp.ID]
		c.pendingMu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

// readMessage reads one Content-Length framed message.
func (c *lspConn) readMessage() ([]byte, error) {
	var contentLength int

	// Read headers.
	for {
		line, err := c.stdout.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break // end of headers
		}
		if strings.HasPrefix(line, "Content-Length:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("parse Content-Length: %w", err)
			}
			contentLength = n
		}
	}

	if contentLength <= 0 {
		return nil, fmt.Errorf("invalid Content-Length: %d", contentLength)
	}

	data := make([]byte, contentLength)
	if _, err := io.ReadFull(c.stdout, data); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return data, nil
}

// close shuts down the connection.
func (c *lspConn) close() {
	c.stdin.Close()
}

func (c *lspConn) handleNotification(method string, params json.RawMessage) {
	if method != "textDocument/publishDiagnostics" || len(params) == 0 {
		return
	}
	var msg struct {
		URI         string `json:"uri"`
		Diagnostics []struct {
			Range struct {
				Start struct {
					Line int `json:"line"`
				} `json:"start"`
			} `json:"range"`
			Severity int    `json:"severity"`
			Source   string `json:"source"`
			Message  string `json:"message"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(params, &msg); err != nil || msg.URI == "" {
		return
	}
	diags := make([]lspbridge.Diagnostic, 0, len(msg.Diagnostics))
	for _, d := range msg.Diagnostics {
		diags = append(diags, lspbridge.Diagnostic{
			File:     uriToPath(msg.URI),
			Line:     d.Range.Start.Line + 1,
			Severity: lspDiagnosticSeverity(d.Severity),
			Message:  d.Message,
			Source:   d.Source,
		})
	}
	c.diagnosticsMu.Lock()
	c.diagnostics[msg.URI] = diags
	c.diagnosticsMu.Unlock()
}

func (c *lspConn) publishedDiagnostics(absPath, repoPath string) []lspbridge.Diagnostic {
	uri := fileURI(absPath)
	c.diagnosticsMu.Lock()
	diags := append([]lspbridge.Diagnostic(nil), c.diagnostics[uri]...)
	c.diagnosticsMu.Unlock()
	for i := range diags {
		diags[i].File = relPath(diags[i].File, repoPath)
	}
	return diags
}

func lspDiagnosticSeverity(severity int) string {
	switch severity {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	default:
		return "unknown"
	}
}

// lspSymbolKindName maps LSP SymbolKind numbers to readable names.
func lspSymbolKindName(kind int) string {
	names := map[int]string{
		1: "file", 2: "module", 3: "namespace", 4: "package",
		5: "class", 6: "method", 7: "property", 8: "field",
		9: "constructor", 10: "enum", 11: "interface", 12: "function",
		13: "variable", 14: "constant", 15: "string", 16: "number",
		17: "boolean", 18: "array", 19: "object", 20: "key",
		21: "null", 22: "enum-member", 23: "struct", 24: "event",
		25: "operator", 26: "type-parameter",
	}
	if name, ok := names[kind]; ok {
		return name
	}
	return fmt.Sprintf("kind-%d", kind)
}

// tokenType used in LSP requests.
const tokenTypeURI = "file://"

func fileURI(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return tokenTypeURI + path
}

func uriToPath(uri string) string {
	return strings.TrimPrefix(uri, tokenTypeURI)
}

func relPath(absPath, root string) string {
	if strings.HasPrefix(absPath, root) {
		rel := strings.TrimPrefix(absPath, root)
		return strings.TrimPrefix(rel, "/")
	}
	return absPath
}

// statusOK writes a JSON response.
func statusOK(w http.ResponseWriter, v any) {
	statusJSON(w, http.StatusOK, v)
}

func statusJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// statusError writes a JSON error response.
func statusError(w http.ResponseWriter, code int, msg string) {
	statusJSON(w, code, map[string]string{"error": msg})
}
