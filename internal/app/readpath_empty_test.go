package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thread3d/dependancythang/internal/app"
)

// gitRepoWithoutManifests creates a repository with nothing for depmcp to read.
func gitRepoWithoutManifests(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# nothing to update\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	run("add", "-A")
	run("commit", "-m", "initial")
	return dir
}

// TestPlanForRepositoryWithNothingToUpdate is the regression for a plan built
// from zero groups, which panicked instead of answering "nothing to do".
func TestPlanForRepositoryWithNothingToUpdate(t *testing.T) {
	ctx := context.Background()
	repoPath := gitRepoWithoutManifests(t)
	svc := newService(t, repoPath)

	plan, err := svc.PlanUpdates(ctx, repoPath)
	if err != nil {
		t.Fatalf("PlanUpdates: %v", err)
	}
	if plan.PlanID == "" {
		t.Error("plan has no id")
	}
	if plan.TotalChanges != 0 || len(plan.Groups) != 0 {
		t.Errorf("plan = %d changes in %d groups, want none", plan.TotalChanges, len(plan.Groups))
	}

	// It was stored, so a client can hold its id, and applying it is a no-op
	// rather than a failure.
	stored, err := svc.GetPlan(ctx, plan.PlanID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if stored.PlanID != plan.PlanID {
		t.Errorf("stored plan id = %q, want %q", stored.PlanID, plan.PlanID)
	}
	if _, err := svc.ApplyPlan(ctx, plan.PlanID, true); err == nil {
		t.Error("applying an empty plan succeeded")
	} else if got := app.CodeOf(err); got != "NOTHING_TO_APPLY" {
		t.Errorf("code = %q, want NOTHING_TO_APPLY (%v)", got, err)
	}
}
