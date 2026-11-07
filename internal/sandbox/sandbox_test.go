package sandbox_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/thread3d/dependancythang/internal/sandbox"
)

// requireBwrap skips when no working sandbox is available, so the suite stays
// runnable on machines or containers without user namespaces.
func requireBwrap(t *testing.T) *sandbox.Bubblewrap {
	t.Helper()
	sb := &sandbox.Bubblewrap{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sb.Available(ctx); err != nil {
		t.Skipf("bubblewrap unavailable here: %v", err)
	}
	return sb
}

// TestEnvironmentIsStripped is the §4.1 promise: no credentials reach Phase B.
func TestEnvironmentIsStripped(t *testing.T) {
	sb := requireBwrap(t)
	dir := t.TempDir()

	res, err := sb.Run(context.Background(), sandbox.Request{
		Dir:     dir,
		Argv:    []string{"/bin/sh", "-c", "env"},
		Env:     map[string]string{"PATH": "/usr/bin:/bin"}, // deliberately minimal
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Passed() {
		t.Fatalf("env exited %d: %s", res.ExitCode, res.Output)
	}
	// Only what we passed may appear. The shell adds PWD, SHLVL and _ itself,
	// so those are allowed; anything else is a leak.
	allowed := map[string]bool{"PATH": true, "PWD": true, "SHLVL": true, "_": true, "HOME": true}
	seen := map[string]string{}
	for _, line := range strings.Split(res.Output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Errorf("unparseable env line: %q", line)
			continue
		}
		if !allowed[key] {
			t.Errorf("variable %q leaked into the sandbox (value %q)", key, value)
		}
		seen[key] = value
	}
	// This request passes no HOME, so the sandbox must not invent one.
	if home, ok := seen["HOME"]; ok {
		t.Errorf("HOME = %q, want it unset when the caller did not provide one", home)
	}
}

func TestHostEnvironmentDoesNotLeak(t *testing.T) {
	sb := requireBwrap(t)
	t.Setenv("DEPMCP_TEST_SECRET", "sk-live-must-not-leak")

	res, err := sb.Run(context.Background(), sandbox.Request{
		Dir:     t.TempDir(),
		Argv:    []string{"/bin/sh", "-c", "echo HOME=$HOME; echo SECRET=$DEPMCP_TEST_SECRET"},
		Env:     map[string]string{"PATH": "/usr/bin:/bin", "HOME": "/tmp/home"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(res.Output, "sk-live-must-not-leak") {
		t.Errorf("a host secret reached the sandbox:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "HOME=/tmp/home") {
		t.Errorf("HOME was not the sandbox value:\n%s", res.Output)
	}
}

func TestNetworkIsIsolated(t *testing.T) {
	sb := requireBwrap(t)
	res, err := sb.Run(context.Background(), sandbox.Request{
		Dir:     t.TempDir(),
		Argv:    []string{"/bin/cat", "/proc/net/dev"},
		Env:     map[string]string{"PATH": "/usr/bin:/bin"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Passed() {
		t.Fatalf("cat /proc/net/dev exited %d: %s", res.ExitCode, res.Output)
	}
	// A fresh network namespace has loopback and nothing else.
	for _, line := range strings.Split(res.Output, "\n") {
		name, _, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}
		if iface := strings.TrimSpace(name); iface != "" && iface != "lo" {
			t.Errorf("sandbox sees a network interface %q; --unshare-all should have removed it", iface)
		}
	}
}

func TestWorkingDirectoryIsWritableAndRootIsNot(t *testing.T) {
	sb := requireBwrap(t)
	dir := t.TempDir()

	res, err := sb.Run(context.Background(), sandbox.Request{
		Dir:     dir,
		Argv:    []string{"/bin/sh", "-c", "touch .probe && echo worktree-writable; touch /usr/nope 2>&1 || echo root-readonly"},
		Env:     map[string]string{"PATH": "/usr/bin:/bin"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, "worktree-writable") {
		t.Errorf("worktree was not writable:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "root-readonly") {
		t.Errorf("/usr was writable inside the sandbox:\n%s", res.Output)
	}
}

func TestExitCodeAndTimeoutAreReported(t *testing.T) {
	sb := requireBwrap(t)

	failing, err := sb.Run(context.Background(), sandbox.Request{
		Dir:     t.TempDir(),
		Argv:    []string{"/bin/sh", "-c", "exit 7"},
		Env:     map[string]string{"PATH": "/usr/bin:/bin"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if failing.ExitCode != 7 || failing.Passed() {
		t.Errorf("exit code = %d, passed = %v; want 7 and false", failing.ExitCode, failing.Passed())
	}

	timed, err := sb.Run(context.Background(), sandbox.Request{
		Dir:     t.TempDir(),
		Argv:    []string{"/bin/sh", "-c", "sleep 30"},
		Env:     map[string]string{"PATH": "/usr/bin:/bin"},
		Timeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !timed.TimedOut || timed.Passed() {
		t.Errorf("timeout not reported: %+v", timed)
	}
}

func TestUnconfinedRunsWithoutIsolation(t *testing.T) {
	res, err := sandbox.Unconfined{}.Run(context.Background(), sandbox.Request{
		Dir:     t.TempDir(),
		Argv:    []string{"/bin/sh", "-c", "echo hi"},
		Env:     map[string]string{"PATH": "/usr/bin:/bin"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Passed() || !strings.Contains(res.Output, "hi") {
		t.Errorf("unconfined run failed: %+v", res)
	}
}

func TestNewRejectsUnknownSandbox(t *testing.T) {
	if _, err := sandbox.New("nonsense"); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Errorf("New(nonsense) error = %v, want ErrUnavailable", err)
	}
	if _, err := sandbox.New("container"); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Errorf("New(container) error = %v, want ErrUnavailable (not implemented)", err)
	}
	sb, err := sandbox.New("")
	if err != nil || sb.Name() != "bubblewrap" {
		t.Errorf("New(\"\") = %v, %v; want bubblewrap by default", sb, err)
	}
}

// ensure the test file compiles even when exec is only used indirectly
var _ = exec.Command
