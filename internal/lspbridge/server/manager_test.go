package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"drydock/internal/lspbridge"
)

func TestManagerCommandConfigOverride(t *testing.T) {
	mgr := NewManager(nil, WithLSPCommandConfig(lspbridge.LangGo, lspbridge.LSPCommandConfig{
		Command: "/opt/drydock/bin/gopls",
		Args:    []string{"serve", "-remote=auto"},
	}))
	defer mgr.Shutdown()

	cfg, err := mgr.commandConfig(lspbridge.LangGo)
	if err != nil {
		t.Fatalf("commandConfig: %v", err)
	}
	if cfg.Command != "/opt/drydock/bin/gopls" {
		t.Fatalf("expected override command, got %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.Args, []string{"serve", "-remote=auto"}) {
		t.Fatalf("expected override args, got %+v", cfg.Args)
	}
}

func TestManagerCommandConfigDisable(t *testing.T) {
	mgr := NewManager(nil, WithLSPCommandConfig(lspbridge.LangRust, lspbridge.LSPCommandConfig{Disabled: true}))
	defer mgr.Shutdown()

	if _, err := mgr.commandConfig(lspbridge.LangRust); err == nil {
		t.Fatal("expected disabled language to return an error")
	}
}

func TestConfiguredLSPCommandConfigsFromEnv(t *testing.T) {
	t.Setenv("DRYDOCK_LSP_GO_COMMAND", "/custom/gopls")
	t.Setenv("DRYDOCK_LSP_GO_ARGS", "serve,-remote=auto")
	t.Setenv("DRYDOCK_LSP_RUST_DISABLED", "true")

	configs := configuredLSPCommandConfigs()
	goCfg := configs[lspbridge.LangGo]
	if goCfg.Command != "/custom/gopls" || !reflect.DeepEqual(goCfg.Args, []string{"serve", "-remote=auto"}) {
		t.Fatalf("unexpected Go config from env: %+v", goCfg)
	}
	if !configs[lspbridge.LangRust].Disabled {
		t.Fatalf("expected Rust to be disabled from env, got %+v", configs[lspbridge.LangRust])
	}
}

func TestManagerConcurrentGetOrStartStartsOneProcess(t *testing.T) {
	repo := t.TempDir()
	marker := filepath.Join(t.TempDir(), "starts")
	mgr := NewManager(nil, WithLSPCommandConfig(lspbridge.LangGo, lspbridge.LSPCommandConfig{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperLSPProcess", "--", marker},
	}))
	defer mgr.Shutdown()

	const callers = 16
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := mgr.GetOrStart(context.Background(), lspbridge.LangGo, repo)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("GetOrStart: %v", err)
		}
	}

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read start marker: %v", err)
	}
	if got := len(bytesLines(data)); got != 1 {
		t.Fatalf("expected one language server process, got %d starts", got)
	}
}

func TestManagerShutdownDoesNotBlockOtherOperations(t *testing.T) {
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()
	conn := newLSPConn(clientToServerW, serverToClientR)
	enteredShutdown := make(chan struct{})
	releaseShutdown := make(chan struct{})
	go func() {
		defer serverToClientW.Close()
		defer clientToServerR.Close()
		data, err := readFramedMessage(bufio.NewReader(clientToServerR))
		if err != nil {
			return
		}
		var req struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal(data, &req) != nil {
			return
		}
		close(enteredShutdown)
		<-releaseShutdown
		_ = writeFramedMessage(serverToClientW, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": nil})
	}()

	cmd := exec.Command("sh", "-c", "while :; do sleep 1; done")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	mgr := NewManager(nil)
	key := processKey{lang: lspbridge.LangGo, repoPath: t.TempDir()}
	mgr.mu.Lock()
	mgr.processes[key] = &managedProcess{cmd: cmd, conn: conn, lang: key.lang, repoPath: key.repoPath, lastUsed: time.Now()}
	mgr.mu.Unlock()

	shutdownDone := make(chan struct{})
	go func() {
		mgr.Shutdown()
		close(shutdownDone)
	}()
	<-enteredShutdown

	statusDone := make(chan struct{})
	go func() {
		mgr.ProcessStatus()
		close(statusDone)
	}()
	select {
	case <-statusDone:
		close(releaseShutdown)
	case <-time.After(500 * time.Millisecond):
		close(releaseShutdown)
		<-shutdownDone
		t.Fatal("ProcessStatus blocked behind language server shutdown")
	}
	<-shutdownDone
}

func TestHelperLSPProcess(t *testing.T) {
	marker := helperArgAfterSeparator()
	if marker == "" {
		return
	}
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(2)
	}
	_, _ = f.WriteString("start\n")
	_ = f.Close()

	reader := bufio.NewReader(os.Stdin)
	for {
		data, err := readFramedMessage(reader)
		if err != nil {
			return
		}
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(data, &req) != nil {
			continue
		}
		if req.Method == "exit" {
			return
		}
		if req.ID != nil {
			_ = writeFramedMessage(os.Stdout, map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{}})
		}
	}
}

func helperArgAfterSeparator() string {
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

func bytesLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}
