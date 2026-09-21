package gitexec

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestCommandSetsHardenedEnv is the invariant the five unhardened forks lacked:
// every git subprocess disables credential prompts and optional locks so a
// review-pipeline worker can never block on a password prompt.
func TestCommandSetsHardenedEnv(t *testing.T) {
	cmd := Command(context.Background(), "/repo", "status", "--porcelain")

	wantArgs := []string{"git", "-C", "/repo", "status", "--porcelain"}
	if !slices.Equal(cmd.Args, wantArgs) {
		t.Fatalf("args = %v, want %v", cmd.Args, wantArgs)
	}
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"} {
		if !slices.Contains(cmd.Env, want) {
			t.Errorf("env missing %q; got %v", want, cmd.Env)
		}
	}
}

func TestRunBytesReturnsStdout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	out, err := RunBytes(context.Background(), t.TempDir(), "version")
	if err != nil {
		t.Fatalf("RunBytes: %v", err)
	}
	if !strings.HasPrefix(string(out), "git version") {
		t.Fatalf("output = %q, want git version prefix", out)
	}
}

func TestRunBytesExtractsStderr(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// A non-repository directory makes "git status" fail with a message on
	// stderr; RunBytes must surface that text rather than a bare exit code.
	_, err := RunBytes(context.Background(), t.TempDir(), "status")
	if err == nil {
		t.Fatal("expected error running git status outside a repository")
	}
	if !strings.HasPrefix(err.Error(), "git status:") {
		t.Fatalf("error = %q, want \"git status:\" prefix", err)
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("error = %q, want stderr text", err)
	}
}

func TestWaitError(t *testing.T) {
	if got := WaitError([]string{"log"}, nil, nil); got != nil {
		t.Fatalf("WaitError(nil) = %v, want nil", got)
	}

	sentinel := errors.New("context canceled")
	if got := WaitError([]string{"log"}, sentinel, nil); !errors.Is(got, sentinel) {
		t.Fatalf("non-exit error not passed through: %v", got)
	}

	// An *exec.ExitError with captured stderr is rendered with the git prefix
	// while remaining recoverable via errors.As.
	exitErr := &exec.ExitError{}
	got := WaitError([]string{"diff", "HEAD"}, exitErr, []byte("  boom\n"))
	if got == nil || got.Error() != "git diff HEAD: boom" {
		t.Fatalf("WaitError = %v, want \"git diff HEAD: boom\"", got)
	}
	var recovered *exec.ExitError
	if !errors.As(got, &recovered) {
		t.Fatalf("WaitError result does not unwrap to *exec.ExitError: %v", got)
	}
}

// TestRunBytesExitCodeRecoverable is the property DRYDOCK-9ysx's cost statement
// demands: a caller must be able to match on error identity and recover the exit
// code uniformly, rather than the shared helper baking in an unwrappable dead end.
func TestRunBytesExitCodeRecoverable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// "git status" outside a repository exits non-zero.
	_, err := RunBytes(context.Background(), t.TempDir(), "status")
	if err == nil {
		t.Fatal("expected error running git status outside a repository")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("errors.As(*exec.ExitError) failed for %v", err)
	}
	if exitErr.ExitCode() <= 0 {
		t.Fatalf("exit code = %d, want a recoverable non-zero code", exitErr.ExitCode())
	}
	if !strings.HasPrefix(err.Error(), "git status:") {
		t.Fatalf("error = %q, want \"git status:\" prefix", err)
	}
}
