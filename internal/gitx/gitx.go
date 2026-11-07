// Package gitx shells out to the user's git.
//
// go-git's worktree support is incomplete and the specification calls for real
// `git worktree` (§3), so we drive the installed binary and inherit the user's
// hooks, credential helpers, and signing configuration.
package gitx

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Repo is a git working tree or bare repository on disk.
type Repo struct {
	Dir string
}

// New returns a Repo rooted at dir.
func New(dir string) Repo { return Repo{Dir: dir} }

// run executes git in the repository and returns trimmed stdout.
func (r Repo) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// IsRepo reports whether dir is inside a git repository.
func (r Repo) IsRepo(ctx context.Context) bool {
	_, err := r.run(ctx, "rev-parse", "--git-dir")
	return err == nil
}

// DefaultBranch resolves the branch to base work on: the remote's HEAD when it
// is known, otherwise the checked-out branch. Never silently falls back to a
// detached HEAD, which would produce branches based on nothing meaningful.
func (r Repo) DefaultBranch(ctx context.Context, remote string) (string, error) {
	if out, err := r.run(ctx, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil && out != "" {
		return strings.TrimPrefix(out, remote+"/"), nil
	}
	if out, err := r.run(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && out != "" && out != "HEAD" {
		return out, nil
	}
	return "", fmt.Errorf("%w: cannot determine the default branch", ErrNoDefaultBranch)
}

// ErrNoDefaultBranch is returned when no usable base branch exists (§8).
var ErrNoDefaultBranch = fmt.Errorf("no default branch")

// Fetch updates the remote refs.
func (r Repo) Fetch(ctx context.Context, remote string) error {
	_, err := r.run(ctx, "fetch", "--prune", remote)
	return err
}

// ResolveRef returns the commit id for a ref, or an error when it does not exist.
func (r Repo) ResolveRef(ctx context.Context, ref string) (string, error) {
	return r.run(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
}

// WorktreeAdd creates a worktree at path on a new branch based on base.
func (r Repo) WorktreeAdd(ctx context.Context, path, branch, base string) error {
	_, err := r.run(ctx, "worktree", "add", "-B", branch, path, base)
	return err
}

// WorktreeRemove deletes a worktree, discarding uncommitted changes.
func (r Repo) WorktreeRemove(ctx context.Context, path string) error {
	_, err := r.run(ctx, "worktree", "remove", "--force", path)
	return err
}

// WorktreeForBranch returns the path of the worktree that has branch checked
// out, or "" when none does. A day-scoped branch name means two runs on the same
// day would otherwise collide with the first run's worktree.
func (r Repo) WorktreeForBranch(ctx context.Context, branch string) (string, error) {
	out, err := r.run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	var current string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			current = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimPrefix(line, "branch ")
			if ref == "refs/heads/"+branch {
				return current, nil
			}
		}
	}
	return "", nil
}

// WorktreePrune removes administrative files for worktrees that are gone.
func (r Repo) WorktreePrune(ctx context.Context) error {
	_, err := r.run(ctx, "worktree", "prune")
	return err
}

// BranchDelete removes a local branch.
func (r Repo) BranchDelete(ctx context.Context, branch string) error {
	_, err := r.run(ctx, "branch", "-D", branch)
	return err
}

// BranchExists reports whether a local branch exists.
func (r Repo) BranchExists(ctx context.Context, branch string) bool {
	_, err := r.run(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// Status returns porcelain status output, empty when clean.
func (r Repo) Status(ctx context.Context) (string, error) {
	return r.run(ctx, "status", "--porcelain")
}

// CommitAll stages everything and commits, returning the new commit id. It never
// passes --no-verify: a failing hook must fail the run visibly (§11).
func (r Repo) CommitAll(ctx context.Context, message string) (string, error) {
	if _, err := r.run(ctx, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := r.run(ctx, "commit", "-m", message); err != nil {
		return "", err
	}
	return r.run(ctx, "rev-parse", "HEAD")
}

// CommitIfDirty commits only when something changed; it reports whether a commit
// was created so callers can avoid empty commits.
func (r Repo) CommitIfDirty(ctx context.Context, message string) (string, bool, error) {
	status, err := r.Status(ctx)
	if err != nil {
		return "", false, err
	}
	if status == "" {
		return "", false, nil
	}
	sha, err := r.CommitAll(ctx, message)
	if err != nil {
		return "", false, err
	}
	return sha, true, nil
}

// InWorktree returns a Repo bound to a worktree path.
func (r Repo) InWorktree(path string) Repo { return Repo{Dir: path} }

// Init creates a repository, used by tests and by `depmcp add` diagnostics.
func Init(ctx context.Context, dir string) (Repo, error) {
	r := Repo{Dir: dir}
	cmd := exec.CommandContext(ctx, "git", "init", "-b", "main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return r, fmt.Errorf("git init: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return r, nil
}
