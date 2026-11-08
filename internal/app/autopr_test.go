package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thread3d/dependancythang/internal/app"
	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/gitx"
	"github.com/thread3d/dependancythang/internal/sandbox"
)

// TestRunOnceHonoursAutoPR is DEP-9DE3A533: the schedule used to stop at a verified
// branch even when the configuration asked it to publish.
func TestRunOnceHonoursAutoPR(t *testing.T) {
	ctx := context.Background()
	const module, from, to = "example.com/dep", "v1.0.0", "v1.1.0"
	repoPath := gitRepo(t, module, from)
	svc := newService(t, repoPath)
	svc.Registry = &fakeRegistry{versions: map[string][]ecosystem.Version{
		module: {{Raw: to, PublishedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339)}},
	}}
	svc.Apply = fakeApplier(new([]ecosystem.Change))
	svc.Sandbox = func(app.SandboxConfig) (sandbox.Sandbox, error) { return &fakeSandbox{}, nil }

	if !gitx.New(repoPath).IsRepo(ctx) {
		t.Fatal("the fixture is not a git repository")
	}

	// auto_pr off: stops at the branch.
	svc.Cfg.Defaults.AutoPR = false
	out, err := svc.RunOnce(ctx, repoPath, app.RunOptions{})
	if err != nil {
		t.Fatalf("RunOnce (auto_pr off): %v", err)
	}
	if !strings.Contains(out.Summary, "no pull request") {
		t.Errorf("summary = %q, want it to say no PR was opened", out.Summary)
	}

	// auto_pr on: publishes through the local forge, which writes a report.
	svc.Cfg.Defaults.AutoPR = true
	svc.Cfg.Repositories[0].Forge = "local"
	out, err = svc.RunOnce(ctx, repoPath, app.RunOptions{})
	if err != nil {
		t.Fatalf("RunOnce (auto_pr on): %v", err)
	}
	if !out.Verified {
		t.Fatalf("run was not verified: %s", out.Summary)
	}
	if !strings.Contains(out.Summary, "report written") && !strings.Contains(out.Summary, "published") {
		t.Errorf("summary = %q, want evidence that publishing happened", out.Summary)
	}
	reports, err := os.ReadDir(filepath.Join(svc.StateDir(), "reports"))
	if err != nil || len(reports) == 0 {
		t.Errorf("no report was written with auto_pr on (err=%v)", err)
	}
}
