package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/sandbox"
	"github.com/thread3d/dependancythang/internal/store"
	"github.com/thread3d/dependancythang/internal/verify"
)

// Codes from verification (§7).
const (
	CodeSandboxUnavailable = "SANDBOX_UNAVAILABLE"
	CodeVerifyDisabled     = "VERIFY_DISABLED"
	CodeVerifyFailed       = "VERIFY_FAILED"
	CodeVerifyTimeout      = "VERIFY_TIMEOUT"
	CodeWorktreeMissing    = "WORKTREE_MISSING"
)

// Phase B environment. Nothing here is inherited from the caller: the sandbox
// gets exactly these variables and no credentials (§4.1).
//
// The module cache is mounted read-only, which is the point of the phase
// boundary. The build cache is deliberately a fresh tmpfs instead of a shared
// host directory: a writable cache shared between runs is a cache-poisoning path
// (§11), and recompiling is a cheaper price than trusting one.
func verifyEnv(gomodCache string) map[string]string {
	return map[string]string{
		"PATH":        "/usr/bin:/bin",
		"HOME":        "/tmp/home",
		"TMPDIR":      "/tmp",
		"GOMODCACHE":  gomodCache,
		"GOCACHE":     "/tmp/gocache",
		"GOFLAGS":     "-mod=readonly",
		"GOPROXY":     "off", // network is off; make the toolchain say so clearly
		"GOTOOLCHAIN": "local",
		"CGO_ENABLED": "0",
	}
}

// hostGoEnv asks the host toolchain where its caches live.
func hostGoEnv(ctx context.Context, key string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "env", key)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env %s: %w", key, err)
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", fmt.Errorf("go env %s returned nothing", key)
	}
	return value, nil
}

// SandboxFactory is a seam: production resolves the configured sandbox, tests
// inject a fake so verification logic is exercised without namespaces.
type SandboxFactory func(cfg SandboxConfig) (sandbox.Sandbox, error)

// SandboxConfig is the resolved sandbox settings.
type SandboxConfig struct {
	Kind    string
	Timeout time.Duration
}

func (s *Service) newSandbox(cfg SandboxConfig) (sandbox.Sandbox, error) {
	if s.Sandbox != nil {
		return s.Sandbox(cfg)
	}
	return sandbox.New(cfg.Kind)
}

// VerifyRun builds and tests a run's branch inside the sandbox and records the
// outcome. A failing verify is the normal, useful result: it is what blocks a
// bad update (§8 stage 7).
func (s *Service) VerifyRun(ctx context.Context, runID string) (store.Run, verify.Result, error) {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return store.Run{}, verify.Result{}, err
	}
	if run.Commit == "" {
		return run, verify.Result{}, coded(CodeNothingToApply, fmt.Errorf(
			"run %s has no commit to verify (status %s)", runID, run.Status))
	}
	if info, statErr := os.Stat(run.Worktree); statErr != nil || !info.IsDir() {
		return run, verify.Result{}, coded(CodeWorktreeMissing, fmt.Errorf(
			"worktree %s is gone; the run cannot be verified", run.Worktree))
	}

	vc := s.Cfg.Defaults.Verify
	if !vc.Enabled {
		return run, verify.Result{}, coded(CodeVerifyDisabled,
			errors.New("verify.enabled is false; a branch cannot be proven and will not be eligible for a PR"))
	}
	timeout, terr := time.ParseDuration(vc.Timeout)
	if terr != nil || timeout <= 0 {
		timeout = 15 * time.Minute
	}

	sb, err := s.newSandbox(SandboxConfig{Kind: vc.Sandbox, Timeout: timeout})
	if err != nil {
		return run, verify.Result{}, coded(CodeSandboxUnavailable, err)
	}
	if avail, ok := sb.(interface {
		Available(context.Context) error
	}); ok {
		if err := avail.Available(ctx); err != nil {
			return run, verify.Result{}, coded(CodeSandboxUnavailable, err)
		}
	}

	gomodCache, err := hostGoEnv(ctx, "GOMODCACHE")
	if err != nil {
		return run, verify.Result{}, coded(CodeSandboxUnavailable, err)
	}

	eco, err := ecosystemFor("gomod")
	if err != nil {
		return run, verify.Result{}, err
	}
	raw := eco.VerifyCommands(ecosystemTarget(run.Target))
	cmds := make([]verify.Command, 0, len(raw))
	for _, argv := range raw {
		cmds = append(cmds, verify.Command{Label: strings.Join(argv[1:], " "), Argv: argv})
	}

	lock, err := s.Lock(run.RepoID)
	if err != nil {
		return run, verify.Result{}, err
	}
	defer lock.Release()

	if err := s.Store.SetRunStatus(ctx, run.RunID, store.StatusVerifying, "verifying in sandbox"); err != nil {
		return run, verify.Result{}, err
	}

	result, err := verify.Run(ctx, sb, cmds, verify.Options{
		Dir:           run.Worktree,
		Env:           verifyEnv(gomodCache),
		ReadOnlyPaths: []string{gomodCache},
		Network:       false,
		Timeout:       timeout,
		StopOnFailure: true,
	})
	if err != nil {
		_ = s.Store.SetRunStatus(context.Background(), run.RunID, store.StatusFailed, "verification error: "+err.Error())
		return run, result, fmt.Errorf("verify run %s: %w", runID, err)
	}

	run.VerifySandbox = result.Sandbox
	run.VerifySummary = result.Summary
	run.VerifiedAt = time.Now().Unix()
	if result.Passed {
		run.Status = store.StatusVerified
		run.VerifyStatus = "passed"
	} else {
		run.Status = store.StatusFailed
		run.VerifyStatus = "failed"
	}
	if err := s.Store.SaveRun(ctx, run); err != nil {
		return run, result, err
	}

	if !result.Passed {
		code := CodeVerifyFailed
		for _, oc := range result.Commands {
			if oc.TimedOut {
				code = CodeVerifyTimeout
				break
			}
		}
		return run, result, coded(code, fmt.Errorf("verification did not pass: %s", result.Summary))
	}
	return run, result, nil
}

// ecosystemTarget rebuilds a target from a run's stored key such as "gomod:/".
func ecosystemTarget(target string) ecosystem.Target {
	eco, dir := "gomod", "/"
	if i := strings.Index(target, ":"); i >= 0 {
		eco, dir = target[:i], target[i+1:]
	}
	return ecosystem.Target{Ecosystem: eco, Dir: dir}
}

// VerifyArtifacts reports the sandbox in use and whether it can start, for
// `doctor` and `status`.
func (s *Service) VerifyArtifacts(ctx context.Context) (name string, problem error) {
	vc := s.Cfg.Defaults.Verify
	sb, err := s.newSandbox(SandboxConfig{Kind: vc.Sandbox})
	if err != nil {
		return vc.Sandbox, err
	}
	if avail, ok := sb.(interface {
		Available(context.Context) error
	}); ok {
		return sb.Name(), avail.Available(ctx)
	}
	return sb.Name(), nil
}
