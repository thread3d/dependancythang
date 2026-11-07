package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/thread3d/dependancythang/internal/dependabot"
	"github.com/thread3d/dependancythang/internal/forge"
	"github.com/thread3d/dependancythang/internal/planner"
	"github.com/thread3d/dependancythang/internal/store"
)

// Codes from the forge layer.
const (
	CodeNotVerified        = "NOT_VERIFIED"
	CodeForgeNotConfigured = "FORGE_NOT_CONFIGURED"
	CodePushFailed         = "PUSH_FAILED"
)

// remoteURLRE extracts owner/repo from a git remote URL in any of the usual
// shapes: https://, ssh://, and the scp-like git@host:owner/repo form.
var remoteURLRE = regexp.MustCompile(`(?:[:/])([^/:]+)/([^/]+?)(?:\.git)?/?$`)

// parseRemote returns the owner and repository from a git remote URL.
func parseRemote(raw string) (owner, repo string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	m := remoteURLRE.FindStringSubmatch(raw)
	if m == nil {
		return "", "", false
	}
	return m[1], strings.TrimSuffix(m[2], ".git"), true
}

// forgeFor resolves the configured forge for a repository.
//
// Tokens come from the environment for now. They are never written to state, and
// the sandbox never receives them (§11).
func (s *Service) forgeFor(repoPath, name, remoteName string) (forge.Forge, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "local":
		return forge.Local{Dir: filepath.Join(s.stateDir, "reports")}, nil
	}

	owner, repo, ok := parseRemote(s.remoteURL(repoPath, remoteName))
	if !ok {
		return nil, coded(CodeForgeNotConfigured, fmt.Errorf(
			"cannot determine owner/repository from the %q remote of %s", remoteName, repoPath))
	}
	base := strings.TrimSuffix(forgeBase(name), "/")

	switch strings.ToLower(name) {
	case "github":
		return &forge.GitHub{
			Remote: forge.Remote{Base: base, Token: os.Getenv("DEPMCP_GITHUB_TOKEN")},
			Owner:  owner, Repo: repo,
		}, nil
	case "gitlab":
		return &forge.GitLab{
			Remote:  forge.Remote{Base: base, Token: os.Getenv("DEPMCP_GITLAB_TOKEN")},
			Project: owner + "/" + repo,
		}, nil
	case "gitea", "forgejo":
		return &forge.Gitea{
			Remote: forge.Remote{Base: base, Token: os.Getenv("DEPMCP_GITEA_TOKEN")},
			Owner:  owner, Repo: repo,
		}, nil
	default:
		return nil, coded(CodeForgeNotConfigured, fmt.Errorf("unknown forge %q", name))
	}
}

// remoteURL reads a repository's remote URL without touching the network.
func (s *Service) remoteURL(repoPath, remoteName string) string {
	if remoteName == "" {
		remoteName = "origin"
	}
	cmd := exec.Command("git", "-C", repoPath, "remote", "get-url", remoteName)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// PublishRequest identifies a run to publish.
type PublishRequest struct {
	RunID string
}

// PublishResult reports what the forge did.
type PublishResult struct {
	Run    store.Run   `json:"run"`
	Forge  string      `json:"forge"`
	Pushed bool        `json:"pushed"`
	PR     forge.PRRef `json:"pr"`
}

// OpenPullRequest publishes a verified branch and opens a pull request.
//
// It refuses anything that has not passed verification: a proposal that was never
// built is exactly what this tool exists to prevent (§8 stage 7).
func (s *Service) OpenPullRequest(ctx context.Context, req PublishRequest) (PublishResult, error) {
	run, err := s.GetRun(ctx, req.RunID)
	if err != nil {
		return PublishResult{}, err
	}
	if run.Status != store.StatusVerified {
		return PublishResult{Run: run}, coded(CodeNotVerified, fmt.Errorf(
			"run %s is %s; only a verified branch may be proposed (verify it first)", run.RunID, run.Status))
	}
	plan, err := s.GetPlan(ctx, run.PlanID)
	if err != nil {
		return PublishResult{Run: run}, err
	}
	repo, repoPath, err := s.repoByID(run.RepoID)
	if err != nil {
		return PublishResult{Run: run}, err
	}

	forgeName := s.Cfg.ForgeFor(repo)
	remoteName := s.Cfg.RemoteFor(repo)
	fg, err := s.forgeFor(repoPath, forgeName, remoteName)
	if err != nil {
		return PublishResult{Run: run}, err
	}

	out := PublishResult{Run: run, Forge: fg.Name()}

	// A local forge stops here: the branch already exists on this machine.
	if fg.Name() != "local" {
		if err := s.pushBranch(ctx, repoPath, remoteName, run.Branch); err != nil {
			return out, coded(CodePushFailed, err)
		}
		out.Pushed = true
	}

	lines := make([]forge.ChangeLine, 0, plan.TotalChanges)
	for _, g := range plan.Groups {
		for _, c := range g.Changes {
			lines = append(lines, forge.ChangeLine{
				Dependency: c.Dependency, From: c.From, To: c.To,
				Class: string(c.Class), Advisories: c.Advisories,
			})
		}
	}
	pr := forge.PullRequest{
		Repo:   repoPath,
		Branch: run.Branch,
		Base:   s.Cfg.Fetch.Remote,
		Title:  forge.Title(lines, singleGroupName(plan)),
		Body: forge.RenderBody(forge.BodyInput{
			RunID:        run.RunID,
			Branch:       run.Branch,
			Changes:      lines,
			Verification: "passed in " + run.VerifySandbox + " (" + run.VerifySummary + ")",
			ReportedBy:   "depmcp",
		}),
	}
	if strings.TrimSpace(pr.Base) == "" {
		pr.Base = "main"
	}

	ref, err := fg.OpenPR(ctx, repoPath, pr)
	if err != nil {
		return out, err
	}
	out.PR = ref

	run.Status = store.StatusPROpened
	run.Detail = "published via " + fg.Name()
	if err := s.Store.SaveRun(ctx, run); err != nil {
		return out, err
	}
	out.Run = run
	return out, nil
}

// singleGroupName returns the group name when a plan has exactly one, which is
// what makes a grouped pull request title readable.
func singleGroupName(p planner.Plan) string {
	if len(p.Groups) == 1 {
		return p.Groups[0].Name
	}
	return ""
}

// forgeBase resolves the API root for a forge. Self-hosted instances override it
// with DEPMCP_FORGE_BASE.
func forgeBase(name string) string {
	if override := strings.TrimSpace(os.Getenv("DEPMCP_FORGE_BASE")); override != "" {
		return override
	}
	switch strings.ToLower(name) {
	case "github":
		return "https://api.github.com"
	case "gitlab":
		return "https://gitlab.com/api/v4"
	default:
		return ""
	}
}

// pushBranch publishes a branch with the user's own git credentials.
func (s *Service) pushBranch(ctx context.Context, repoPath, remote, branch string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "push", "--set-upstream", remote, branch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push %s %s: %w: %s", remote, branch, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ImportDependabot reads a repository's dependabot.yml and reports the mapping.
// It changes no configuration: importing is a review step, not a silent rewrite.
func (s *Service) ImportDependabot(_ context.Context, repoRef string) (dependabot.Result, error) {
	_, repoPath, err := s.repoByID(repoRef)
	if err != nil {
		return dependabot.Result{}, err
	}
	return dependabot.ImportPathForRepo(repoPath)
}
