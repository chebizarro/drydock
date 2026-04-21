// NIP-5F Unix domain socket signer client.
//
// Connects to a Signet signer service over a Unix domain socket using
// the NIP-5F protocol: 4-byte big-endian length-prefixed JSON-RPC frames.
//
// Socket location: $NOSTR_SIGNER_SOCK or $HOME/.local/share/nostr/signer.sock
package signing

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"fiatjaf.com/nostr"
)

const (
	// maxFrameSize is the maximum allowed frame size (1 MiB per NIP-5F spec).
	maxFrameSize = 1 << 20

	// defaultSocketPath relative to $HOME.
	defaultSocketRel = ".local/share/nostr/signer.sock"

	// dialTimeout for the initial Unix socket connection.
	dialTimeout = 5 * time.Second

	// requestTimeout for individual sign/getpubkey requests.
	requestTimeout = 10 * time.Second
)

// SocketSigner implements nostr.Signer over a NIP-5F Unix domain socket.
type SocketSigner struct {
	socketPath string
	clientName string

	mu   sync.Mutex
	conn net.Conn
	seq  uint64
}

// SocketSignerConfig holds configuration for the socket signer.
type SocketSignerConfig struct {
	// SocketPath overrides the default socket location.
	// If empty, uses $NOSTR_SIGNER_SOCK or $HOME/.local/share/nostr/signer.sock
	SocketPath string
	// ClientName identifies this client in the handshake (default: "drydock").
	ClientName string
}

// handshakeMsg is the initial message sent by the signer upon connection.
type handshakeMsg struct {
	Name             string   `json:"name"`
	SupportedMethods []string `json:"supported_methods"`
}

// clientHello is the response the client sends after receiving the handshake.
type clientHello struct {
	Client string `json:"client"`
}

// rpcRequest is the JSON-RPC-like request format from NIP-5F.
type rpcRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params []any  `json:"params"`
}

// rpcResponse is the JSON-RPC-like response format from NIP-5F.
type rpcResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("signer error %d: %s", e.Code, e.Message)
}

// NewSocketSigner creates a socket signer client and performs the NIP-5F handshake.
// Returns the connected signer or an error if the socket is unreachable.
func NewSocketSigner(ctx context.Context, cfg SocketSignerConfig) (*SocketSigner, error) {
	path := resolveSocketPath(cfg.SocketPath)
	clientName := cfg.ClientName
	if clientName == "" {
		clientName = "drydock"
	}

	s := &SocketSigner{
		socketPath: path,
		clientName: clientName,
	}

	if err := s.connect(ctx); err != nil {
		return nil, fmt.Errorf("socket signer: %w", err)
	}

	// Validate by fetching public key.
	if _, err := s.GetPublicKey(ctx); err != nil {
		s.close()
		return nil, fmt.Errorf("socket signer validation failed: %w", err)
	}

	return s, nil
}

// GetPublicKey returns the public key from the signer.
func (s *SocketSigner) GetPublicKey(ctx context.Context) (nostr.PubKey, error) {
	resp, err := s.call(ctx, "get_public_key", nil)
	if err != nil {
		return nostr.PubKey{}, fmt.Errorf("get_public_key: %w", err)
	}

	var hexPubKey string
	if err := json.Unmarshal(resp, &hexPubKey); err != nil {
		return nostr.PubKey{}, fmt.Errorf("parse public key: %w", err)
	}

	pk, err := nostr.PubKeyFromHex(hexPubKey)
	if err != nil {
		return nostr.PubKey{}, fmt.Errorf("decode public key: %w", err)
	}
	return pk, nil
}

// SignEvent signs the event via the socket signer, setting ID, PubKey, and Sig.
func (s *SocketSigner) SignEvent(ctx context.Context, evt *nostr.Event) error {
	if evt == nil {
		return errors.New("nil event")
	}

	// Build the unsigned event payload for the signer.
	unsigned := map[string]any{
		"kind":       evt.Kind,
		"content":    evt.Content,
		"tags":       evt.Tags,
		"created_at": evt.CreatedAt,
	}

	// Get pubkey for second param.
	pk, err := s.GetPublicKey(ctx)
	if err != nil {
		return fmt.Errorf("sign_event get pubkey: %w", err)
	}

	resp, err := s.call(ctx, "sign_event", []any{unsigned, pk.Hex()})
	if err != nil {
		return fmt.Errorf("sign_event: %w", err)
	}

	// Parse the signed event response back into the event struct.
	var signed nostr.Event
	if err := json.Unmarshal(resp, &signed); err != nil {
		return fmt.Errorf("parse signed event: %w", err)
	}

	evt.ID = signed.ID
	evt.PubKey = signed.PubKey
	evt.Sig = signed.Sig
	return nil
}

// Close shuts down the socket connection.
func (s *SocketSigner) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

// call sends a JSON-RPC request and waits for the response.
func (s *SocketSigner) call(ctx context.Context, method string, params []any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Reconnect if needed.
	if s.conn == nil {
		if err := s.connectLocked(ctx); err != nil {
			return nil, err
		}
	}

	s.seq++
	reqID := fmt.Sprintf("drydock-%d", s.seq)

	req := rpcRequest{
		ID:     reqID,
		Method: method,
		Params: params,
	}
	if req.Params == nil {
		req.Params = []any{}
	}

	if err := s.writeFrame(req); err != nil {
		// Connection may be stale; try once to reconnect.
		s.closeLocked()
		if err := s.connectLocked(ctx); err != nil {
			return nil, fmt.Errorf("reconnect failed: %w", err)
		}
		s.seq++
		req.ID = fmt.Sprintf("drydock-%d", s.seq)
		if err := s.writeFrame(req); err != nil {
			return nil, fmt.Errorf("write request after reconnect: %w", err)
		}
	}

	// Set read deadline from context or default.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(requestTimeout)
	}
	s.conn.SetReadDeadline(deadline)

	var resp rpcResponse
	if err := s.readFrame(&resp); err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

// connect establishes the Unix socket connection and performs the handshake.
func (s *SocketSigner) connect(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connectLocked(ctx)
}

func (s *SocketSigner) connectLocked(ctx context.Context) error {
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", s.socketPath, err)
	}
	s.conn = conn

	// Read the signer's handshake message.
	conn.SetReadDeadline(time.Now().Add(dialTimeout))
	var hs handshakeMsg
	if err := s.readFrame(&hs); err != nil {
		conn.Close()
		s.conn = nil
		return fmt.Errorf("read handshake: %w", err)
	}

	// Send client hello.
	conn.SetWriteDeadline(time.Now().Add(dialTimeout))
	hello := clientHello{Client: s.clientName}
	if err := s.writeFrame(hello); err != nil {
		conn.Close()
		s.conn = nil
		return fmt.Errorf("send client hello: %w", err)
	}

	return nil
}

func (s *SocketSigner) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
}

func (s *SocketSigner) closeLocked() error {
	if s.conn != nil {
		err := s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}

// writeFrame writes a length-prefixed JSON frame to the connection.
func (s *SocketSigner) writeFrame(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}
	if len(data) > maxFrameSize {
		return fmt.Errorf("frame too large: %d bytes (max %d)", len(data), maxFrameSize)
	}

	// 4-byte big-endian length prefix.
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))

	s.conn.SetWriteDeadline(time.Now().Add(requestTimeout))
	if _, err := s.conn.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("write length prefix: %w", err)
	}
	if _, err := s.conn.Write(data); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}

// readFrame reads a length-prefixed JSON frame from the connection.
func (s *SocketSigner) readFrame(v any) error {
	var lenBuf [4]byte
	if _, err := io.ReadFull(s.conn, lenBuf[:]); err != nil {
		return fmt.Errorf("read length prefix: %w", err)
	}

	size := binary.BigEndian.Uint32(lenBuf[:])
	if size > maxFrameSize {
		return fmt.Errorf("frame too large: %d bytes (max %d)", size, maxFrameSize)
	}
	if size == 0 {
		return errors.New("empty frame")
	}

	data := make([]byte, size)
	if _, err := io.ReadFull(s.conn, data); err != nil {
		return fmt.Errorf("read payload: %w", err)
	}

	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unmarshal frame: %w", err)
	}
	return nil
}

// resolveSocketPath determines the socket path from config, env, or default.
func resolveSocketPath(configured string) string {
	if configured != "" {
		return configured
	}
	if env := os.Getenv("NOSTR_SIGNER_SOCK"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("/tmp", "nostr-signer.sock")
	}
	return filepath.Join(home, defaultSocketRel)
}
