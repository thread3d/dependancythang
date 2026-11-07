package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thread3d/dependancythang/internal/app"
	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/sandbox"
	"github.com/thread3d/dependancythang/internal/store"
)

// fakeSandbox returns a canned result and records what it was asked to run.
type fakeSandbox struct {
	exitCode int
	ran      []string
}

func (f *fakeSandbox) Name() string { return "fake" }

func (f *fakeSandbox) Available(context.Context) error { return nil }

func (f *fakeSandbox) Run(_ context.Context, req sandbox.Request) (sandbox.Result, error) {
	label := ""
	if len(req.Argv) > 1 {
		label = req.Argv[1]
	}
	f.ran = append(f.ran, label)
	// A passing build, then a test outcome controlled by exitCode.
	code := 0
	if label == "test" {
		code = f.exitCode
	}
	return sandbox.Result{Argv: req.Argv, ExitCode: code, Output: "fake output"}, nil
}

// unavailableSandbox reports that it cannot start, like a kernel without user
// namespaces.
type unavailableSandbox struct{}

func (unavailableSandbox) Name() string { return "fake-unavailable" }

func (unavailableSandbox) Available(context.Context) error {
	return sandbox.ErrUnavailable
}

func (unavailableSandbox) Run(context.Context, sandbox.Request) (sandbox.Result, error) {
	return sandbox.Result{}, errors.New("should not run")
}

// applyOneRun prepares a repository, applies one plan, and returns the run.
func applyOneRun(t *testing.T) (*app.Service, store.Run, string) {
	t.Helper()
	const module, from, to = "example.com/dep", "v1.0.0", "v1.1.0"
	repoPath := gitRepo(t, module, from)
	svc := newService(t, repoPath)
	svc.Registry = &fakeRegistry{versions: map[string][]ecosystem.Version{
		module: {{Raw: to, PublishedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339)}},
	}}
	svc.Apply = fakeApplier(new([]ecosystem.Change))
	plan := seedPlan(t, svc, repoPath, module, from, to)

	run, err := svc.ApplyPlan(context.Background(), plan.PlanID, true)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	return svc, run, repoPath
}

func TestVerifyRunPassesAndMarksVerified(t *testing.T) {
	svc, run, _ := applyOneRun(t)

	fake := &fakeSandbox{}
	svc.Sandbox = func(app.SandboxConfig) (sandbox.Sandbox, error) { return fake, nil }

	updated, result, err := svc.VerifyRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatalf("VerifyRun: %v", err)
	}
	if !result.Passed {
		t.Errorf("result should pass: %s", result.Summary)
	}
	if updated.Status != store.StatusVerified {
		t.Errorf("status = %q, want %q", updated.Status, store.StatusVerified)
	}
	if updated.VerifyStatus != "passed" {
		t.Errorf("verify status = %q, want passed", updated.VerifyStatus)
	}
	if len(fake.ran) == 0 {
		t.Error("no commands were run")
	}
}

// TestVerifyRunBlocksFailingUpdate is M3's exit criterion: a bad update is
// caught before anything is proposed.
func TestVerifyRunBlocksFailingUpdate(t *testing.T) {
	svc, run, _ := applyOneRun(t)

	fake := &fakeSandbox{exitCode: 1}
	svc.Sandbox = func(app.SandboxConfig) (sandbox.Sandbox, error) { return fake, nil }

	updated, result, err := svc.VerifyRun(context.Background(), run.RunID)
	if err == nil {
		t.Fatal("a failing verification returned no error")
	}
	if got := app.CodeOf(err); got != app.CodeVerifyFailed {
		t.Errorf("code = %q, want %q (%v)", got, app.CodeVerifyFailed, err)
	}
	if result.Passed {
		t.Error("result claims to pass")
	}
	if updated.Status != store.StatusFailed {
		t.Errorf("status = %q, want %q", updated.Status, store.StatusFailed)
	}
	if updated.VerifyStatus != "failed" {
		t.Errorf("verify status = %q, want failed", updated.VerifyStatus)
	}
	if updated.VerifySummary == "" {
		t.Error("no summary recorded for the failure")
	}
}

func TestVerifyRunRefusesWithoutSandbox(t *testing.T) {
	svc, run, _ := applyOneRun(t)
	svc.Sandbox = func(app.SandboxConfig) (sandbox.Sandbox, error) { return unavailableSandbox{}, nil }

	_, _, err := svc.VerifyRun(context.Background(), run.RunID)
	if got := app.CodeOf(err); got != app.CodeSandboxUnavailable {
		t.Errorf("code = %q, want %q (%v)", got, app.CodeSandboxUnavailable, err)
	}
}

func TestVerifyRunRefusesWhenDisabled(t *testing.T) {
	svc, run, _ := applyOneRun(t)
	svc.Cfg.Defaults.Verify.Enabled = false

	_, _, err := svc.VerifyRun(context.Background(), run.RunID)
	if got := app.CodeOf(err); got != app.CodeVerifyDisabled {
		t.Errorf("code = %q, want %q (%v)", got, app.CodeVerifyDisabled, err)
	}
}

func TestVerifyRunRefusesAfterWorktreeRemoved(t *testing.T) {
	svc, run, _ := applyOneRun(t)
	if err := os.RemoveAll(filepath.Dir(run.Worktree)); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}

	_, _, err := svc.VerifyRun(context.Background(), run.RunID)
	if got := app.CodeOf(err); got != app.CodeWorktreeMissing {
		t.Errorf("code = %q, want %q (%v)", got, app.CodeWorktreeMissing, err)
	}
}
