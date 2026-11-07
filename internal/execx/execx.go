// Package execx runs external commands. It is shared by every ecosystem so that
// tests can inject a fake toolchain once instead of per package.
package execx

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes a command in a directory and returns its combined output.
type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) (string, error)
}

// Exec runs commands with the ambient environment.
type Exec struct{}

// Run implements Runner.
func (Exec) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// LookPath reports whether a command is available, for doctor and graceful
// degradation when an ecosystem's CLI is missing.
func LookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
