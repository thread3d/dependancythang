package app_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thread3d/dependancythang/internal/app"
	"github.com/thread3d/dependancythang/internal/config"
	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/planner"
	"github.com/thread3d/dependancythang/internal/policy"
	"github.com/thread3d/dependancythang/internal/store"
)

// fakeRegistry serves a fixed version list per module, with control over
// publish times and yanking so tests can simulate registry drift.
type fakeRegistry struct {
	versions map[string][]ecosystem.Version
	err      error
}

func (f *fakeRegistry) Versions(_ context.Context, module string) ([]ecosystem.Version, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.versions[module], nil
}

func (f *fakeRegistry) PublishTime(_ context.Context, _, version string) (string, error) {
	return "", nil
}

// fakeApplier rewrites go.mod in the worktree the way `go get` would, without
// needing a toolchain or network.
func fakeApplier(recorded *[]ecosystem.Change) func(context.Context, string, []ecosystem.Change) (ecosystem.ApplyResult, error) {
	return func(_ context.Context, dir string, changes []ecosystem.Change) (ecosystem.ApplyResult, error) {
		*recorded = changes
		path := filepath.Join(dir, "go.mod")
		data, err := os.ReadFile(path)
		if err != nil {
			return ecosystem.ApplyResult{}, err
		}
		text := string(data)
		for _, c := range changes {
			text = strings.ReplaceAll(text, c.From, c.To)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			return ecosystem.ApplyResult{}, err
		}
		res := ecosystem.ApplyResult{Observed: map[string][2]string{}, Changed: []string{}}
		for _, c := range changes {
			res.Observed[c.Name] = [2]string{c.From, c.To}
			res.Changed = append(res.Changed, c.Name)
		}
		return res, nil
	}
}

// gitRepo creates a git repository containing a go.mod that requires one module.
func gitRepo(t *testing.T, module, version string) string {
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
	goMod := "module example.com/victim\n\ngo 1.25\n\nrequire " + module + " " + version + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	run("add", "-A")
	run("commit", "-m", "initial")
	return dir
}

// newService opens a Service pointed at a temp config, with a tracked repository.
func newService(t *testing.T, repoPath string) *app.Service {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := "version: 1\ndefaults:\n  cooldown:\n    patch_days: 0\n    minor_days: 0\n    major_days: 0\nrepositories:\n  - path: " + repoPath + "\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(config.EnvConfig, configPath)
	t.Setenv(config.EnvStateDir, filepath.Join(dir, "state"))

	svc, err := app.Open(context.Background())
	if err != nil {
		t.Fatalf("open service: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// seedPlan builds and stores a one-change plan for the given repository.
func seedPlan(t *testing.T, svc *app.Service, repoPath, module, from, to string) planner.Plan {
	t.Helper()
	repoID := config.RepoID(repoPath)
	plan := planner.Plan{
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		RepoID:    repoID,
		Ecosystem: "gomod",
		Groups: []planner.Group{{
			Name: "default", Ecosystem: "gomod", Target: "gomod:/",
			Changes: []planner.Change{{
				Dependency: module, From: from, To: to,
				Class: ecosystem.ClassMinor, Direct: true, Target: "gomod:/",
			}},
		}},
		TotalChanges: 1,
	}
	plan.PlanID = "test-plan-" + to
	if err := svc.Store.SavePlan(context.Background(), plan.PlanID, repoID, "gomod:/", "gomod", time.Now(), mustJSON(t, plan)); err != nil {
		t.Fatalf("save plan: %v", err)
	}
	return plan
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func branchList(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "branch", "--list")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git branch: %v: %s", err, out)
	}
	return string(out)
}

// TestApplyPlanCreatesBranchAndRevertCleansUp is M2's exit criterion.
func TestApplyPlanCreatesBranchAndRevertCleansUp(t *testing.T) {
	ctx := context.Background()
	const module, from, to = "example.com/dep", "v1.0.0", "v1.1.0"
	repoPath := gitRepo(t, module, from)
	svc := newService(t, repoPath)

	svc.Registry = &fakeRegistry{versions: map[string][]ecosystem.Version{
		module: {{Raw: to, PublishedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339)}},
	}}
	var applied []ecosystem.Change
	svc.Apply = fakeApplier(&applied)

	plan := seedPlan(t, svc, repoPath, module, from, to)

	run, err := svc.ApplyPlan(ctx, plan.PlanID, true)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if run.Status != store.StatusApplied {
		t.Errorf("status = %q, want %q (detail: %s)", run.Status, store.StatusApplied, run.Detail)
	}
	if run.Commit == "" {
		t.Error("run has no commit id")
	}
	if len(applied) != 1 || applied[0].To != to {
		t.Errorf("applier received %+v, want one change to %s", applied, to)
	}

	// The branch exists and the worktree has the bumped go.mod.
	if !strings.Contains(branchList(t, repoPath), run.Branch) {
		t.Errorf("branch %q missing from %s", run.Branch, branchList(t, repoPath))
	}
	bumped, err := os.ReadFile(filepath.Join(run.Worktree, "go.mod"))
	if err != nil {
		t.Fatalf("read worktree go.mod: %v", err)
	}
	if !strings.Contains(string(bumped), to) {
		t.Errorf("worktree go.mod was not updated:\n%s", bumped)
	}

	// The user's working tree must be untouched.
	pristine, err := os.ReadFile(filepath.Join(repoPath, "go.mod"))
	if err != nil {
		t.Fatalf("read repo go.mod: %v", err)
	}
	if strings.Contains(string(pristine), to) {
		t.Errorf("the working tree was modified:\n%s", pristine)
	}

	// Revert removes both the worktree and the branch.
	reverted, err := svc.RevertRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("RevertRun: %v", err)
	}
	if reverted.Status != store.StatusReverted {
		t.Errorf("status after revert = %q, want %q", reverted.Status, store.StatusReverted)
	}
	if strings.Contains(branchList(t, repoPath), run.Branch) {
		t.Errorf("branch %q survived revert", run.Branch)
	}
	if _, err := os.Stat(run.Worktree); !os.IsNotExist(err) {
		t.Errorf("worktree %s survived revert: %v", run.Worktree, err)
	}
}

// TestApplyPlanRefusesStalePlan covers the TOCTOU rule from §8.
func TestApplyPlanRefusesStalePlan(t *testing.T) {
	ctx := context.Background()
	const module, from, to = "example.com/dep", "v1.0.0", "v2.0.0"
	repoPath := gitRepo(t, module, from)
	svc := newService(t, repoPath)

	// The registry no longer offers the planned version.
	svc.Registry = &fakeRegistry{versions: map[string][]ecosystem.Version{
		module: {{Raw: "v1.5.0", PublishedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339)}},
	}}
	var applied []ecosystem.Change
	svc.Apply = fakeApplier(&applied)
	plan := seedPlan(t, svc, repoPath, module, from, to)

	_, err := svc.ApplyPlan(ctx, plan.PlanID, true)
	if err == nil {
		t.Fatal("stale plan was applied")
	}
	if got := app.CodeOf(err); got != app.CodePlanStale {
		t.Errorf("code = %q, want %q (%v)", got, app.CodePlanStale, err)
	}
	if len(applied) != 0 {
		t.Error("applier ran even though the plan was stale")
	}
	if strings.Contains(branchList(t, repoPath), "depmcp/") {
		t.Error("a branch was created for a stale plan")
	}
}

func TestApplyPlanRefusesYankedAndCoolingVersions(t *testing.T) {
	ctx := context.Background()
	const module, from = "example.com/dep", "v1.0.0"

	t.Run("yanked", func(t *testing.T) {
		repoPath := gitRepo(t, module, from)
		svc := newService(t, repoPath)
		svc.Registry = &fakeRegistry{versions: map[string][]ecosystem.Version{
			module: {{Raw: "v1.1.0", Yanked: true, PublishedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339)}},
		}}
		svc.Apply = fakeApplier(new([]ecosystem.Change))
		plan := seedPlan(t, svc, repoPath, module, from, "v1.1.0")

		_, err := svc.ApplyPlan(ctx, plan.PlanID, true)
		if got := app.CodeOf(err); got != app.CodeVersionYanked {
			t.Errorf("code = %q, want %q (%v)", got, app.CodeVersionYanked, err)
		}
	})

	t.Run("still in cooldown", func(t *testing.T) {
		repoPath := gitRepo(t, module, from)
		svc := newService(t, repoPath)
		svc.Cfg.Defaults.Cooldown.MinorDays = 7
		svc.Registry = &fakeRegistry{versions: map[string][]ecosystem.Version{
			module: {{Raw: "v1.1.0", PublishedAt: time.Now().Add(-1 * time.Hour).Format(time.RFC3339)}},
		}}
		svc.Apply = fakeApplier(new([]ecosystem.Change))
		plan := seedPlan(t, svc, repoPath, module, from, "v1.1.0")

		_, err := svc.ApplyPlan(ctx, plan.PlanID, true)
		if got := app.CodeOf(err); got != app.CodeCooldownActive {
			t.Errorf("code = %q, want %q (%v)", got, app.CodeCooldownActive, err)
		}
	})
}

func TestApplyPlanRequiresConfirmation(t *testing.T) {
	ctx := context.Background()
	const module, from, to = "example.com/dep", "v1.0.0", "v1.1.0"
	repoPath := gitRepo(t, module, from)
	svc := newService(t, repoPath)
	svc.Registry = &fakeRegistry{versions: map[string][]ecosystem.Version{
		module: {{Raw: to, PublishedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339)}},
	}}
	svc.Apply = fakeApplier(new([]ecosystem.Change))
	plan := seedPlan(t, svc, repoPath, module, from, to)

	_, err := svc.ApplyPlan(ctx, plan.PlanID, false)
	if got := app.CodeOf(err); got != app.CodeConfirmationRequired {
		t.Errorf("code = %q, want %q (%v)", got, app.CodeConfirmationRequired, err)
	}
	if strings.Contains(branchList(t, repoPath), "depmcp/") {
		t.Error("a branch was created without confirmation")
	}
}

func TestRevertRefusesTerminalRun(t *testing.T) {
	ctx := context.Background()
	const module, from, to = "example.com/dep", "v1.0.0", "v1.1.0"
	repoPath := gitRepo(t, module, from)
	svc := newService(t, repoPath)
	svc.Registry = &fakeRegistry{versions: map[string][]ecosystem.Version{
		module: {{Raw: to, PublishedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339)}},
	}}
	svc.Apply = fakeApplier(new([]ecosystem.Change))
	plan := seedPlan(t, svc, repoPath, module, from, to)

	run, err := svc.ApplyPlan(ctx, plan.PlanID, true)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	// Pretend the branch was merged upstream.
	if err := svc.Store.SetRunStatus(ctx, run.RunID, store.StatusMerged, "merged upstream"); err != nil {
		t.Fatalf("SetRunStatus: %v", err)
	}

	_, err = svc.RevertRun(ctx, run.RunID)
	if got := app.CodeOf(err); got != app.CodeRunTerminal {
		t.Errorf("code = %q, want %q (%v)", got, app.CodeRunTerminal, err)
	}
	// The merged branch must survive.
	if !strings.Contains(branchList(t, repoPath), run.Branch) {
		t.Error("revert deleted the branch of a merged run")
	}
}

func TestListRuns(t *testing.T) {
	ctx := context.Background()
	const module, from, to = "example.com/dep", "v1.0.0", "v1.1.0"
	repoPath := gitRepo(t, module, from)
	svc := newService(t, repoPath)
	svc.Registry = &fakeRegistry{versions: map[string][]ecosystem.Version{
		module: {{Raw: to, PublishedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339)}},
	}}
	svc.Apply = fakeApplier(new([]ecosystem.Change))
	plan := seedPlan(t, svc, repoPath, module, from, to)

	if _, err := svc.ApplyPlan(ctx, plan.PlanID, true); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	runs, err := svc.ListRuns(ctx, repoPath, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].Status != store.StatusApplied {
		t.Errorf("status = %q, want applied", runs[0].Status)
	}
	if !strings.HasPrefix(runs[0].Branch, "depmcp/") {
		t.Errorf("branch = %q, want a depmcp/ prefix", runs[0].Branch)
	}
}

// workflowRepo creates a git repository whose workflow files all pin one action
// at one ref, the shape every repository with more than one workflow file has.
func workflowRepo(t *testing.T, action, ref string, files ...string) string {
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
	workflows := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(workflows, 0o755); err != nil {
		t.Fatalf("mkdir workflows: %v", err)
	}
	for _, name := range files {
		body := "on: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: " +
			action + "@" + ref + "\n"
		if err := os.WriteFile(filepath.Join(workflows, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	run("add", "-A")
	run("commit", "-m", "initial")
	return dir
}

// TestApplyPlanAcceptsOneActionRefInTwoWorkflowFiles is the regression for a plan
// depmcp built and then refused as PLAN_STALE: two workflow files sharing one
// action ref produce two changes with the same dependency and target version,
// which is work in two files rather than a duplicate.
func TestApplyPlanAcceptsOneActionRefInTwoWorkflowFiles(t *testing.T) {
	ctx := context.Background()
	const action, from, to = "actions/checkout", "v7", "v8"
	repoPath := workflowRepo(t, action, from, "ci.yml", "lean.yml")
	svc := newService(t, repoPath)
	svc.Registry = &fakeRegistry{versions: map[string][]ecosystem.Version{
		action: {{Raw: to, PublishedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339)}},
	}}

	// The read path detects one target per workflow file, so the plan carries the
	// same change twice, once per file.
	decisions := []policy.Decision{}
	for _, name := range []string{"ci.yml", "lean.yml"} {
		decisions = append(decisions, policy.Decision{
			Dependency: action, From: from, To: to,
			Class: ecosystem.ClassPin, Include: true, Direct: true,
			Ecosystem: "actions", Target: "actions:/.github/workflows/" + name,
		})
	}
	plan := planner.Build(decisions, "", config.RepoID(repoPath), time.Now())
	if plan.TotalChanges != 2 {
		t.Fatalf("plan changes = %d, want 2 (one per workflow file)", plan.TotalChanges)
	}
	plan.PlanID = "two-workflows-" + to
	if err := svc.Store.SavePlan(ctx, plan.PlanID, config.RepoID(repoPath), firstGroupTarget(plan), "actions", time.Now(), mustJSON(t, plan)); err != nil {
		t.Fatalf("save plan: %v", err)
	}

	run, err := svc.ApplyPlan(ctx, plan.PlanID, true)
	if err != nil {
		t.Fatalf("ApplyPlan refused a plan it built itself: %v (code %q)", err, app.CodeOf(err))
	}
	if run.Status != store.StatusApplied {
		t.Errorf("status = %q, want %q (detail: %s)", run.Status, store.StatusApplied, run.Detail)
	}
	for _, name := range []string{"ci.yml", "lean.yml"} {
		data, err := os.ReadFile(filepath.Join(run.Worktree, ".github", "workflows", name))
		if err != nil {
			t.Fatalf("read %s from worktree: %v", name, err)
		}
		if !strings.Contains(string(data), action+"@"+to) {
			t.Errorf("%s was not bumped to %s:\n%s", name, to, data)
		}
	}
}

// firstGroupTarget mirrors what PlanUpdates stores as a plan's target row.
func firstGroupTarget(plan planner.Plan) string {
	for _, g := range plan.Groups {
		if g.Target != "" {
			return g.Target
		}
	}
	return ""
}
