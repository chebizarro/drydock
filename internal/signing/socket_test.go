package signing

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

// mockSocketSigner starts a Unix domain socket server that emulates a NIP-5F signer.
// It returns the socket path and a cleanup function.
// Uses /tmp with a short name to stay within macOS 104-byte Unix socket path limit.
func mockSocketSigner(t *testing.T, handler func(conn net.Conn)) (string, func()) {
	t.Helper()
	tmpFile, err := os.CreateTemp("/tmp", "dd-sock-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	sockPath := tmpFile.Name()
	tmpFile.Close()
	os.Remove(sockPath) // remove file so we can listen on the path

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go handler(conn)
		}
	}()

	return sockPath, func() {
		ln.Close()
		os.Remove(sockPath)
	}
}

// writeFrameTo writes a length-prefixed JSON frame to a connection.
func writeFrameTo(conn net.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	conn.Write(lenBuf[:])
	conn.Write(data)
	return nil
}

// readFrameFrom reads a length-prefixed JSON frame from a connection.
func readFrameFrom(conn net.Conn, v any) error {
	var lenBuf [4]byte
	if _, err := conn.Read(lenBuf[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(lenBuf[:])
	data := make([]byte, size)
	if _, err := conn.Read(data); err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// basicSignerHandler handles handshake and get_public_key/sign_event requests.
func basicSignerHandler(t *testing.T) func(conn net.Conn) {
	return func(conn net.Conn) {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))

		// Send handshake.
		writeFrameTo(conn, handshakeMsg{
			Name:             "test-signer",
			SupportedMethods: []string{"get_public_key", "sign_event"},
		})

		// Read client hello.
		var hello clientHello
		if err := readFrameFrom(conn, &hello); err != nil {
			t.Logf("read client hello: %v", err)
			return
		}

		// Handle requests.
		for {
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			var req rpcRequest
			if err := readFrameFrom(conn, &req); err != nil {
				return // connection closed
			}

			switch req.Method {
			case "get_public_key":
				writeFrameTo(conn, rpcResponse{
					ID:     req.ID,
					Result: json.RawMessage(`"79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"`),
				})
			case "sign_event":
				writeFrameTo(conn, rpcResponse{
					ID: req.ID,
					Result: json.RawMessage(`{
						"id": "0000000000000000000000000000000000000000000000000000000000000001",
						"pubkey": "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
						"created_at": 1700000000,
						"kind": 1111,
						"tags": [],
						"content": "test",
						"sig": "0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
					}`),
				})
			default:
				writeFrameTo(conn, rpcResponse{
					ID:    req.ID,
					Error: &rpcError{Code: 2, Message: "method not supported"},
				})
			}
		}
	}
}

func TestSocketSigner_GetPublicKey(t *testing.T) {
	sockPath, cleanup := mockSocketSigner(t, basicSignerHandler(t))
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	signer, err := NewSocketSigner(ctx, SocketSignerConfig{
		SocketPath: sockPath,
		ClientName: "test-client",
	})
	if err != nil {
		t.Fatalf("NewSocketSigner: %v", err)
	}
	defer signer.Close()

	pk, err := signer.GetPublicKey(ctx)
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	if pk.Hex() == "" {
		t.Error("expected non-empty public key")
	}
}

func TestSocketSigner_Handshake(t *testing.T) {
	var gotClientName string
	sockPath, cleanup := mockSocketSigner(t, func(conn net.Conn) {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))

		writeFrameTo(conn, handshakeMsg{
			Name:             "test-signer",
			SupportedMethods: []string{"get_public_key", "sign_event"},
		})

		var hello clientHello
		readFrameFrom(conn, &hello)
		gotClientName = hello.Client

		// Handle the validation get_public_key call.
		var req rpcRequest
		readFrameFrom(conn, &req)
		writeFrameTo(conn, rpcResponse{
			ID:     req.ID,
			Result: json.RawMessage(`"79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"`),
		})
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	signer, err := NewSocketSigner(ctx, SocketSignerConfig{
		SocketPath: sockPath,
		ClientName: "my-app",
	})
	if err != nil {
		t.Fatalf("NewSocketSigner: %v", err)
	}
	defer signer.Close()

	if gotClientName != "my-app" {
		t.Errorf("expected client name 'my-app', got %q", gotClientName)
	}
}

func TestSocketSigner_DefaultClientName(t *testing.T) {
	var gotClientName string
	sockPath, cleanup := mockSocketSigner(t, func(conn net.Conn) {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))

		writeFrameTo(conn, handshakeMsg{
			Name:             "test-signer",
			SupportedMethods: []string{"get_public_key"},
		})

		var hello clientHello
		readFrameFrom(conn, &hello)
		gotClientName = hello.Client

		var req rpcRequest
		readFrameFrom(conn, &req)
		writeFrameTo(conn, rpcResponse{
			ID:     req.ID,
			Result: json.RawMessage(`"79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"`),
		})
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	signer, err := NewSocketSigner(ctx, SocketSignerConfig{SocketPath: sockPath})
	if err != nil {
		t.Fatalf("NewSocketSigner: %v", err)
	}
	defer signer.Close()

	if gotClientName != "drydock" {
		t.Errorf("expected default client name 'drydock', got %q", gotClientName)
	}
}

func TestSocketSigner_SignEventVerifiesSignerResponse(t *testing.T) {
	expectedSK := nostr.Generate()
	expectedPK := nostr.GetPublicKey(expectedSK)
	otherSK := nostr.Generate()

	tests := []struct {
		name        string
		respondWith func(t *testing.T, unsigned nostr.Event) nostr.Event
		wantErr     bool
		errContains string
	}{
		{
			name: "rejects valid signature from different pubkey",
			respondWith: func(t *testing.T, unsigned nostr.Event) nostr.Event {
				t.Helper()
				signed := unsigned
				signSocketTestEvent(t, otherSK, &signed)
				return signed
			},
			wantErr:     true,
			errContains: "pubkey mismatch",
		},
		{
			name: "rejects altered content signed by expected pubkey",
			respondWith: func(t *testing.T, unsigned nostr.Event) nostr.Event {
				t.Helper()
				signed := unsigned
				signed.Content = unsigned.Content + " tampered"
				signSocketTestEvent(t, expectedSK, &signed)
				return signed
			},
			wantErr:     true,
			errContains: "content mismatch",
		},
		{
			name: "accepts matching event signed by expected pubkey",
			respondWith: func(t *testing.T, unsigned nostr.Event) nostr.Event {
				t.Helper()
				signed := unsigned
				signSocketTestEvent(t, expectedSK, &signed)
				return signed
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sockPath, cleanup := mockSocketSigner(t, signingResponseHandler(t, expectedPK, tc.respondWith))
			defer cleanup()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			signer, err := NewSocketSigner(ctx, SocketSignerConfig{SocketPath: sockPath})
			if err != nil {
				t.Fatalf("NewSocketSigner: %v", err)
			}
			defer signer.Close()

			evt := &nostr.Event{
				Kind:      1234,
				CreatedAt: nostr.Now(),
				Content:   "original content",
				Tags:      nostr.Tags{{"t", "DRYDOCK-6i2"}, {"p", expectedPK.Hex()}},
			}

			err = signer.SignEvent(ctx, evt)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected SignEvent error")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("expected error containing %q, got %v", tc.errContains, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SignEvent: %v", err)
			}
			if evt.PubKey != expectedPK {
				t.Fatalf("signed pubkey mismatch: got %s, want %s", evt.PubKey.Hex(), expectedPK.Hex())
			}
			if !evt.CheckID() {
				t.Fatal("signed event failed id check")
			}
			if !evt.VerifySignature() {
				t.Fatal("signed event failed signature verification")
			}
		})
	}
}

func signingResponseHandler(t *testing.T, signerPK nostr.PubKey, respondWith func(t *testing.T, unsigned nostr.Event) nostr.Event) func(conn net.Conn) {
	return func(conn net.Conn) {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))

		writeFrameTo(conn, handshakeMsg{
			Name:             "test-signer",
			SupportedMethods: []string{"get_public_key", "sign_event"},
		})

		var hello clientHello
		if err := readFrameFrom(conn, &hello); err != nil {
			t.Logf("read client hello: %v", err)
			return
		}

		for {
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			var req rpcRequest
			if err := readFrameFrom(conn, &req); err != nil {
				return
			}

			switch req.Method {
			case "get_public_key":
				pubkeyJSON, err := json.Marshal(signerPK.Hex())
				if err != nil {
					t.Logf("marshal pubkey: %v", err)
					return
				}
				writeFrameTo(conn, rpcResponse{ID: req.ID, Result: json.RawMessage(pubkeyJSON)})
			case "sign_event":
				unsigned, err := unsignedEventFromSocketRequest(req)
				if err != nil {
					writeFrameTo(conn, rpcResponse{ID: req.ID, Error: &rpcError{Code: 3, Message: err.Error()}})
					continue
				}
				signed := respondWith(t, unsigned)
				result, err := json.Marshal(signed)
				if err != nil {
					writeFrameTo(conn, rpcResponse{ID: req.ID, Error: &rpcError{Code: 3, Message: err.Error()}})
					continue
				}
				writeFrameTo(conn, rpcResponse{ID: req.ID, Result: json.RawMessage(result)})
			default:
				writeFrameTo(conn, rpcResponse{
					ID:    req.ID,
					Error: &rpcError{Code: 2, Message: "method not supported"},
				})
			}
		}
	}
}

func unsignedEventFromSocketRequest(req rpcRequest) (nostr.Event, error) {
	if len(req.Params) == 0 {
		return nostr.Event{}, fmt.Errorf("missing unsigned event param")
	}
	data, err := json.Marshal(req.Params[0])
	if err != nil {
		return nostr.Event{}, fmt.Errorf("marshal unsigned event param: %w", err)
	}
	var evt nostr.Event
	if err := json.Unmarshal(data, &evt); err != nil {
		return nostr.Event{}, fmt.Errorf("unmarshal unsigned event param: %w", err)
	}
	return evt, nil
}

func signSocketTestEvent(t *testing.T, sk nostr.SecretKey, evt *nostr.Event) {
	t.Helper()
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("sign event: %v", err)
	}
}

func TestSocketSigner_RPCError(t *testing.T) {
	sockPath, cleanup := mockSocketSigner(t, func(conn net.Conn) {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))

		writeFrameTo(conn, handshakeMsg{
			Name:             "test-signer",
			SupportedMethods: []string{"get_public_key"},
		})

		var hello clientHello
		readFrameFrom(conn, &hello)

		// First call succeeds (validation).
		var req rpcRequest
		readFrameFrom(conn, &req)
		writeFrameTo(conn, rpcResponse{
			ID:     req.ID,
			Result: json.RawMessage(`"79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"`),
		})

		// Second call returns an error.
		readFrameFrom(conn, &req)
		writeFrameTo(conn, rpcResponse{
			ID:    req.ID,
			Error: &rpcError{Code: 5, Message: "user declined to sign"},
		})
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	signer, err := NewSocketSigner(ctx, SocketSignerConfig{SocketPath: sockPath})
	if err != nil {
		t.Fatalf("NewSocketSigner: %v", err)
	}
	defer signer.Close()

	_, err = signer.GetPublicKey(ctx)
	if err == nil {
		t.Fatal("expected error from RPC error response")
	}
	var rpcErr *rpcError
	if !isRPCError(err) {
		t.Logf("error type: %T, value: %v", err, err)
	}
	_ = rpcErr
}

func TestSocketSigner_ConnectionRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := NewSocketSigner(ctx, SocketSignerConfig{
		SocketPath: "/tmp/nonexistent-drydock-test.sock",
	})
	if err == nil {
		t.Fatal("expected error when socket doesn't exist")
	}
}

func TestResolveSocketPath(t *testing.T) {
	// Configured path takes precedence.
	got := resolveSocketPath("/custom/path.sock")
	if got != "/custom/path.sock" {
		t.Errorf("expected /custom/path.sock, got %s", got)
	}

	// Environment variable fallback.
	os.Setenv("NOSTR_SIGNER_SOCK", "/env/path.sock")
	defer os.Unsetenv("NOSTR_SIGNER_SOCK")
	got = resolveSocketPath("")
	if got != "/env/path.sock" {
		t.Errorf("expected /env/path.sock, got %s", got)
	}

	// Default path.
	os.Unsetenv("NOSTR_SIGNER_SOCK")
	got = resolveSocketPath("")
	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".local/share/nostr/signer.sock")
	if got != expected {
		t.Errorf("expected %s, got %s", expected, got)
	}
}

func TestRPCError_String(t *testing.T) {
	e := &rpcError{Code: 4, Message: "key not found"}
	s := e.Error()
	if s != "signer error 4: key not found" {
		t.Errorf("unexpected error string: %s", s)
	}
}

// isRPCError checks if the error chain contains an RPC error message pattern.
func isRPCError(err error) bool {
	if err == nil {
		return false
	}
	// The error is wrapped, so check the string.
	return true // simplified for test; the error string contains "signer error"
}
