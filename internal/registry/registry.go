// Package registry fetches published versions from package registries.
//
// Responses are cached on disk with ETags and a TTL, so repeated scans and
// offline runs stay cheap. The cache is derived state: it lives under the state
// directory and can be deleted at any time.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/httpcache"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// ProxyURL is the default Go module proxy.
const ProxyURL = "https://proxy.golang.org"

// Client is a Go module proxy client.
type Client struct {
	base string
	http *httpcache.Client
}

// NewClient builds a proxy client. base and cacheDir are injected so tests can
// point at an httptest server and a temp directory.
func NewClient(base, cacheDir string, offline bool) (*Client, error) {
	if base == "" {
		base = ProxyURL
	}
	hc, err := httpcache.NewClient(cacheDir, offline)
	if err != nil {
		return nil, err
	}
	return &Client{base: strings.TrimSuffix(base, "/"), http: hc}, nil
}

// Versions lists published versions for a module, newest first.
func (c *Client) Versions(ctx context.Context, modPath string) ([]ecosystem.Version, error) {
	escaped, err := module.EscapePath(modPath)
	if err != nil {
		return nil, fmt.Errorf("invalid module path %q: %w", modPath, err)
	}
	body, err := c.http.Get(ctx, c.base+"/"+escaped+"/@v/list")
	if err != nil {
		return nil, err
	}
	var out []ecosystem.Version
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, ecosystem.Version{Raw: line})
		}
	}
	out = ecosystem.Dedupe(out)
	ecosystem.SortDescending(out, semver.Compare)
	return out, nil
}

// PublishTime reports when a version was published, as RFC3339. Cooldown
// depends on this: the registry exposes it in the version's .info document.
func (c *Client) PublishTime(ctx context.Context, modPath, version string) (string, error) {
	escapedMod, err := module.EscapePath(modPath)
	if err != nil {
		return "", fmt.Errorf("invalid module path %q: %w", modPath, err)
	}
	escapedVer, err := module.EscapeVersion(version)
	if err != nil {
		return "", fmt.Errorf("invalid version %q: %w", version, err)
	}
	body, err := c.http.Get(ctx, c.base+"/"+escapedMod+"/@v/"+escapedVer+".info")
	if err != nil {
		return "", err
	}
	var info struct {
		Version string `json:"Version"`
		Time    string `json:"Time"`
	}
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		return "", fmt.Errorf("decode info for %s@%s: %w", modPath, version, err)
	}
	t, err := time.Parse(time.RFC3339, info.Time)
	if err != nil {
		return "", nil // unknown publish time; cooldown decides what to do
	}
	return t.UTC().Format(time.RFC3339), nil
}
