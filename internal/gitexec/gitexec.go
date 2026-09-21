// Package gitexec runs git subprocesses with a hardened environment shared by
// every caller in the tree. Credential prompts are disabled so a review-pipeline
// worker can never block on an interactive password, optional locks are disabled
// so read-only commands do not contend for the index lock, and command failures
// surface the process stderr rather than a bare exit code.
package gitexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Command builds "git -C repo args..." with the hardened environment. Callers
// that need streaming or byte-limited output (see repo.Manager) build on this so
// they inherit the same environment and never re-fork the wrapper.
func Command(ctx context.Context, repo string, args ...string) *exec.Cmd {
	return Harden(exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...))
}

// Harden applies the shared hardened environment to a git command and returns
// it. Use it for invocations that cannot take the "-C repo" form of Command
// (notably "git clone", whose target directory does not exist yet) so they still
// disable credential prompts and optional locks.
func Harden(cmd *exec.Cmd) *exec.Cmd {
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// RunBytes runs a git command and returns its stdout. On failure it extracts the
// process stderr from the exit error so callers see the underlying git message;
// non-exit errors (context cancellation, missing binary) are returned verbatim.
func RunBytes(ctx context.Context, repo string, args ...string) ([]byte, error) {
	out, err := Command(ctx, repo, args...).Output()
	if err != nil {
		return nil, WaitError(args, err, nil)
	}
	return out, nil
}

// Run is the string form of RunBytes. Output is returned verbatim; callers that
// want surrounding whitespace removed trim it themselves.
func Run(ctx context.Context, repo string, args ...string) (string, error) {
	out, err := RunBytes(ctx, repo, args...)
	return string(out), err
}

// WaitError renders the standard failure error for a finished git command. For a
// non-zero exit it reports stderr (from stderr when captured separately, else
// from the ExitError) while keeping the underlying error recoverable via
// errors.As, so callers can still reach the *exec.ExitError and its exit code.
// Other errors are returned unchanged. It lets streaming callers produce the
// same error shape as RunBytes.
func WaitError(args []string, err error, stderr []byte) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if len(stderr) == 0 {
			stderr = exitErr.Stderr
		}
		return &gitError{
			msg: fmt.Sprintf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(stderr))),
			err: err,
		}
	}
	return err
}

// gitError carries the formatted "git <args>: <stderr>" message while keeping
// the underlying process error (typically *exec.ExitError) reachable through
// errors.As/errors.Unwrap, so an exit code is never lost behind the message.
type gitError struct {
	msg string
	err error
}

func (e *gitError) Error() string { return e.msg }
func (e *gitError) Unwrap() error { return e.err }
