// Package gomod implements the ecosystem interface for Go modules.
//
// Manifests are parsed with golang.org/x/mod/modfile, the same package the Go
// toolchain uses, so indentation, comment placement, and the indirect marker are
// read exactly as `go mod tidy` would write them.
package gomod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

// Name is the configuration identifier.
const Name = "gomod"

// Ecosystem is the Go modules implementation.
type Ecosystem struct {
	runner Runner
}

// New returns the Go modules ecosystem, using the real toolchain.
func New() *Ecosystem { return &Ecosystem{runner: ExecRunner{}} }

// NewWithRunner returns an ecosystem that runs commands through r, so tests can
// exercise the applier without a Go toolchain or network.
func NewWithRunner(r Runner) *Ecosystem { return &Ecosystem{runner: r} }

// Name implements ecosystem.Ecosystem.
func (*Ecosystem) Name() string { return Name }

// skipDirs are never descended into when looking for manifests.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true,
	"testdata": true, "third_party": true,
}

// Detect walks root for go.mod files, bounded in depth so a large monorepo
// cannot turn discovery into a filesystem crawl.
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
			if rel != "." && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			if depth >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" {
			return nil
		}
		dir := filepath.Dir(rel)
		if dir == "." {
			dir = "/"
		}
		module, err := modulePath(path)
		if err != nil {
			// An unparseable go.mod is not a reason to abort the whole walk;
			// report the target so the caller can surface the parse error.
			module = ""
		}
		targets = append(targets, ecosystem.Target{
			Ecosystem: Name,
			Dir:       dir,
			Manifest:  path,
			Module:    module,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("detect go modules under %s: %w", root, err)
	}
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].Dir < targets[j].Dir })
	return targets, nil
}

func modulePath(goModPath string) (string, error) {
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return "", err
	}
	f, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return "", err
	}
	if f.Module == nil {
		return "", fmt.Errorf("%s: no module directive", goModPath)
	}
	return f.Module.Mod.Path, nil
}

// Inventory lists every requirement, marking indirect ones.
func (e *Ecosystem) Inventory(ctx context.Context, t ecosystem.Target) ([]ecosystem.Dependency, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(t.Manifest)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.Manifest, err)
	}
	f, err := modfile.Parse(t.Manifest, data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", t.Manifest, err)
	}

	deps := make([]ecosystem.Dependency, 0, len(f.Require))
	for _, r := range f.Require {
		deps = append(deps, ecosystem.Dependency{
			Name:       r.Mod.Path,
			Current:    r.Mod.Version,
			Constraint: r.Mod.Version,
			Direct:     !r.Indirect,
			Target:     t.Key(),
		})
	}
	sort.SliceStable(deps, func(i, j int) bool {
		if deps[i].Direct != deps[j].Direct {
			return deps[i].Direct // direct dependencies first
		}
		return deps[i].Name < deps[j].Name
	})
	return deps, nil
}

// Compare orders versions with Go's own semver rules.
func (*Ecosystem) Compare(a, b string) int { return semver.Compare(a, b) }

// Classify reports the semver distance, using Go module semantics.
func (*Ecosystem) Classify(from, to string) ecosystem.UpdateClass {
	if !semver.IsValid(from) || !semver.IsValid(to) {
		return ecosystem.ClassUnknown
	}
	if semver.Compare(from, to) == 0 {
		return ecosystem.ClassNone
	}
	// golang.org/x/mod/semver exposes Major and MajorMinor but not Minor or
	// Patch, so the middle component is derived by comparing major.minor.
	switch {
	case semver.Major(from) != semver.Major(to):
		return ecosystem.ClassMajor
	case semver.MajorMinor(from) != semver.MajorMinor(to):
		return ecosystem.ClassMinor
	default:
		return ecosystem.ClassPatch
	}
}

// VerifyCommands returns the default build and test commands for a Go module.
// `go vet` is deliberately absent: it is useful but noisy on existing code, and
// a failing verify should mean the update broke something.
func (*Ecosystem) VerifyCommands(ecosystem.Target) [][]string {
	return [][]string{
		{"go", "build", "./..."},
		{"go", "test", "./..."},
	}
}

// IsPrerelease reports whether v is a pre-release of its series, so policy can
// skip pre-releases unless the current version already is one (§6).
func IsPrerelease(v string) bool { return semver.Prerelease(v) != "" }
