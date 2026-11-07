// Package ecosystem defines what a package manager has to provide to be
// supported (§6). Version identity is deliberately opaque: Docker tags and
// Action refs are not semver, so each ecosystem owns its own ordering.
package ecosystem

import (
	"context"
	"sort"
	"strings"
)

// Target is one manifest to update, e.g. a go.mod.
type Target struct {
	Ecosystem string `json:"ecosystem"`
	// Dir is the directory containing the manifest, relative to the repository
	// root, so monorepo targets stay distinguishable.
	Dir      string `json:"dir"`
	Manifest string `json:"manifest"`
	// Module is the ecosystem-native identity of the code being updated.
	Module string `json:"module,omitempty"`
}

// Key is a stable identifier for a target within a repository.
func (t Target) Key() string {
	return t.Ecosystem + ":" + t.Dir
}

// Dependency is one resolved requirement.
type Dependency struct {
	Name       string `json:"name"`
	Current    string `json:"current"`
	Constraint string `json:"constraint,omitempty"`
	// Direct is false for indirect (transitive) requirements, which policy
	// treats differently: reported, but not proposed unless an advisory forces
	// it (§3 row 9).
	Direct bool   `json:"direct"`
	Target string `json:"target"`
}

// Version is a registry-published version.
type Version struct {
	Raw         string `json:"version"`
	PublishedAt string `json:"published_at,omitempty"` // RFC3339, empty if unknown
	Yanked      bool   `json:"yanked,omitempty"`
	Retracted   bool   `json:"retracted,omitempty"`
}

// UpdateClass is the semver distance between two versions.
type UpdateClass string

// Update classes. Unknown is an honest answer for versions that do not parse
// under the ecosystem's rules.
const (
	ClassNone    UpdateClass = "none"
	ClassPatch   UpdateClass = "patch"
	ClassMinor   UpdateClass = "minor"
	ClassMajor   UpdateClass = "major"
	ClassDigest  UpdateClass = "digest"
	ClassUnknown UpdateClass = "unknown"
)

// Ecosystem is the single extension point for package managers.
type Ecosystem interface {
	// Name is the stable identifier used in configuration.
	Name() string
	// Detect finds manifest targets under root.
	Detect(ctx context.Context, root string) ([]Target, error)
	// Inventory lists dependencies for one target.
	Inventory(ctx context.Context, t Target) ([]Dependency, error)
	// Compare orders two versions under this ecosystem's semantics.
	Compare(a, b string) int
	// Classify reports the semver distance between two versions.
	Classify(from, to string) UpdateClass
	// VerifyCommands returns the build and test commands Phase B should run.
	VerifyCommands(t Target) [][]string
}

// Registry returns published versions for a module path. It is separate from
// Ecosystem because fetching is I/O and must be fakeable in tests.
type Registry interface {
	// Versions returns every published version, newest first.
	Versions(ctx context.Context, module string) ([]Version, error)
	// PublishTime reports when a version was published, for cooldown (§3 row 5).
	PublishTime(ctx context.Context, module, version string) (string, error)
}

// SortDescending orders versions newest-first using cmp.
func SortDescending(vs []Version, cmp func(a, b string) int) {
	sort.SliceStable(vs, func(i, j int) bool { return cmp(vs[i].Raw, vs[j].Raw) > 0 })
}

// Dedupe removes duplicate and empty entries while preserving order.
func Dedupe(vs []Version) []Version {
	seen := make(map[string]bool, len(vs))
	out := vs[:0]
	for _, v := range vs {
		v.Raw = strings.TrimSpace(v.Raw)
		if v.Raw == "" || seen[v.Raw] {
			continue
		}
		seen[v.Raw] = true
		out = append(out, v)
	}
	return out
}
