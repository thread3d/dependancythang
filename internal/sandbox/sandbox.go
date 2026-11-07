// Package sandbox runs untrusted build and test commands with as little access
// to the machine as possible.
//
// This is Phase B of the two-phase model (§4.1): credentials live only in Phase
// A (apply), and verification gets a read-only view of the module cache, no
// network, an empty home, and a stripped environment.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ErrUnavailable reports that no sandbox can be constructed. Callers map it onto
// SANDBOX_UNAVAILABLE rather than silently running untrusted code (§11).
var ErrUnavailable = errors.New("sandbox unavailable")

// Request describes one command to run.
type Request struct {
	// Dir is the working directory inside the sandbox, bound read-write.
	Dir string
	// Argv is the command to execute.
	Argv []string
	// Env is the complete environment; nothing is inherited.
	Env map[string]string
	// ReadOnlyPaths are host paths made visible read-only (module cache, tools).
	ReadOnlyPaths []string
	// WritablePaths are host paths bound read-write (currently unused: the
	// worktree is the only writable mount).
	WritablePaths []string
	// Network allows network access. Default false.
	Network bool
	// Timeout bounds the command. Zero means ten minutes.
	Timeout time.Duration
}

// Result is the outcome of one sandboxed command.
type Result struct {
	Argv       []string `json:"argv"`
	ExitCode   int      `json:"exit_code"`
	Output     string   `json:"output"`
	Truncated  bool     `json:"truncated,omitempty"`
	TimedOut   bool     `json:"timed_out,omitempty"`
	DurationMS int64    `json:"duration_ms"`
}

// Passed reports whether the command succeeded.
func (r Result) Passed() bool { return r.ExitCode == 0 && !r.TimedOut }

// Sandbox executes one command in isolation.
type Sandbox interface {
	// Name identifies the implementation in output and audit records.
	Name() string
	// Run executes the command.
	Run(ctx context.Context, req Request) (Result, error)
}

// maxOutput bounds captured output so a hostile build cannot flood a context.
const maxOutput = 8 << 10

// truncate caps output and reports whether it did.
func truncate(s string) (string, bool) {
	if len(s) <= maxOutput {
		return s, false
	}
	return s[:maxOutput] + "\n…truncated", true
}

// finish turns a completed command into a Result.
func finish(argv []string, out []byte, exitCode int, timedOut bool, started time.Time) Result {
	text, truncated := truncate(strings.TrimSpace(string(out)))
	return Result{
		Argv:       argv,
		ExitCode:   exitCode,
		Output:     text,
		Truncated:  truncated,
		TimedOut:   timedOut,
		DurationMS: time.Since(started).Milliseconds(),
	}
}

// exitCodeOf extracts a process exit code, defaulting to 1 for other failures.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

// envList renders a map as the []string an exec.Cmd expects.
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// requireAbsolute rejects relative mount paths, which would silently resolve
// against whatever the caller's working directory happened to be.
func requireAbsolute(paths ...string) error {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("sandbox path %q must be absolute", p)
		}
	}
	return nil
}
