package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"drydock/internal/lspbridge"
)

const (
	// idleTTL is how long an idle LSP process lives before being reaped.
	idleTTL = 5 * time.Minute

	// reapInterval is how often the reaper checks for idle processes.
	reapInterval = 30 * time.Second

	// initTimeout is the max time to wait for LSP initialization.
	initTimeout = 30 * time.Second
)

// processKey uniquely identifies an LSP process.
type processKey struct {
	lang     string
	repoPath string
}

// managedProcess tracks a running language server.
type managedProcess struct {
	cmd      *exec.Cmd
	conn     *lspConn
	lang     string
	repoPath string
	lastUsed time.Time
}

// Manager manages language server processes.
type Manager struct {
	mu        sync.Mutex
	processes map[processKey]*managedProcess
	logger    *slog.Logger
	cancel    context.CancelFunc
}

// NewManager creates a process manager and starts the idle reaper.
func NewManager(logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		processes: make(map[processKey]*managedProcess),
		logger:    logger,
		cancel:    cancel,
	}
	go m.reapLoop(ctx)
	return m
}

// GetOrStart returns an existing LSP connection or starts a new one.
func (m *Manager) GetOrStart(ctx context.Context, lang, repoPath string) (*lspConn, error) {
	key := processKey{lang: lang, repoPath: repoPath}

	m.mu.Lock()
	if proc, ok := m.processes[key]; ok {
		proc.lastUsed = time.Now()
		m.mu.Unlock()
		return proc.conn, nil
	}
	m.mu.Unlock()

	// Start new process.
	cmdName := lspbridge.LSPCommand(lang)
	if cmdName == "" {
		return nil, fmt.Errorf("unsupported language: %s", lang)
	}

	// Check if the command exists.
	if _, err := exec.LookPath(cmdName); err != nil {
		return nil, fmt.Errorf("language server %q not found: %w", cmdName, err)
	}

	args := lspArgs(lang)
	cmd := exec.CommandContext(ctx, cmdName, args...)
	cmd.Dir = repoPath
	cmd.Stderr = os.Stderr // forward LSP server errors for debugging

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cmdName, err)
	}

	conn := newLSPConn(stdin, stdout)

	// Initialize the LSP connection.
	initCtx, initCancel := context.WithTimeout(ctx, initTimeout)
	defer initCancel()

	if err := initializeLSP(initCtx, conn, repoPath); err != nil {
		conn.close()
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("initialize %s: %w", cmdName, err)
	}

	proc := &managedProcess{
		cmd:      cmd,
		conn:     conn,
		lang:     lang,
		repoPath: repoPath,
		lastUsed: time.Now(),
	}

	m.mu.Lock()
	m.processes[key] = proc
	m.mu.Unlock()

	m.logger.Info("started language server", "lang", lang, "cmd", cmdName, "repo", repoPath)
	return conn, nil
}

// ProcessStatus returns the status of all managed processes.
func (m *Manager) ProcessStatus() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()

	status := make(map[string]string, len(m.processes))
	for k, p := range m.processes {
		idle := time.Since(p.lastUsed).Round(time.Second)
		status[k.lang+"@"+filepath.Base(k.repoPath)] = fmt.Sprintf("running (idle %s)", idle)
	}
	return status
}

// Shutdown kills all managed processes.
func (m *Manager) Shutdown() {
	m.cancel()

	m.mu.Lock()
	defer m.mu.Unlock()

	for key, proc := range m.processes {
		m.logger.Info("shutting down language server", "lang", key.lang, "repo", key.repoPath)
		shutdownLSP(proc.conn)
		proc.conn.close()
		proc.cmd.Process.Kill()
		proc.cmd.Wait()
		delete(m.processes, key)
	}
}

// reapLoop periodically checks for and kills idle processes.
func (m *Manager) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reapIdle()
		}
	}
}

func (m *Manager) reapIdle() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for key, proc := range m.processes {
		if now.Sub(proc.lastUsed) > idleTTL {
			m.logger.Info("reaping idle language server", "lang", key.lang, "repo", key.repoPath)
			shutdownLSP(proc.conn)
			proc.conn.close()
			proc.cmd.Process.Kill()
			proc.cmd.Wait()
			delete(m.processes, key)
		}
	}
}

// initializeLSP sends the LSP initialize/initialized handshake.
func initializeLSP(ctx context.Context, conn *lspConn, rootPath string) error {
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   fileURI(rootPath),
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"references": map[string]any{},
				"hover":      map[string]any{},
				"definition": map[string]any{},
			},
			"workspace": map[string]any{
				"symbol": map[string]any{},
			},
		},
	}

	_, err := conn.call(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	if err := conn.notify("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	return nil
}

// shutdownLSP sends the LSP shutdown/exit sequence.
func shutdownLSP(conn *lspConn) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn.call(ctx, "shutdown", nil)
	conn.notify("exit", nil)
}

// workspaceSymbol searches for symbols in the workspace.
func workspaceSymbol(ctx context.Context, conn *lspConn, query string) ([]json.RawMessage, error) {
	params := map[string]any{
		"query": query,
	}
	result, err := conn.call(ctx, "workspace/symbol", params)
	if err != nil {
		return nil, err
	}

	var symbols []json.RawMessage
	if err := json.Unmarshal(result, &symbols); err != nil {
		return nil, err
	}
	return symbols, nil
}

// lspArgs returns additional command-line arguments for specific language servers.
func lspArgs(lang string) []string {
	switch lang {
	case lspbridge.LangGo:
		return []string{"serve"}
	case lspbridge.LangTypeScript, lspbridge.LangJavaScript:
		return []string{"--stdio"}
	case lspbridge.LangPython:
		return nil // pylsp uses stdio by default
	case lspbridge.LangRust:
		return nil // rust-analyzer uses stdio by default
	case lspbridge.LangC, lspbridge.LangCPP:
		return nil // clangd uses stdio by default
	default:
		return nil
	}
}
