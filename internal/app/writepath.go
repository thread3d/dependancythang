package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thread3d/dependancythang/internal/config"
	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/gitx"
	"github.com/thread3d/dependancythang/internal/planner"
	"github.com/thread3d/dependancythang/internal/store"
)

// Codes from the write path (§7).
const (
	CodePlanStale            = "PLAN_STALE"
	CodeVersionYanked        = "VERSION_YANKED"
	CodeCooldownActive       = "COOLDOWN_ACTIVE"
	CodeConfirmationRequired = "CONFIRMATION_REQUIRED"
	CodeRunNotFound          = "RUN_NOT_FOUND"
	CodeRunTerminal          = "RUN_TERMINAL"
	CodeGitFailed            = "GIT_FAILED"
	CodeNothingToApply       = "NOTHING_TO_APPLY"
)

// newRunID returns a short, sortable-ish identifier for a run.
func newRunID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// branchTarget derives a branch-safe name from a target key such as "gomod:/".
func branchTarget(target, ecosystemName string) string {
	eco := ecosystemName
	if eco == "" {
		eco = "deps"
	}
	if target != "" {
		if i := strings.Index(target, ":"); i >= 0 {
			eco = target[:i]
		}
	}
	eco = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '-'
		}
	}, eco)
	return strings.Trim(eco, "-")
}

// RevalidatePlan re-checks a stored plan against the registry before anything is
// written (§8). A plan is a hypothesis from the moment it was built; silently
// retargeting a reviewed plan is worse than refusing it.
func (s *Service) RevalidatePlan(ctx context.Context, plan planner.Plan) error {
	if problems := planner.Validate(plan, time.Now()); len(problems) > 0 {
		return coded(CodePlanStale, fmt.Errorf("plan is not self-consistent: %v", problems))
	}
	cooldown := s.cooldownDays()
	regs := map[string]ecosystem.Registry{}

	for _, g := range plan.Groups {
		ecoName := g.Ecosystem
		if ecoName == "" {
			ecoName = ecosystemOfTarget(g.Target)
		}
		reg, ok := regs[ecoName]
		if !ok {
			var rerr error
			reg, rerr = s.registryForEcosystem(ecoName)
			if rerr != nil {
				return rerr
			}
			regs[ecoName] = reg
		}
		for _, c := range g.Changes {
			versions, verr := reg.Versions(ctx, c.Dependency)
			if verr != nil {
				return coded(CodeRegistryUnavailable,
					fmt.Errorf("re-validating %s: %w", c.Dependency, verr))
			}
			found := false
			var meta ecosystem.Version
			for _, v := range versions {
				if v.Raw == c.To {
					found, meta = true, v
					break
				}
			}
			if !found {
				return coded(CodePlanStale, fmt.Errorf(
					"%s@%s no longer exists in the registry; re-plan before applying", c.Dependency, c.To))
			}
			if meta.Yanked || meta.Retracted {
				return coded(CodeVersionYanked, fmt.Errorf(
					"%s@%s is yanked or retracted", c.Dependency, c.To))
			}
			days := cooldownFor(cooldown, c.Class)
			if days > 0 && meta.PublishedAt != "" {
				if published, perr := time.Parse(time.RFC3339, meta.PublishedAt); perr == nil {
					if time.Now().Before(published.Add(time.Duration(days) * 24 * time.Hour)) {
						return coded(CodeCooldownActive, fmt.Errorf(
							"%s@%s is still inside its %d-day cooldown", c.Dependency, c.To, days))
					}
				}
			}
		}
	}
	return nil
}

func cooldownFor(c CooldownDays, class ecosystem.UpdateClass) int {
	switch class {
	case ecosystem.ClassPatch:
		return c.PatchDays
	case ecosystem.ClassMinor:
		return c.MinorDays
	case ecosystem.ClassMajor:
		return c.MajorDays
	default:
		return 0
	}
}

// CooldownDays mirrors the configured cooldown so this file does not need the
// policy package just for three integers.
type CooldownDays struct {
	PatchDays int
	MinorDays int
	MajorDays int
}

func (s *Service) cooldownDays() CooldownDays {
	return CooldownDays{
		PatchDays: s.Cfg.Defaults.Cooldown.PatchDays,
		MinorDays: s.Cfg.Defaults.Cooldown.MinorDays,
		MajorDays: s.Cfg.Defaults.Cooldown.MajorDays,
	}
}

// ApplyPlan creates a branch and worktree, applies the plan there, and commits.
// The user's working tree is never touched.
func (s *Service) ApplyPlan(ctx context.Context, planID string, confirm bool) (store.Run, error) {
	if s.ReadOnly() {
		return store.Run{}, coded(CodeConfigReadOnly,
			errors.New("server.read_only is true; refusing to write to a repository"))
	}
	if !confirm {
		return store.Run{}, coded(CodeConfirmationRequired,
			errors.New("apply_plan rewrites a worktree and commits: pass confirm=true or answer the confirmation prompt"))
	}

	plan, err := s.GetPlan(ctx, planID)
	if err != nil {
		return store.Run{}, err
	}
	if plan.TotalChanges == 0 {
		return store.Run{}, coded(CodeNothingToApply, errors.New("plan contains no changes to apply"))
	}
	repo, repoPath, err := s.repoByID(plan.RepoID)
	if err != nil {
		return store.Run{}, err
	}
	_ = repo

	// TOCTOU: re-validate before writing a single byte.
	if err := s.RevalidatePlan(ctx, plan); err != nil {
		return store.Run{}, err
	}

	repoID := plan.RepoID
	lock, err := s.Lock(repoID)
	if err != nil {
		return store.Run{}, err
	}
	defer lock.Release()

	now := time.Now()
	run := store.Run{
		RunID:     newRunID(now),
		RepoID:    repoID,
		PlanID:    plan.PlanID,
		Target:    firstTarget(plan),
		Status:    store.StatusApplying,
		CreatedAt: now.Unix(),
	}
	run.Branch = fmt.Sprintf("%s/%s-%s", branchPrefix(s.Cfg.Defaults.BranchPrefix),
		branchTarget(run.Target, plan.Ecosystem), now.UTC().Format("20060102"))
	run.Worktree = filepath.Join(s.stateDir, "worktrees", repoID, run.RunID)

	if err := s.Store.SaveRun(ctx, run); err != nil {
		return store.Run{}, err
	}

	if err := s.applyIntoWorktree(ctx, &run, plan, repoPath, s.Cfg.RemoteFor(repo)); err != nil {
		_ = s.Store.SetRunStatus(context.Background(), run.RunID, store.StatusFailed, err.Error())
		run.Status = store.StatusFailed
		run.Detail = err.Error()
		return run, err
	}
	return run, nil
}

// applyIntoWorktree performs the git and toolchain work for one run, cleaning up
// on failure so a crashed apply never leaves a half-made branch behind.
func (s *Service) applyIntoWorktree(ctx context.Context, run *store.Run, plan planner.Plan, repoPath, remote string) error {
	base := gitx.New(repoPath)
	if remote == "" {
		remote = "origin"
	}
	if s.Cfg.Fetch.Enabled {
		// A fetch failure is not fatal: the run records that it based on a local
		// ref, which the PR body and the plan then admit (§8).
		_ = base.Fetch(ctx, remote)
	}
	branch, err := base.DefaultBranch(ctx, remote)
	if err != nil {
		return coded(CodeGitFailed, err)
	}
	baseRef := branch
	if _, err := base.ResolveRef(ctx, remote+"/"+branch); err == nil {
		baseRef = remote + "/" + branch
	}

	// A day-scoped branch name can already be checked out by an earlier run (a
	// retry, or a schedule firing twice). Supersede it rather than failing: §8
	// says a newer run replaces the older branch for the same package.
	if stale, werr := base.WorktreeForBranch(ctx, run.Branch); werr == nil && stale != "" {
		_ = base.WorktreeRemove(ctx, stale)
		_ = base.WorktreePrune(ctx)
	}

	if err := base.WorktreeAdd(ctx, run.Worktree, run.Branch, baseRef); err != nil {
		return coded(CodeGitFailed, err)
	}
	cleanup := func(reason string) {
		_ = base.WorktreeRemove(context.Background(), run.Worktree)
		_ = base.BranchDelete(context.Background(), run.Branch)
		_ = base.WorktreePrune(context.Background())
		run.Detail = reason
	}

	// A plan can span ecosystems (a Dockerfile and a workflow in one repository),
	// so each group is applied by its own applier rather than one for the whole
	// plan.
	result, err := s.applyGroups(ctx, run.Worktree, plan)
	if err != nil {
		cleanup(err.Error())
		return fmt.Errorf("apply to %s: %w", run.Branch, err)
	}

	wt := gitx.New(run.Worktree)
	sha, committed, err := wt.CommitIfDirty(ctx, commitMessage(plan, result))
	if err != nil {
		cleanup(err.Error())
		return coded(CodeGitFailed, err)
	}
	if !committed {
		cleanup("no file changes were produced")
		return coded(CodeNothingToApply,
			errors.New("the toolchain produced no changes; nothing to commit"))
	}

	run.Commit = sha
	run.Status = store.StatusApplied
	run.Detail = fmt.Sprintf("%d change(s) applied", len(result.Changed))
	if result.Unsplittable {
		run.Detail += "; resolved graph moved beyond the requested changes"
	}
	if err := s.Store.SaveRun(ctx, *run); err != nil {
		return err
	}
	return nil
}

// applierFor returns the applier for an ecosystem, defaulting to that
// ecosystem's real implementation.
func (s *Service) applierFor(name string) (func(context.Context, string, []ecosystem.Change) (ecosystem.ApplyResult, error), error) {
	if s.Apply != nil {
		return s.Apply, nil
	}
	eco, err := ecosystemFor(name)
	if err != nil {
		return nil, err
	}
	return eco.Apply, nil
}

// branchPrefix resolves the configured branch prefix.
func branchPrefix(configured string) string {
	if strings.TrimSpace(configured) == "" {
		return "depmcp"
	}
	return strings.Trim(configured, "/")
}

func firstTarget(p planner.Plan) string {
	for _, g := range p.Groups {
		if g.Target != "" {
			return g.Target
		}
	}
	return ""
}

// applyGroups applies every group with the ecosystem that owns it and merges the
// results into one report.
func (s *Service) applyGroups(ctx context.Context, dir string, plan planner.Plan) (ecosystem.ApplyResult, error) {
	merged := ecosystem.ApplyResult{Observed: map[string][2]string{}}
	for _, g := range plan.Groups {
		if len(g.Changes) == 0 {
			continue
		}
		name := g.Ecosystem
		if name == "" {
			name = ecosystemOfTarget(g.Target)
		}
		apply, err := s.applierFor(name)
		if err != nil {
			return merged, err
		}
		res, err := apply(ctx, dir, changesOfGroup(g))
		if err != nil {
			return merged, err
		}
		for k, v := range res.Observed {
			merged.Observed[k] = v
		}
		merged.Changed = append(merged.Changed, res.Changed...)
		merged.Commands = append(merged.Commands, res.Commands...)
		if res.Unsplittable {
			merged.Unsplittable = true
		}
	}
	sort.Strings(merged.Changed)
	return merged, nil
}

// changesOfGroup renders one group as applier input.
func changesOfGroup(g planner.Group) []ecosystem.Change {
	out := make([]ecosystem.Change, 0, len(g.Changes))
	for _, c := range g.Changes {
		out = append(out, ecosystem.Change{Name: c.Dependency, From: c.From, To: c.To, Digest: c.Digest})
	}
	return out
}

// changesOf flattens a plan into the applier's input.
func changesOf(p planner.Plan) []ecosystem.Change {
	var out []ecosystem.Change
	for _, g := range p.Groups {
		for _, c := range g.Changes {
			out = append(out, ecosystem.Change{Name: c.Dependency, From: c.From, To: c.To, Digest: c.Digest})
		}
	}
	return out
}

// commitMessage renders a conventional commit listing every change and every
// advisory id, per §8.
func commitMessage(p planner.Plan, res ecosystem.ApplyResult) string {
	var b strings.Builder
	switch {
	case p.TotalChanges == 1:
		c := p.Groups[0].Changes[0]
		fmt.Fprintf(&b, "build(deps): bump %s from %s to %s", c.Dependency, c.From, c.To)
	case len(p.Groups) == 1:
		fmt.Fprintf(&b, "build(deps): bump the %s group with %d updates",
			p.Groups[0].Name, p.TotalChanges)
	default:
		fmt.Fprintf(&b, "build(deps): apply %d dependency updates", p.TotalChanges)
	}
	b.WriteString("\n")
	for _, g := range p.Groups {
		for _, c := range g.Changes {
			fmt.Fprintf(&b, "\n- %s %s -> %s (%s)", c.Dependency, c.From, c.To, c.Class)
			if len(c.Advisories) > 0 {
				fmt.Fprintf(&b, " [%s]", strings.Join(c.Advisories, ", "))
			}
		}
	}
	if res.Unsplittable {
		b.WriteString("\n\nThe resolved dependency graph moved beyond the requested changes.")
	}
	b.WriteString("\n\nCo-authored-by: depmcp <depmcp@localhost>\n")
	return b.String()
}

// RevertRun deletes a run's branch and worktree (§12). A merged or closed run is
// terminal: its content already landed, so only a message is returned.
func (s *Service) RevertRun(ctx context.Context, runID string) (store.Run, error) {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return store.Run{}, err
	}
	if store.Terminal(run.Status) || !store.Revertable(run.Status) {
		return run, coded(CodeRunTerminal, fmt.Errorf(
			"run %s is %s: only its worktree would be cleaned, never its branch", runID, run.Status))
	}
	_, repoPath, err := s.repoByID(run.RepoID)
	if err != nil {
		return store.Run{}, err
	}
	lock, err := s.Lock(run.RepoID)
	if err != nil {
		return store.Run{}, err
	}
	defer lock.Release()

	git := gitx.New(repoPath)
	// Both steps are best effort: a half-reverted run is worse than one that
	// reports what it could not clean.
	var problems []string
	if err := git.WorktreeRemove(ctx, run.Worktree); err != nil {
		problems = append(problems, "worktree: "+err.Error())
	}
	if err := git.BranchDelete(ctx, run.Branch); err != nil {
		problems = append(problems, "branch: "+err.Error())
	}
	_ = git.WorktreePrune(ctx)

	detail := "reverted"
	if len(problems) > 0 {
		detail = "reverted with problems: " + strings.Join(problems, "; ")
	}
	if err := s.Store.SetRunStatus(ctx, run.RunID, store.StatusReverted, detail); err != nil {
		return run, err
	}
	run.Status = store.StatusReverted
	run.Detail = detail
	return run, nil
}

// GetRun loads one run.
func (s *Service) GetRun(ctx context.Context, runID string) (store.Run, error) {
	run, err := s.Store.LoadRun(ctx, runID)
	if errors.Is(err, store.ErrNoRun) {
		return run, coded(CodeRunNotFound, fmt.Errorf("no run with id %q", runID))
	}
	return run, err
}

// ListRuns returns recent runs, optionally for one repository.
func (s *Service) ListRuns(ctx context.Context, repoRef string, limit int) ([]store.Run, error) {
	repoID := ""
	if strings.TrimSpace(repoRef) != "" {
		_, path, err := s.repoByID(repoRef)
		if err != nil {
			return nil, err
		}
		repoID = config.RepoID(path)
	}
	return s.Store.ListRuns(ctx, repoID, limit)
}
