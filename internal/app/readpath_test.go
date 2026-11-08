package app_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thread3d/dependancythang/internal/app"
	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/ecosystem/gomod"
	"github.com/thread3d/dependancythang/internal/httpcache"
	"github.com/thread3d/dependancythang/internal/planner"
	"github.com/thread3d/dependancythang/internal/policy"
)

// TestScanReportsOneActionPerTarget is the regression for a plan depmcp built
// and then refused with PLAN_STALE.
//
// A repository's workflows live in one directory, so they share one target. Two
// of them pinning the same action at the same ref is one dependency of that
// target — the applier rewrites both files from one change — but the scan used to
// report it once per file, which doubled it in `scan`, doubled it in `check`, and
// carried it into a plan twice.
func TestScanReportsOneActionPerTarget(t *testing.T) {
	ctx := context.Background()
	const action, ref = "actions/checkout", "v7"
	repoPath := workflowRepo(t, action, ref, "ci.yml", "lean.yml")
	svc := newService(t, repoPath)

	repoTarget, deps, err := svc.ScanDependencies(ctx, repoPath)
	if err != nil {
		t.Fatalf("ScanDependencies: %v", err)
	}

	const target = "actions:/.github/workflows"
	var found []string
	for _, d := range deps {
		if d.Name == action {
			found = append(found, d.Target)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s reported %d times (targets %v), want once per target", action, len(found), found)
	}
	if found[0] != target {
		t.Errorf("target = %q, want %q", found[0], target)
	}

	// The stored snapshot must hold the row too: a second manifest in the same
	// target must not replace the first one's inventory.
	rows, at, err := svc.Store.LoadInventory(ctx, repoTarget.ID, target)
	if err != nil {
		t.Fatalf("LoadInventory: %v", err)
	}
	if at == 0 {
		t.Error("inventory snapshot was not recorded")
	}
	if len(rows) != 1 || rows[0].Name != action || rows[0].Current != ref {
		t.Errorf("stored inventory = %+v, want one %s at %s", rows, action, ref)
	}
}

// gitRepoWithModules creates a repository holding one go.mod per directory, each
// requiring the same module.
func gitRepoWithModules(t *testing.T, module, version string, dirs ...string) string {
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
	for i, rel := range dirs {
		sub := filepath.Join(dir, rel)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		body := fmt.Sprintf("module example.com/mod%d\n\ngo 1.25\n\nrequire %s %s\n", i, module, version)
		if err := os.WriteFile(filepath.Join(sub, "go.mod"), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s/go.mod: %v", rel, err)
		}
	}
	run("add", "-A")
	run("commit", "-m", "initial")
	return dir
}

// TestPlanForTwoGoModFilesIsNotRejected covers the same defect in the shape every
// monorepo has: one module bump needed by two go.mod files. Those are two units of
// work in two targets, so the plan carries the bump twice and must still pass its
// own validation.
func TestPlanForTwoGoModFilesIsNotRejected(t *testing.T) {
	ctx := context.Background()
	const module, from, to = "example.com/dep", "v1.0.0", "v1.1.0"
	repoPath := gitRepoWithModules(t, module, from, ".", "svc")
	svc := newService(t, repoPath)

	repoTarget, deps, err := svc.ScanDependencies(ctx, repoPath)
	if err != nil {
		t.Fatalf("ScanDependencies: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("deps = %+v, want one per go.mod", deps)
	}

	eco := gomod.New()
	var decisions []policy.Decision
	for _, d := range deps {
		dec := policy.Decide(policy.Input{
			Dep: d, Candidates: []ecosystem.Version{{Raw: to}},
			Classify: eco.Classify, Compare: eco.Compare,
		}, policy.Rules{Now: time.Now()})
		dec.Ecosystem = "gomod"
		decisions = append(decisions, dec)
	}

	plan := planner.Build(decisions, "", repoTarget.ID, time.Now())
	if plan.TotalChanges != 2 {
		t.Fatalf("plan changes = %d, want 2 (one per go.mod)", plan.TotalChanges)
	}
	if len(plan.Groups) != 2 {
		t.Fatalf("groups = %d, want 2 (one per go.mod)", len(plan.Groups))
	}
	if problems := planner.Validate(plan, time.Now()); len(problems) != 0 {
		t.Errorf("the plan built for this repository is rejected as stale: %v", problems)
	}
	// The applied run is still guarded: a plan that lists the same change for one
	// target twice is refused, because the applier would collapse the two.
	plan.Groups[1].Target = plan.Groups[0].Target
	plan.Groups[1].Changes[0].Target = plan.Groups[0].Target
	if problems := planner.Validate(plan, time.Now()); len(problems) == 0 {
		t.Error("a duplicate change inside one target was accepted")
	}
}

// failingRegistry stands in for a registry that cannot be reached, which is what a
// rate-limited GitHub API looks like from here.
type failingRegistry struct{ err error }

func (f *failingRegistry) Versions(context.Context, string) ([]ecosystem.Version, error) {
	return nil, f.err
}

func (f *failingRegistry) PublishTime(context.Context, string, string) (string, error) {
	return "", nil
}

// TestPlanRefusesWhenTheRegistryCannotBeReached is the regression for a plan that
// silently described an unreachable registry as "nothing to update". A real case:
// api.github.com answered 403 for every tag lookup, and plan_updates returned a
// zero-change plan instead of saying so.
func TestPlanRefusesWhenTheRegistryCannotBeReached(t *testing.T) {
	ctx := context.Background()
	repoPath := gitRepoWithoutManifests(t)
	if err := os.WriteFile(filepath.Join(repoPath, "go.mod"),
		[]byte("module example.com/victim\n\ngo 1.25\n\nrequire example.com/dep v1.0.0\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = repoPath
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	commit := exec.Command("git", "commit", "-qm", "depend on one module")
	commit.Dir = repoPath
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}

	svc := newService(t, repoPath)
	svc.Registry = &failingRegistry{err: errors.New("unexpected status 403 Forbidden")}

	if _, err := svc.PlanUpdates(ctx, repoPath); err == nil {
		t.Fatal("a plan was produced from a registry that could not be reached")
	} else if got := app.CodeOf(err); got != "REGISTRY_UNAVAILABLE" {
		t.Errorf("code = %q, want REGISTRY_UNAVAILABLE (%v)", got, err)
	}

	// Offline with nothing cached is a different answer, and the caller can act on
	// it: come back online rather than retry.
	svc.Registry = &failingRegistry{err: fmt.Errorf("tags: %w", httpcache.ErrOfflineCacheMiss)}
	if _, err := svc.PlanUpdates(ctx, repoPath); err == nil {
		t.Fatal("an offline cache miss produced a plan")
	} else if got := app.CodeOf(err); got != "OFFLINE_CACHE_MISS" {
		t.Errorf("code = %q, want OFFLINE_CACHE_MISS (%v)", got, err)
	}
}
