package forge

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// GitHub opens pull requests through the REST API.
type GitHub struct {
	Remote
	// Owner and Repo default to those parsed from a repository's remote URL.
	Owner string
	Repo  string
}

// Name implements Forge.
func (g *GitHub) Name() string { return "github" }

func (g *GitHub) headers() map[string]string {
	h := map[string]string{
		"X-GitHub-Api-Version": "2022-11-28",
	}
	if g.Token != "" {
		h["Authorization"] = "Bearer " + g.Token
	}
	return h
}

// repoPath renders owner/repo.
func (g *GitHub) repoPath() (string, error) {
	if g.Owner == "" || g.Repo == "" {
		return "", fmt.Errorf("github forge needs an owner and repository")
	}
	return g.Owner + "/" + g.Repo, nil
}

// Push publishes the branch.
func (g *GitHub) Push(ctx context.Context, repoPath, branch string) error {
	// Pushing is git's job; the forge only opens the request. This keeps
	// credentials in git's own helpers rather than in a token we hold.
	_ = ctx
	_ = repoPath
	_ = branch
	return nil
}

// OpenPR creates a pull request, or returns the existing one when a managed PR
// for the same head already exists.
func (g *GitHub) OpenPR(ctx context.Context, _ string, pr PullRequest) (PRRef, error) {
	repo, err := g.repoPath()
	if err != nil {
		return PRRef{}, err
	}
	existing, err := g.ListManagedPRs(ctx, repo)
	if err == nil {
		for _, ref := range existing {
			if ref.Head == pr.Branch {
				return ref, nil // idempotent: never open a duplicate
			}
		}
	}
	payload := map[string]any{
		"title": pr.Title,
		"head":  pr.Branch,
		"base":  pr.Base,
		"body":  pr.Body,
		"draft": pr.Draft,
	}
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	u := fmt.Sprintf("%s/repos/%s/pulls", strings.TrimSuffix(g.Base, "/"), repo)
	if _, err := g.do(ctx, "POST", u, payload, &out, g.headers()); err != nil {
		return PRRef{}, err
	}
	if len(pr.Labels) > 0 {
		labelURL := fmt.Sprintf("%s/repos/%s/issues/%d/labels", strings.TrimSuffix(g.Base, "/"), repo, out.Number)
		_, _ = g.do(ctx, "POST", labelURL, map[string]any{"labels": pr.Labels}, nil, g.headers())
	}
	return PRRef{Forge: "github", Number: out.Number, URL: out.HTMLURL, Head: pr.Branch}, nil
}

// ListManagedPRs returns open pull requests carrying Marker.
func (g *GitHub) ListManagedPRs(ctx context.Context, _ string) ([]PRRef, error) {
	repo, err := g.repoPath()
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/repos/%s/pulls?state=open&per_page=100", strings.TrimSuffix(g.Base, "/"), repo)
	var out []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if _, err := g.do(ctx, "GET", u, nil, &out, g.headers()); err != nil {
		return nil, err
	}
	var refs []PRRef
	for _, pr := range out {
		if !strings.Contains(pr.Body, Marker) {
			continue
		}
		refs = append(refs, PRRef{Forge: "github", Number: pr.Number, URL: pr.HTMLURL, Head: pr.Head.Ref})
	}
	return refs, nil
}

// GitLab opens merge requests.
type GitLab struct {
	Remote
	Project string // "group/project" or a numeric id
}

// Name implements Forge.
func (g *GitLab) Name() string { return "gitlab" }

func (g *GitLab) headers() map[string]string {
	if g.Token == "" {
		return nil
	}
	return map[string]string{"PRIVATE-TOKEN": g.Token}
}

// Push is git's job; see GitHub.Push.
func (g *GitLab) Push(context.Context, string, string) error { return nil }

// projectPath escapes the project identifier for the API path.
func (g *GitLab) projectPath() (string, error) {
	if g.Project == "" {
		return "", fmt.Errorf("gitlab forge needs a project")
	}
	return url.PathEscape(g.Project), nil
}

// OpenPR creates a merge request.
func (g *GitLab) OpenPR(ctx context.Context, _ string, pr PullRequest) (PRRef, error) {
	project, err := g.projectPath()
	if err != nil {
		return PRRef{}, err
	}
	payload := map[string]any{
		"source_branch": pr.Branch,
		"target_branch": pr.Base,
		"title":         pr.Title,
		"description":   pr.Body,
	}
	var out struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
		State  string `json:"state"`
	}
	u := fmt.Sprintf("%s/projects/%s/merge_requests", strings.TrimSuffix(g.Base, "/"), project)
	if _, err := g.do(ctx, "POST", u, payload, &out, g.headers()); err != nil {
		return PRRef{}, err
	}
	return PRRef{Forge: "gitlab", Number: out.IID, URL: out.WebURL, Head: pr.Branch}, nil
}

// ListManagedPRs returns open merge requests carrying Marker.
func (g *GitLab) ListManagedPRs(ctx context.Context, _ string) ([]PRRef, error) {
	project, err := g.projectPath()
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/projects/%s/merge_requests?state=opened&per_page=100",
		strings.TrimSuffix(g.Base, "/"), project)
	var out []struct {
		IID          int    `json:"iid"`
		WebURL       string `json:"web_url"`
		Description  string `json:"description"`
		SourceBranch string `json:"source_branch"`
	}
	if _, err := g.do(ctx, "GET", u, nil, &out, g.headers()); err != nil {
		return nil, err
	}
	var refs []PRRef
	for _, mr := range out {
		if !strings.Contains(mr.Description, Marker) {
			continue
		}
		refs = append(refs, PRRef{Forge: "gitlab", Number: mr.IID, URL: mr.WebURL, Head: mr.SourceBranch})
	}
	return refs, nil
}

// Gitea opens pull requests; Forgejo speaks the same API.
type Gitea struct {
	Remote
	Owner string
	Repo  string
}

// Name implements Forge.
func (g *Gitea) Name() string { return "gitea" }

func (g *Gitea) headers() map[string]string {
	if g.Token == "" {
		return nil
	}
	return map[string]string{"Authorization": "token " + g.Token}
}

// Push is git's job; see GitHub.Push.
func (g *Gitea) Push(context.Context, string, string) error { return nil }

func (g *Gitea) repoPath() (string, error) {
	if g.Owner == "" || g.Repo == "" {
		return "", fmt.Errorf("gitea forge needs an owner and repository")
	}
	return g.Owner + "/" + g.Repo, nil
}

// OpenPR creates a pull request.
func (g *Gitea) OpenPR(ctx context.Context, _ string, pr PullRequest) (PRRef, error) {
	repo, err := g.repoPath()
	if err != nil {
		return PRRef{}, err
	}
	payload := map[string]any{
		"title": pr.Title,
		"head":  pr.Branch,
		"base":  pr.Base,
		"body":  pr.Body,
	}
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	u := fmt.Sprintf("%s/api/v1/repos/%s/pulls", strings.TrimSuffix(g.Base, "/"), repo)
	if _, err := g.do(ctx, "POST", u, payload, &out, g.headers()); err != nil {
		return PRRef{}, err
	}
	return PRRef{Forge: "gitea", Number: out.Number, URL: out.HTMLURL, Head: pr.Branch}, nil
}

// ListManagedPRs returns open pull requests carrying Marker.
func (g *Gitea) ListManagedPRs(ctx context.Context, _ string) ([]PRRef, error) {
	repo, err := g.repoPath()
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/api/v1/repos/%s/pulls?state=open&limit=100", strings.TrimSuffix(g.Base, "/"), repo)
	var out []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if _, err := g.do(ctx, "GET", u, nil, &out, g.headers()); err != nil {
		return nil, err
	}
	var refs []PRRef
	for _, pr := range out {
		if !strings.Contains(pr.Body, Marker) {
			continue
		}
		refs = append(refs, PRRef{Forge: "gitea", Number: pr.Number, URL: pr.HTMLURL, Head: pr.Head.Ref})
	}
	return refs, nil
}
