// Package verify runs a project's build and tests inside a sandbox and decides
// whether an update is acceptable.
//
// It is deliberately the least trusting part of depmcp: the commands come from
// the ecosystem, but the environment, mounts, and network are decided here, and
// a failure stops the sequence (§8 stage 7).
package verify

import (
	"context"
	"fmt"
	"time"

	"github.com/thread3d/dependancythang/internal/sandbox"
)

// Command is one verification step, e.g. "go build ./...".
type Command struct {
	Label string   `json:"label"`
	Argv  []string `json:"argv"`
}

// Outcome is the result of one step.
type Outcome struct {
	Label      string   `json:"label"`
	Argv       []string `json:"argv"`
	Passed     bool     `json:"passed"`
	ExitCode   int      `json:"exit_code"`
	TimedOut   bool     `json:"timed_out,omitempty"`
	Output     string   `json:"output,omitempty"`
	Truncated  bool     `json:"truncated,omitempty"`
	DurationMS int64    `json:"duration_ms"`
}

// Result is the whole verification.
type Result struct {
	Passed     bool      `json:"passed"`
	Sandbox    string    `json:"sandbox"`
	Summary    string    `json:"summary"`
	Commands   []Outcome `json:"commands"`
	DurationMS int64     `json:"duration_ms"`
	// Unconfined marks a run that executed without isolation, so the record
	// never implies a safety property it did not have.
	Unconfined bool `json:"unconfined,omitempty"`
}

// Options configures a verification run.
type Options struct {
	// Dir is the worktree to build and test, mounted writable.
	Dir string
	// Env is the complete environment. Nothing is inherited from the caller.
	Env map[string]string
	// ReadOnlyPaths are host paths the sandbox may read (module cache, tools).
	ReadOnlyPaths []string
	// Network allows network access. Verification turns it off by default.
	Network bool
	// Timeout bounds each command.
	Timeout time.Duration
	// StopOnFailure ends the sequence at the first failing command.
	StopOnFailure bool
}

// Run executes the commands and reports whether everything passed.
func Run(ctx context.Context, sb sandbox.Sandbox, cmds []Command, opts Options) (Result, error) {
	if len(cmds) == 0 {
		return Result{}, fmt.Errorf("no verification commands to run")
	}
	started := time.Now()
	res := Result{Sandbox: sb.Name(), Unconfined: sb.Name() == "none"}

	for _, cmd := range cmds {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		out, err := sb.Run(ctx, sandbox.Request{
			Dir:           opts.Dir,
			Argv:          cmd.Argv,
			Env:           opts.Env,
			ReadOnlyPaths: opts.ReadOnlyPaths,
			Network:       opts.Network,
			Timeout:       opts.Timeout,
		})
		if err != nil {
			return res, fmt.Errorf("run %s: %w", cmd.Label, err)
		}
		oc := Outcome{
			Label: cmd.Label, Argv: cmd.Argv,
			Passed: out.Passed(), ExitCode: out.ExitCode, TimedOut: out.TimedOut,
			Output: out.Output, Truncated: out.Truncated, DurationMS: out.DurationMS,
		}
		res.Commands = append(res.Commands, oc)
		if !oc.Passed && opts.StopOnFailure {
			break
		}
	}

	res.DurationMS = time.Since(started).Milliseconds()
	res.Passed = len(res.Commands) > 0
	for _, oc := range res.Commands {
		if !oc.Passed {
			res.Passed = false
			break
		}
	}
	res.Summary = summarize(res)
	return res, nil
}

// summarize renders a one-line verdict naming the first failure.
func summarize(res Result) string {
	for _, oc := range res.Commands {
		if oc.Passed {
			continue
		}
		switch {
		case oc.TimedOut:
			return fmt.Sprintf("%s timed out", oc.Label)
		default:
			return fmt.Sprintf("%s failed (exit %d)", oc.Label, oc.ExitCode)
		}
	}
	labels := make([]string, 0, len(res.Commands))
	for _, oc := range res.Commands {
		labels = append(labels, oc.Label)
	}
	return fmt.Sprintf("%v passed", labels)
}
