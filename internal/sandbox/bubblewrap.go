package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"time"
)

// Bubblewrap is a Linux sandbox built on user namespaces.
//
// The command gets: no network, an empty environment, a fresh /tmp, a read-only
// root filesystem, read-only access to the paths it needs (module cache, Go
// toolchain), and exactly one writable directory — the worktree.
type Bubblewrap struct {
	// Binary is the bwrap executable. Empty means "bwrap" from PATH.
	Binary string
}

// Name implements Sandbox.
func (b *Bubblewrap) Name() string { return "bubblewrap" }

// binary resolves the executable to use.
func (b *Bubblewrap) binary() string {
	if b.Binary != "" {
		return b.Binary
	}
	return "bwrap"
}

// Available checks that bwrap exists and can actually create a sandbox. Running
// `true` inside it is the only honest test: the binary can be present while
// user namespaces are disabled.
func (b *Bubblewrap) Available(ctx context.Context) error {
	path, err := exec.LookPath(b.binary())
	if err != nil {
		return fmt.Errorf("%w: bubblewrap not found in PATH", ErrUnavailable)
	}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probe, path,
		"--unshare-all", "--die-with-parent", "--clearenv",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind-try", "/bin", "/bin",
		"--dev", "/dev", "--proc", "/proc",
		"--setenv", "PATH", "/usr/bin:/bin",
		"/bin/sh", "-c", "true")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: bubblewrap cannot start a sandbox (%v: %s)",
			ErrUnavailable, err, firstLine(string(out)))
	}
	return nil
}

// Run implements Sandbox.
func (b *Bubblewrap) Run(ctx context.Context, req Request) (Result, error) {
	if err := requireAbsolute(append([]string{req.Dir}, req.ReadOnlyPaths...)...); err != nil {
		return Result{}, err
	}
	if len(req.Argv) == 0 {
		return Result{}, fmt.Errorf("sandbox: empty command")
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"--unshare-all", "--die-with-parent", "--new-session"}
	// A read-only root: the toolchain and libraries are visible, nothing else is
	// writable.
	for _, p := range []string{"/usr", "/lib", "/lib64", "/bin", "/sbin"} {
		args = append(args, "--ro-bind-try", p, p)
	}
	// /usr/bin/go is usually a symlink into /etc/alternatives; without this the
	// link dangles and the sandbox reports "execvp go: No such file or directory".
	args = append(args, "--ro-bind-try", "/etc/alternatives", "/etc/alternatives")
	// The private /tmp must be mounted *before* the worktree bind. A worktree
	// living under /tmp is common (and universal in tests), and mounting the
	// tmpfs afterwards would cover the worktree with an empty directory.
	args = append(args, "--tmpfs", "/tmp")
	if !req.Network {
		// The network namespace is already gone; a fresh /etc/ssl keeps the
		// sandbox from reading host TLS configuration it cannot use anyway.
		args = append(args, "--ro-bind-try", "/etc/ssl", "/etc/ssl")
	}
	for _, p := range req.ReadOnlyPaths {
		if p == "" {
			continue
		}
		args = append(args, "--ro-bind-try", p, p)
	}
	// The worktree is the single writable location.
	args = append(args, "--bind", req.Dir, req.Dir)
	args = append(args,
		"--dev", "/dev",
		"--proc", "/proc",
		"--chdir", req.Dir,
		"--clearenv",
	)
	env := req.Env
	if env == nil {
		env = map[string]string{"PATH": "/usr/bin:/bin"}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--setenv", k, env[k])
	}
	args = append(args, "--")
	args = append(args, req.Argv...)

	started := time.Now()
	cmd := exec.CommandContext(runCtx, b.binary(), args...)
	out, err := cmd.CombinedOutput()
	timedOut := runCtx.Err() == context.DeadlineExceeded

	res := finish(req.Argv, out, exitCodeOf(err), timedOut, started)
	if timedOut {
		res.Output += "\n…sandbox timed out"
	}
	return res, nil
}

// firstLine renders a short diagnostic from command output.
func firstLine(s string) string {
	s = string([]byte(s))
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}

// EnsureBwrapPath reports whether bwrap is on PATH, for doctor output.
func EnsureBwrapPath() string {
	if p, err := exec.LookPath("bwrap"); err == nil {
		return p
	}
	return ""
}

// hostHasUserNamespaces reports whether user namespaces are enabled.
func hostHasUserNamespaces() bool {
	data, err := os.ReadFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		return false
	}
	return len(data) > 0 && string(data) != "0\n"
}
