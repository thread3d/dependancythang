// Package ecosystem defines what a package manager has to provide to be
// supported (§6). Version identity is deliberately opaque: Docker tags and
// Action refs are not semver, so each ecosystem owns its own ordering.
package ecosystem

import (
	"context"
	"errors"
	"fmt"
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
	Direct bool `json:"direct"`
	// Dev marks a development-only requirement (npm devDependencies), which
	// policy can exclude through allow rules.
	Dev    bool   `json:"dev,omitempty"`
	Target string `json:"target"`
}

// Version is a registry-published version.
type Version struct {
	Raw         string `json:"version"`
	PublishedAt string `json:"published_at,omitempty"` // RFC3339, empty if unknown
	Yanked      bool   `json:"yanked,omitempty"`
	Retracted   bool   `json:"retracted,omitempty"`
	// Digest is the secondary identity some ecosystems pin instead of, or as
	// well as, a tag: a Docker manifest digest or an Action commit SHA.
	Digest string `json:"digest,omitempty"`
}

// Change is one requested dependency bump, expressed in ecosystem terms.
type Change struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
	// Digest carries a secondary identity for ecosystems that pin one, such as a
	// Docker image digest or an Action commit SHA.
	Digest string `json:"digest,omitempty"`
}

// ApplyResult reports what actually happened, read back from the toolchain rather
// than assumed from the request (§8: the lockfile is the truth).
type ApplyResult struct {
	// Observed maps a dependency to its version before and after.
	Observed map[string][2]string `json:"observed,omitempty"`
	// Changed lists dependencies whose version moved.
	Changed []string `json:"changed,omitempty"`
	// Skipped lists changes the applier deliberately declined, with the reason.
	// A decline is not a mismatch: the ecosystem cannot express the edit (a
	// property-indirected Maven version, say) and saying so is more useful than
	// failing the plan. The run reports them instead of pretending they landed.
	Skipped []SkippedChange `json:"skipped,omitempty"`
	// Unsplittable is set when the diff is broader than requested, which happens
	// whenever the package manager resolves transitively.
	Unsplittable bool `json:"unsplittable,omitempty"`
	// Commands records what was executed, for the audit log.
	Commands []string `json:"commands,omitempty"`
}

// SkippedChange is one planned change an applier declined, and why.
type SkippedChange struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// UpdateClass is the semver distance between two versions.
type UpdateClass string

// Update classes. Unknown is an honest answer for versions that do not parse
// under the ecosystem's rules.
const (
	ClassNone   UpdateClass = "none"
	ClassPatch  UpdateClass = "patch"
	ClassMinor  UpdateClass = "minor"
	ClassMajor  UpdateClass = "major"
	ClassDigest UpdateClass = "digest"
	// ClassPin covers ecosystems whose identity is a tag or ref rather than a
	// version triple: Docker tags, Action refs.
	ClassPin     UpdateClass = "pin"
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
	// Apply rewrites manifests and lockfiles inside dir. It delegates to the
	// ecosystem's own tool wherever one exists; hand-editing lockfiles is the
	// main source of subtle breakage (§6).
	//
	// The result has to account for every change: in Observed when it landed, in
	// Skipped when this ecosystem declines it and can say why. A change that is
	// neither is treated as a plan built from a different tree, because a commit
	// listing a change that never happened is the failure mode this contract
	// exists to prevent (§8).
	Apply(ctx context.Context, dir string, changes []Change) (ApplyResult, error)
}

// VersionNormalizer is implemented by ecosystems whose canonical spelling differs
// from what an advisory database publishes. OSV writes Go fixed versions as
// "0.44.0" while go.mod, the proxy, and every other version in depmcp's own output
// carry "v0.44.0"; a plan that mixes the two is internally inconsistent.
type VersionNormalizer interface {
	NormalizeVersion(v string) string
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

// VersionMismatch is one planned change an applier could not make because the
// manifest does not hold the version the plan expects.
type VersionMismatch struct {
	Name string `json:"name"`
	// Want is the version the plan was built against.
	Want string `json:"want"`
	// Got is what the manifest actually holds, or empty when the dependency is
	// not there at all.
	Got string `json:"got,omitempty"`
}

// PlanMismatchError reports that the tree an applier was handed is not the tree
// the plan was built from: the base branch moved, or someone edited the file
// between plan and apply. §8 makes this a refusal, not a repair — rewriting the
// versions that happen to be there would produce a diff nobody reviewed and a
// record of a change that never happened.
type PlanMismatchError struct {
	// Where names the manifest, or the directory, that was looked in.
	Where string `json:"where,omitempty"`
	// Base is the ref the tree was created from, set by the caller that knows it.
	// It is what turns "the branch holds v4" into "origin/main holds v4".
	Base       string            `json:"base,omitempty"`
	Mismatches []VersionMismatch `json:"mismatches"`
}

func (e *PlanMismatchError) Error() string {
	parts := make([]string, 0, len(e.Mismatches))
	for _, m := range e.Mismatches {
		if m.Got == "" {
			parts = append(parts, fmt.Sprintf("%s is not here, the plan expects %s", m.Name, m.Want))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s is %s, the plan expects %s", m.Name, m.Got, m.Want))
	}
	msg := "the tree is not the one the plan was built from: " + strings.Join(parts, "; ")
	switch {
	case e.Where != "" && e.Base != "":
		msg += " (" + e.Where + ", based on " + e.Base + ")"
	case e.Where != "":
		msg += " (" + e.Where + ")"
	case e.Base != "":
		msg += " (based on " + e.Base + ")"
	}
	return msg + "; re-plan before applying"
}

// PlanMismatch returns the PlanMismatchError in err's chain, if there is one. The
// write path turns it into PLAN_STALE rather than a generic failure, because the
// plan is what has gone stale, not the toolchain.
func PlanMismatch(err error) (*PlanMismatchError, bool) {
	var pm *PlanMismatchError
	if errors.As(err, &pm) {
		return pm, true
	}
	return nil, false
}
