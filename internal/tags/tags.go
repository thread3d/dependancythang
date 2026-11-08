// Package tags fetches published versions from sources that expose a plain list
// of tags rather than a resolver: the npm registry, Docker Hub, and GitHub.
//
// It is the TagSource half of §6: ecosystems whose identity is not a semver
// triple still need candidates, and each source knows its own pagination,
// timestamps, and secondary identity (a digest or a commit SHA).
package tags

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/ecosystem/semverx"
	"github.com/thread3d/dependancythang/internal/httpcache"
)

// Source lists published versions for a reference.
type Source interface {
	Name() string
	Tags(ctx context.Context, ref string) ([]ecosystem.Version, error)
}

// NPM reads registry.npmjs.org, which returns every version and its publish time
// in one document.
type NPM struct {
	Base string
	HTTP *httpcache.Client
}

// NewNPM builds an npm tag source.
func NewNPM(cacheDir string, offline bool) (*NPM, error) {
	hc, err := httpcache.NewClient(cacheDir, offline)
	if err != nil {
		return nil, err
	}
	return &NPM{Base: "https://registry.npmjs.org", HTTP: hc}, nil
}

// Name implements Source.
func (n *NPM) Name() string { return "npm" }

// Tags implements Source.
func (n *NPM) Tags(ctx context.Context, pkg string) ([]ecosystem.Version, error) {
	body, err := n.HTTP.Get(ctx, strings.TrimSuffix(n.Base, "/")+"/"+url.PathEscape(pkg))
	if err != nil {
		return nil, fmt.Errorf("npm registry %s: %w", pkg, err)
	}
	var doc struct {
		Versions map[string]json.RawMessage `json:"versions"`
		Time     map[string]string          `json:"time"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return nil, fmt.Errorf("decode npm registry %s: %w", pkg, err)
	}
	out := make([]ecosystem.Version, 0, len(doc.Versions))
	for version := range doc.Versions {
		out = append(out, ecosystem.Version{Raw: version, PublishedAt: doc.Time[version]})
	}
	if len(out) == 0 {
		// A 200 with an empty versions map means the package does not exist.
		return nil, fmt.Errorf("npm registry %s: no versions published", pkg)
	}
	ecosystem.SortDescending(out, semverx.Compare)
	return out, nil
}

// DockerHub reads hub.docker.com's public API.
type DockerHub struct {
	Base string
	HTTP *httpcache.Client
}

// NewDockerHub builds a Docker Hub tag source.
func NewDockerHub(cacheDir string, offline bool) (*DockerHub, error) {
	hc, err := httpcache.NewClient(cacheDir, offline)
	if err != nil {
		return nil, err
	}
	return &DockerHub{Base: "https://hub.docker.com", HTTP: hc}, nil
}

// Name implements Source.
func (d *DockerHub) Name() string { return "dockerhub" }

// Tags implements Source. The reference is "namespace/name"; a bare name is
// treated as an official image under library/.
func (d *DockerHub) Tags(ctx context.Context, image string) ([]ecosystem.Version, error) {
	repo := image
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	endpoint := fmt.Sprintf("%s/v2/repositories/%s/tags?page_size=100&ordering=last_updated",
		strings.TrimSuffix(d.Base, "/"), repo)
	body, err := d.HTTP.Get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("docker hub %s: %w", image, err)
	}
	var doc struct {
		Results []struct {
			Name        string `json:"name"`
			LastUpdated string `json:"last_updated"`
			Digest      string `json:"digest"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return nil, fmt.Errorf("decode docker hub %s: %w", image, err)
	}
	out := make([]ecosystem.Version, 0, len(doc.Results))
	for _, r := range doc.Results {
		if r.Name == "" {
			continue
		}
		out = append(out, ecosystem.Version{Raw: r.Name, PublishedAt: r.LastUpdated, Digest: r.Digest})
	}
	return out, nil
}

// GitHub reads the GitHub REST API for repository tags.
type GitHub struct {
	Base string
	HTTP *httpcache.Client
}

// NewGitHub builds a GitHub tag source.
func NewGitHub(cacheDir string, offline bool) (*GitHub, error) {
	hc, err := httpcache.NewClient(cacheDir, offline)
	if err != nil {
		return nil, err
	}
	return &GitHub{Base: "https://api.github.com", HTTP: hc}, nil
}

// Name implements Source.
func (g *GitHub) Name() string { return "github" }

// Tags implements Source. Each tag carries its commit SHA as the digest, which
// is what pinning a workflow to an immutable ref actually requires.
func (g *GitHub) Tags(ctx context.Context, repo string) ([]ecosystem.Version, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/tags?per_page=100", strings.TrimSuffix(g.Base, "/"), repo)
	body, err := g.HTTP.Get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("github tags %s: %w", repo, err)
	}
	var doc []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return nil, fmt.Errorf("decode github tags %s: %w", repo, err)
	}
	out := make([]ecosystem.Version, 0, len(doc))
	for _, t := range doc {
		if t.Name == "" {
			continue
		}
		out = append(out, ecosystem.Version{Raw: t.Name, Digest: t.Commit.SHA})
	}
	return out, nil
}

// PyPI reads the PyPI JSON API, which returns every release with its upload time.
type PyPI struct {
	Base string
	HTTP *httpcache.Client
}

// NewPyPI builds a PyPI tag source.
func NewPyPI(cacheDir string, offline bool) (*PyPI, error) {
	hc, err := httpcache.NewClient(cacheDir, offline)
	if err != nil {
		return nil, err
	}
	return &PyPI{Base: "https://pypi.org", HTTP: hc}, nil
}

// Name implements Source.
func (p *PyPI) Name() string { return "pypi" }

// Tags implements Source.
func (p *PyPI) Tags(ctx context.Context, pkg string) ([]ecosystem.Version, error) {
	endpoint := strings.TrimSuffix(p.Base, "/") + "/pypi/" + url.PathEscape(pkg) + "/json"
	body, err := p.HTTP.Get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("pypi %s: %w", pkg, err)
	}
	var doc struct {
		Releases map[string][]struct {
			UploadTime string `json:"upload_time_iso_8601"`
		} `json:"releases"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return nil, fmt.Errorf("decode pypi %s: %w", pkg, err)
	}
	out := make([]ecosystem.Version, 0, len(doc.Releases))
	for version, files := range doc.Releases {
		if version == "" {
			continue
		}
		published := ""
		if len(files) > 0 {
			published = files[0].UploadTime
		}
		// A yanked release has no files at all.
		out = append(out, ecosystem.Version{Raw: version, PublishedAt: published, Yanked: len(files) == 0})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pypi %s: no releases published", pkg)
	}
	ecosystem.SortDescending(out, semverx.Compare)
	return out, nil
}

// Crates reads crates.io, which paginates versions separately from the crate.
type Crates struct {
	Base string
	HTTP *httpcache.Client
}

// NewCrates builds a crates.io tag source.
func NewCrates(cacheDir string, offline bool) (*Crates, error) {
	hc, err := httpcache.NewClient(cacheDir, offline)
	if err != nil {
		return nil, err
	}
	return &Crates{Base: "https://crates.io", HTTP: hc}, nil
}

// Name implements Source.
func (c *Crates) Name() string { return "crates" }

// Tags implements Source.
func (c *Crates) Tags(ctx context.Context, crate string) ([]ecosystem.Version, error) {
	endpoint := strings.TrimSuffix(c.Base, "/") + "/api/v1/crates/" + url.PathEscape(crate) + "/versions"
	body, err := c.HTTP.Get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("crates.io %s: %w", crate, err)
	}
	var doc struct {
		Versions []struct {
			Num       string `json:"num"`
			Yanked    bool   `json:"yanked"`
			CreatedAt string `json:"created_at"`
		} `json:"versions"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return nil, fmt.Errorf("decode crates.io %s: %w", crate, err)
	}
	out := make([]ecosystem.Version, 0, len(doc.Versions))
	for _, v := range doc.Versions {
		if v.Num == "" {
			continue
		}
		out = append(out, ecosystem.Version{Raw: v.Num, PublishedAt: v.CreatedAt, Yanked: v.Yanked})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("crates.io %s: no versions published", crate)
	}
	ecosystem.SortDescending(out, semverx.Compare)
	return out, nil
}

// Maven reads maven-metadata.xml from a repository, which lists every published
// version of one groupId:artifactId.
type Maven struct {
	Base string
	HTTP *httpcache.Client
}

// NewMaven builds a Maven Central tag source.
func NewMaven(cacheDir string, offline bool) (*Maven, error) {
	hc, err := httpcache.NewClient(cacheDir, offline)
	if err != nil {
		return nil, err
	}
	return &Maven{Base: "https://repo1.maven.org/maven2", HTTP: hc}, nil
}

// Name implements Source.
func (m *Maven) Name() string { return "maven-central" }

// Tags implements Source. The reference is "groupId:artifactId".
func (m *Maven) Tags(ctx context.Context, coord string) ([]ecosystem.Version, error) {
	group, artifact, ok := strings.Cut(coord, ":")
	if !ok || group == "" || artifact == "" {
		return nil, fmt.Errorf("maven coordinate %q is not groupId:artifactId", coord)
	}
	path := strings.ReplaceAll(group, ".", "/")
	endpoint := strings.TrimSuffix(m.Base, "/") + "/" + path + "/" + artifact + "/maven-metadata.xml"
	body, err := m.HTTP.Get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("maven central %s: %w", coord, err)
	}
	var doc struct {
		Versioning struct {
			Latest     string `xml:"latest"`
			Release    string `xml:"release"`
			LastUpdate string `xml:"lastUpdated"`
			Versions   struct {
				Version []string `xml:"version"`
			} `xml:"versions"`
		} `xml:"versioning"`
	}
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		return nil, fmt.Errorf("decode maven-metadata for %s: %w", coord, err)
	}
	out := make([]ecosystem.Version, 0, len(doc.Versioning.Versions.Version))
	for _, v := range doc.Versioning.Versions.Version {
		if v == "" {
			continue
		}
		// A SNAPSHOT is a moving target, not a release: it is reported but never
		// proposed as an update target.
		out = append(out, ecosystem.Version{
			Raw:    v,
			Yanked: strings.Contains(strings.ToUpper(v), "SNAPSHOT"),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("maven central %s: no versions published", coord)
	}
	return out, nil
}
