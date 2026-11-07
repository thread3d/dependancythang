package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// Unconfined runs commands with no isolation at all.
//
// It exists only for environments without a sandbox primitive, and it is never
// selected implicitly: the caller has to configure `verify.sandbox: none`, and
// every result it produces is marked unconfined so the record shows that
// untrusted code ran against the real machine.
type Unconfined struct{}

// Name implements Sandbox.
func (Unconfined) Name() string { return "none" }

// Run implements Sandbox.
func (Unconfined) Run(ctx context.Context, req Request) (Result, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	out, err := runPlain(runCtx, req)
	timedOut := runCtx.Err() == context.DeadlineExceeded
	res := finish(req.Argv, out, exitCodeOf(err), timedOut, started)
	if timedOut {
		res.Output += "\n…command timed out"
	}
	return res, nil
}

// New selects a sandbox by name. Unrecognised names are an error rather than a
// silent fallback, because falling back would run untrusted code unconfined.
func New(name string) (Sandbox, error) {
	switch name {
	case "", "bubblewrap":
		return &Bubblewrap{}, nil
	case "container":
		return nil, fmt.Errorf("%w: container sandboxing is not implemented yet; use bubblewrap or none", ErrUnavailable)
	case "none":
		return Unconfined{}, nil
	default:
		return nil, fmt.Errorf("%w: unknown sandbox %q", ErrUnavailable, name)
	}
}

// runPlain executes the command with the provided environment and working
// directory, without isolation.
func runPlain(ctx context.Context, req Request) ([]byte, error) {
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = envList(req.Env)
	return cmd.CombinedOutput()
}
