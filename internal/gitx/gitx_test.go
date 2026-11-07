package gitx_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thread3d/dependancythang/internal/gitx"
)

// newRepo creates a repository with one commit and a configured identity, so
// tests never depend on the host's git config.
func newRepo(t *testing.T) (gitx.Repo, string) {
	t.Helper()
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-b", "main")
	gitCmd(t, dir, "config", "user.email", "test@example.com")
	gitCmd(t, dir, "config", "user.name", "Test")

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	return gitx.New(dir), dir
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestDefaultBranch(t *testing.T) {
	repo, _ := newRepo(t)
	got, err := repo.DefaultBranch(context.Background(), "origin")
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if got != "main" {
		t.Errorf("default branch = %q, want main", got)
	}
}

func TestWorktreeLifecycle(t *testing.T) {
	ctx := context.Background()
	repo, dir := newRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")
	branch := "depmcp/gomod-test"

	if exists := repo.BranchExists(ctx, branch); exists {
		t.Fatal("branch already exists")
	}
	if err := repo.WorktreeAdd(ctx, wtPath, branch, "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "go.mod")); err != nil {
		t.Errorf("worktree does not contain the base commit's files: %v", err)
	}
	if !repo.BranchExists(ctx, branch) {
		t.Error("branch was not created")
	}

	// The user's tree must be untouched by work in the worktree.
	wt := gitx.New(wtPath)
	if err := os.WriteFile(filepath.Join(wtPath, "extra.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write in worktree: %v", err)
	}
	status, err := repo.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status != "" {
		t.Errorf("main worktree became dirty: %q", status)
	}

	sha, committed, err := wt.CommitIfDirty(ctx, "test commit")
	if err != nil {
		t.Fatalf("CommitIfDirty: %v", err)
	}
	if !committed || sha == "" {
		t.Fatalf("expected a commit, got committed=%v sha=%q", committed, sha)
	}

	// A second call with nothing to do must not create an empty commit.
	if _, committed, err := wt.CommitIfDirty(ctx, "again"); err != nil {
		t.Fatalf("CommitIfDirty: %v", err)
	} else if committed {
		t.Error("created a commit with no changes")
	}

	if err := repo.WorktreeRemove(ctx, wtPath); err != nil {
		t.Fatalf("WorktreeRemove: %v", err)
	}
	if err := repo.BranchDelete(ctx, branch); err != nil {
		t.Fatalf("BranchDelete: %v", err)
	}
	if repo.BranchExists(ctx, branch) {
		t.Error("branch still exists after delete")
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree directory still present: %v", err)
	}
	_ = dir
}

func TestIsRepo(t *testing.T) {
	repo, _ := newRepo(t)
	if !repo.IsRepo(context.Background()) {
		t.Error("IsRepo = false for a real repository")
	}
	plain := gitx.New(t.TempDir())
	if plain.IsRepo(context.Background()) {
		t.Error("IsRepo = true for a plain directory")
	}
}
