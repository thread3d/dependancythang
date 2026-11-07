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
	"github.com/thread3d/dependancythang/internal/sandbox"
	"github.com/thread3d/dependancythang/internal/store"
)

// verifiedRun prepares a run that has passed verification, with the local forge.
func verifiedRun(t *testing.T) (*app.Service, store.Run, string) {
	t.Helper()
	svc, run, repoPath := applyOneRun(t)
	svc.Sandbox = func(app.SandboxConfig) (sandbox.Sandbox, error) { return &fakeSandbox{}, nil }
	if _, _, err := svc.VerifyRun(context.Background(), run.RunID); err != nil {
		t.Fatalf("VerifyRun: %v", err)
	}
	return svc, run, repoPath
}

// TestOpenPullRequestRefusesUnverified is the M6 safety gate.
func TestOpenPullRequestRefusesUnverified(t *testing.T) {
	svc, run, _ := applyOneRun(t) // applied but never verified
	res, err := svc.OpenPullRequest(context.Background(), app.PublishRequest{RunID: run.RunID})
	if err == nil {
		t.Fatal("an unverified run was published")
	}
	if got := app.CodeOf(err); got != app.CodeNotVerified {
		t.Errorf("code = %q, want %q (%v)", got, app.CodeNotVerified, err)
	}
	if res.PR.Report != "" {
		t.Error("a report was written for a refused run")
	}
}

// TestOpenPullRequestLocalForgeWritesReport covers the default forge: it stops at
// a branch and writes a report instead of contacting a remote.
func TestOpenPullRequestLocalForgeWritesReport(t *testing.T) {
	svc, run, _ := verifiedRun(t)

	res, err := svc.OpenPullRequest(context.Background(), app.PublishRequest{RunID: run.RunID})
	if err != nil {
		t.Fatalf("OpenPullRequest: %v", err)
	}
	if res.Forge != "local" {
		t.Errorf("forge = %q, want local by default", res.Forge)
	}
	if res.Pushed {
		t.Error("the local forge pushed to a remote")
	}
	if res.PR.Report == "" {
		t.Fatal("no report written")
	}
	body, err := os.ReadFile(res.PR.Report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	text := string(body)
	for _, want := range []string{"## Dependency updates", "depmcp revert " + run.RunID, "Verification"} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
	if res.Run.Status != store.StatusPROpened {
		t.Errorf("run status = %q, want %q", res.Run.Status, store.StatusPROpened)
	}
}

// TestImportDependabotReadsTheRepository covers the import path against a real
// file on disk.
func TestImportDependabotReadsTheRepository(t *testing.T) {
	svc, _, repoPath := verifiedRun(t)
	dir := filepath.Join(repoPath, ".github")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	doc := `version: 2
updates:
  - package-ecosystem: "gomod"
    directory: "/"
    schedule:
      interval: "weekly"
      timezone: "UTC"
    labels: ["deps"]
`
	if err := os.WriteFile(filepath.Join(dir, "dependabot.yml"), []byte(doc), 0o644); err != nil {
		t.Fatalf("write dependabot.yml: %v", err)
	}

	res, err := svc.ImportDependabot(context.Background(), repoPath)
	if err != nil {
		t.Fatalf("ImportDependabot: %v", err)
	}
	if len(res.Imported) != 1 || res.Imported[0].Ecosystem != "gomod" {
		t.Fatalf("imported = %+v", res.Imported)
	}
	if len(res.Unmapped) == 0 {
		t.Error("labels should have been reported as unmapped")
	}
}

func TestImportDependabotMissingFile(t *testing.T) {
	svc, _, repoPath := verifiedRun(t)
	if _, err := svc.ImportDependabot(context.Background(), repoPath); err == nil {
		t.Error("expected an error for a repository without dependabot.yml")
	}
}

var _ = time.Now
var _ = ecosystem.ClassNone
