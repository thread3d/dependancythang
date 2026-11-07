package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thread3d/dependancythang/internal/app"
	"github.com/thread3d/dependancythang/internal/config"
)

// setup points depmcp at a temp config and state directory and returns both
// paths, so a test can inspect or destroy them.
func setup(t *testing.T) (configPath, stateDir string) {
	t.Helper()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.yaml")
	stateDir = filepath.Join(dir, "state")
	if err := os.WriteFile(configPath, []byte("version: 1\nrepositories: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(config.EnvConfig, configPath)
	t.Setenv(config.EnvStateDir, stateDir)
	return configPath, stateDir
}

// fakeGitRepo creates a directory that looks like a git working tree without
// needing the git binary.
func fakeGitRepo(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("create fake repo: %v", err)
	}
	return path
}

// TestDeletingStateLosesNoConfiguration is M0's exit criterion: wiping the
// state directory must cost history, never intent (§5).
func TestDeletingStateLosesNoConfiguration(t *testing.T) {
	ctx := context.Background()
	configPath, stateDir := setup(t)
	repo := fakeGitRepo(t, "project")

	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	view, err := svc.AddRepository(ctx, app.AddRepositoryRequest{Path: repo, Forge: "github"})
	if err != nil {
		t.Fatalf("AddRepository: %v", err)
	}
	if view.Path != repo {
		t.Errorf("view path = %q, want %q", view.Path, repo)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The configuration file, not the database, must hold the intent.
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(written), repo) {
		t.Fatalf("repository missing from the config file:\n%s", written)
	}

	// Now lose every byte of derived state.
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatalf("remove state: %v", err)
	}

	svc2, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("reopen after wiping state: %v", err)
	}
	defer svc2.Close()

	repos, err := svc2.ListRepositories(ctx)
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("repos = %d after wiping state, want 1 (configuration is authoritative)", len(repos))
	}
	if repos[0].Path != repo || repos[0].Forge != "github" {
		t.Errorf("repo = %+v, want path %q forge github", repos[0], repo)
	}

	// Derived state must have been rebuilt from configuration.
	stats, err := svc2.Store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Repos != 1 {
		t.Errorf("state rows = %d, want 1 rebuilt by reconcile", stats.Repos)
	}
}

func TestAddRepositoryRejectsNonGitDirectory(t *testing.T) {
	ctx := context.Background()
	setup(t)
	plain := t.TempDir()

	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer svc.Close()

	_, err = svc.AddRepository(ctx, app.AddRepositoryRequest{Path: plain})
	if err == nil {
		t.Fatal("expected a rejection for a directory that is not a git repository")
	}
	if got := app.CodeOf(err); got != app.CodeNotAGitRepo {
		t.Errorf("code = %q, want %q", got, app.CodeNotAGitRepo)
	}
}

func TestReadOnlySwitchBlocksWrites(t *testing.T) {
	ctx := context.Background()
	configPath, _ := setup(t)
	if err := os.WriteFile(configPath, []byte("version: 1\nserver:\n  read_only: true\nrepositories: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	repo := fakeGitRepo(t, "project")

	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer svc.Close()

	_, err = svc.AddRepository(ctx, app.AddRepositoryRequest{Path: repo})
	if err == nil {
		t.Fatal("expected write refusal under read_only")
	}
	if got := app.CodeOf(err); got != app.CodeConfigReadOnly {
		t.Errorf("code = %q, want %q", got, app.CodeConfigReadOnly)
	}
}

func TestRemoveRepositoryIsIdempotent(t *testing.T) {
	ctx := context.Background()
	_, _ = setup(t)
	repo := fakeGitRepo(t, "project")

	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer svc.Close()

	if _, err := svc.AddRepository(ctx, app.AddRepositoryRequest{Path: repo}); err != nil {
		t.Fatalf("AddRepository: %v", err)
	}
	removed, err := svc.RemoveRepository(ctx, repo)
	if err != nil {
		t.Fatalf("RemoveRepository: %v", err)
	}
	if !removed {
		t.Error("first removal reported no change")
	}
	again, err := svc.RemoveRepository(ctx, repo)
	if err != nil {
		t.Fatalf("second RemoveRepository: %v", err)
	}
	if again {
		t.Error("removing an untracked repository reported a change")
	}
}

func TestLockReturnsRepoBusyCode(t *testing.T) {
	ctx := context.Background()
	_, _ = setup(t)
	repo := fakeGitRepo(t, "project")

	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer svc.Close()

	first, err := svc.Lock(config.RepoID(repo))
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer first.Release()

	_, err = svc.Lock(config.RepoID(repo))
	if err == nil {
		t.Fatal("second lock succeeded")
	}
	if got := app.CodeOf(err); got != app.CodeRepoBusy {
		t.Errorf("code = %q, want %q", got, app.CodeRepoBusy)
	}
}

func TestAuditRecordsToolCalls(t *testing.T) {
	ctx := context.Background()
	_, stateDir := setup(t)

	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer svc.Close()

	if err := svc.RecordTool(ctx, "list_repositories", map[string]any{"count": 0}, nil, ""); err != nil {
		t.Fatalf("RecordTool: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(stateDir, "audit"))
	if err != nil {
		t.Fatalf("read audit dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit files = %d, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "audit", entries[0].Name()))
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	if !strings.Contains(string(data), "list_repositories") {
		t.Errorf("audit line missing the tool name: %s", data)
	}
}

func TestDoctorReportsChecks(t *testing.T) {
	ctx := context.Background()
	_, _ = setup(t)
	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer svc.Close()

	checks := svc.Doctor(ctx)
	if len(checks) == 0 {
		t.Fatal("doctor returned no checks")
	}
	names := map[string]bool{}
	for _, c := range checks {
		names[c.Name] = true
		if c.Status != "ok" && c.Status != "warn" && c.Status != "fail" {
			t.Errorf("check %s has invalid status %q", c.Name, c.Status)
		}
	}
	for _, want := range []string{"config", "state_dir", "store", "git", "sandbox"} {
		if !names[want] {
			t.Errorf("doctor is missing the %q check", want)
		}
	}
}
