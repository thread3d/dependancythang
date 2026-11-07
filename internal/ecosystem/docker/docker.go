// Package docker implements the ecosystem interface for container image
// references in Dockerfiles and Compose files.
//
// Image identity is a tag plus an optional digest, not a semver triple, so this
// package reports ClassPin or ClassDigest rather than pretending "3.14-slim" is
// a version number.
package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/ecosystem/semverx"
	"gopkg.in/yaml.v3"
)

// Name is the configuration identifier.
const Name = "docker"

// Ecosystem is the Docker implementation.
type Ecosystem struct{}

// New returns the Docker ecosystem.
func New() *Ecosystem { return &Ecosystem{} }

// Name implements ecosystem.Ecosystem.
func (*Ecosystem) Name() string { return Name }

// fromRE matches a Dockerfile FROM line, capturing the image reference and the
// trailing "AS name" alias so rewrites preserve it.
var fromRE = regexp.MustCompile(`^(\s*FROM\s+(?:--\S+\s+)*)([^\s]+)((?:\s+AS\s+\S+)?)(\s*)$`)

// composeImageRE matches an `image:` value in a Compose file.
var composeImageRE = regexp.MustCompile(`^(\s*image:\s*)(["']?)([^"'\s]+)(["']?)(\s*)$`)

// detectFiles reports whether a filename is one this ecosystem owns.
func detectFiles(name string) bool {
	lower := strings.ToLower(name)
	switch {
	case lower == "dockerfile":
		return true
	case strings.HasPrefix(lower, "dockerfile."):
		return true
	case strings.HasSuffix(lower, ".dockerfile"):
		return true
	case lower == "docker-compose.yml", lower == "docker-compose.yaml":
		return true
	case lower == "compose.yml", lower == "compose.yaml":
		return true
	}
	return false
}

// Detect finds Dockerfiles and Compose files.
func (e *Ecosystem) Detect(ctx context.Context, root string) ([]ecosystem.Target, error) {
	const maxDepth = 4
	var targets []ecosystem.Target

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		depth := strings.Count(rel, string(os.PathSeparator))
		if d.IsDir() {
			if rel != "." && (d.Name() == ".git" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			if depth >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !detectFiles(d.Name()) {
			return nil
		}
		dir := filepath.Dir(rel)
		if dir == "." {
			dir = "/"
		}
		targets = append(targets, ecosystem.Target{Ecosystem: Name, Dir: dir, Manifest: path})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("detect dockerfiles under %s: %w", root, err)
	}
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].Manifest < targets[j].Manifest })
	return targets, nil
}

// ref is a parsed image reference.
type ref struct {
	// Image is the repository without tag or digest.
	Image string
	Tag   string
	// Digest is the trailing @sha256:... part, empty when absent.
	Digest string
}

// parseRef splits "image:tag@sha256:..." into its parts. A reference with no tag
// implies "latest", which is what a container runtime would use.
func parseRef(s string) ref {
	var r ref
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '@'); i >= 0 {
		r.Digest = s[i+1:]
		s = s[:i]
	}
	// A colon after the last slash introduces the tag; a colon before it is a
	// registry port (localhost:5000/img).
	if i := strings.LastIndexByte(s, ':'); i > strings.LastIndexByte(s, '/') {
		r.Tag = s[i+1:]
		r.Image = s[:i]
	} else {
		r.Image = s
		r.Tag = "latest"
	}
	return r
}

// String renders the reference back to a single token.
func (r ref) String() string {
	out := r.Image
	if r.Tag != "" {
		out += ":" + r.Tag
	}
	if r.Digest != "" {
		out += "@" + r.Digest
	}
	return out
}

// Inventory lists image references in a Dockerfile or Compose file.
func (e *Ecosystem) Inventory(ctx context.Context, t ecosystem.Target) ([]ecosystem.Dependency, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(t.Manifest)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.Manifest, err)
	}
	var deps []ecosystem.Dependency

	if isCompose(t.Manifest) {
		for _, r := range composeImages(string(data)) {
			deps = append(deps, ecosystem.Dependency{
				Name: r.Image, Current: r.Tag, Constraint: r.String(),
				Direct: true, Target: t.Key(),
			})
		}
	} else {
		for _, line := range strings.Split(string(data), "\n") {
			m := fromRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if strings.EqualFold(m[2], "scratch") {
				continue // the empty image has nothing to update
			}
			r := parseRef(m[2])
			deps = append(deps, ecosystem.Dependency{
				Name: r.Image, Current: r.Tag, Constraint: r.String(),
				Direct: true, Target: t.Key(),
			})
		}
	}

	sort.SliceStable(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	return deps, nil
}

func isCompose(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return strings.HasPrefix(base, "compose.") || strings.HasPrefix(base, "docker-compose.")
}

// composeImages extracts image references from a Compose document.
func composeImages(text string) []ref {
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil
	}
	names := make([]string, 0, len(doc.Services))
	for name := range doc.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []ref
	for _, name := range names {
		image := strings.TrimSpace(doc.Services[name].Image)
		if image == "" {
			continue
		}
		out = append(out, parseRef(image))
	}
	return out
}

// Apply rewrites references in place.
//
// Only the matched lines are touched, and everything else on the line — the
// "AS build" alias, --platform flags, comments — is preserved byte for byte.
func (e *Ecosystem) Apply(ctx context.Context, dir string, changes []ecosystem.Change) (ecosystem.ApplyResult, error) {
	res := ecosystem.ApplyResult{Observed: map[string][2]string{}}
	if len(changes) == 0 {
		return res, nil
	}
	want := map[string]ecosystem.Change{}
	for _, ch := range changes {
		want[ch.Name] = ch
	}

	targets, err := e.Detect(ctx, dir)
	if err != nil {
		return res, err
	}
	for _, t := range targets {
		data, err := os.ReadFile(t.Manifest)
		if err != nil {
			return res, err
		}
		original := string(data)
		updated := original

		if isCompose(t.Manifest) {
			updated = rewriteCompose(original, want, &res)
		} else {
			updated = rewriteDockerfile(original, want, &res)
		}

		if updated != original {
			if err := os.WriteFile(t.Manifest, []byte(updated), 0o644); err != nil {
				return res, fmt.Errorf("write %s: %w", t.Manifest, err)
			}
		}
	}
	sort.Strings(res.Changed)
	return res, nil
}

// rewriteDockerfile swaps tags and digests on FROM lines.
func rewriteDockerfile(text string, want map[string]ecosystem.Change, res *ecosystem.ApplyResult) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		m := fromRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		old := parseRef(m[2])
		ch, ok := want[old.Image]
		if !ok {
			continue
		}
		next := old
		next.Tag = ch.To
		if ch.Digest != "" {
			next.Digest = ch.Digest
		}
		lines[i] = m[1] + next.String() + m[3] + m[4]
		res.Observed[old.Image] = [2]string{old.Tag, next.Tag}
		res.Changed = append(res.Changed, old.Image)
	}
	return strings.Join(lines, "\n")
}

// rewriteCompose swaps tags under services.*.image.
func rewriteCompose(text string, want map[string]ecosystem.Change, res *ecosystem.ApplyResult) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		m := composeImageRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		old := parseRef(m[3])
		ch, ok := want[old.Image]
		if !ok {
			continue
		}
		next := old
		next.Tag = ch.To
		if ch.Digest != "" {
			next.Digest = ch.Digest
		}
		// Keep the original quoting style.
		lines[i] = m[1] + m[2] + next.String() + m[4] + m[5]
		res.Observed[old.Image] = [2]string{old.Tag, next.Tag}
		res.Changed = append(res.Changed, old.Image)
	}
	return strings.Join(lines, "\n")
}

// Compare orders tags: semver-looking tags sort numerically, everything else
// falls back to string order so "latest" and date stamps stay deterministic.
func (*Ecosystem) Compare(a, b string) int {
	return semverx.Compare(a, b)
}

// Classify reports how far apart two image references are: a tag change is a
// pin (or a semver step when both tags parse), a digest change is a digest.
func (*Ecosystem) Classify(from, to string) ecosystem.UpdateClass {
	if from == to {
		return ecosystem.ClassNone
	}
	if class := semverx.Classify(from, to); class != ecosystem.ClassUnknown {
		return class
	}
	if strings.HasPrefix(to, "sha256:") || strings.HasPrefix(from, "sha256:") {
		return ecosystem.ClassDigest
	}
	return ecosystem.ClassPin
}

// VerifyCommands: a container image has nothing to build here; verification of a
// Dockerfile belongs to the project that consumes it.
func (*Ecosystem) VerifyCommands(ecosystem.Target) [][]string { return nil }
