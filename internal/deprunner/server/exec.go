package server

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// commandResult is the bounded outcome of running a toolchain command.
type commandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// commandSpec fully describes one toolchain invocation. Env is the COMPLETE
// environment (the ambient process environment is never inherited), so
// credentials, proxy variables, and NPM_CONFIG_* from the sidecar host cannot
// leak into a run against untrusted content.
type commandSpec struct {
	Dir            string
	Name           string
	Args           []string
	Env            []string
	MaxOutputBytes int
}

// commandRunner executes a single toolchain command. It is an interface so the
// handler is testable without go/npm/cargo/pip present — the same reason
// lspbridge's handler abstracts its process manager. CI has no toolchains.
type commandRunner interface {
	Run(ctx context.Context, spec commandSpec) (commandResult, error)
}

// osCommandRunner is the production runner. It runs each command in its own
// process group, replaces (never inherits) the environment, caps captured
// output, and SIGKILLs the whole group when the context is cancelled so a
// hung or forking build cannot outlive its deadline.
type osCommandRunner struct{}

func (osCommandRunner) Run(ctx context.Context, spec commandSpec) (commandResult, error) {
	cmd := exec.Command(spec.Name, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env // full replacement; nil-safe (empty env) but callers always set it

	var stdout, stderr limitedBuffer
	stdout.limit = spec.MaxOutputBytes
	stderr.limit = spec.MaxOutputBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// New process group so cancellation kills descendants (build.rs, node, git)
	// too, not just the top-level process.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	res := commandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil // non-zero exit is a result, not a runner failure
		}
		// Context cancellation / kill surfaces here.
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, err
	}
	return res, nil
}

// limitedBuffer is an io.Writer that stops storing bytes past limit but keeps
// reporting the full written length to the writer so the child never blocks on a
// full pipe. A limit <= 0 means unbounded.
type limitedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit > 0 {
		remaining := b.limit - b.buf.Len()
		if remaining > 0 {
			if remaining > len(p) {
				remaining = len(p)
			}
			b.buf.Write(p[:remaining])
		}
	} else {
		b.buf.Write(p)
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}
