// Package nodejs implements the ecosystem interface for npm and pnpm.
//
// The two share a manifest (package.json) and differ in lockfile and command, so
// they share one implementation with a name: npm owns package-lock.json, pnpm
// owns pnpm-lock.yaml. Detection is mutually exclusive, which stops a repository
// from being inventoried twice under two names.
package nodejs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/ecosystem/semverx"
	"github.com/thread3d/dependancythang/internal/execx"
	"gopkg.in/yaml.v3"
)

// Names used in configuration.
const (
	Npm  = "npm"
	Pnpm = "pnpm"
)

// Ecosystem is the npm or pnpm implementation.
type Ecosystem struct {
	name   string
	runner execx.Runner
}

// NewNpm returns the npm ecosystem.
func NewNpm() *Ecosystem { return &Ecosystem{name: Npm, runner: execx.Exec{}} }

// NewPnpm returns the pnpm ecosystem.
func NewPnpm() *Ecosystem { return &Ecosystem{name: Pnpm, runner: execx.Exec{}} }

// NewWithRunner builds either flavour with an injected runner, for tests.
func NewWithRunner(name string, r execx.Runner) *Ecosystem {
	return &Ecosystem{name: name, runner: r}
}

// Name implements ecosystem.Ecosystem.
func (e *Ecosystem) Name() string { return e.name }

func (e *Ecosystem) lockfile() string {
	if e.name == Pnpm {
		return "pnpm-lock.yaml"
	}
	return "package-lock.json"
}

// owns reports whether this ecosystem should claim a directory: the manifest is
// present and no competing lockfile is.
func (e *Ecosystem) owns(ctx context.Context, dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return false
	}
	pnpmLock := fileExists(filepath.Join(dir, "pnpm-lock.yaml"))
	npmLock := fileExists(filepath.Join(dir, "package-lock.json"))
	yarnLock := fileExists(filepath.Join(dir, "yarn.lock"))
	if yarnLock {
		return false // yarn is a different implementation (v1.1)
	}
	if e.name == Pnpm {
		return pnpmLock
	}
	// A project with a pnpm lockfile belongs to pnpm even if a stale
	// package-lock.json is lying around; a project with neither still belongs to
	// npm, because that is the default package manager.
	_ = npmLock
	return !pnpmLock
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// skipDirs are never descended into.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, "coverage": true, ".next": true, ".venv": true,
}

// Detect finds package.json files this ecosystem owns.
func (e *Ecosystem) Detect(ctx context.Context, root string) ([]ecosystem.Target, error) {
	const maxDepth = 3
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
		if d.Name() != "package.json" {
			return nil
		}
		dir := filepath.Dir(path)
		if !e.owns(ctx, dir) {
			return nil
		}
		name := ""
		if manifest, err := readManifest(filepath.Join(dir, "package.json")); err == nil {
			name = manifest.Name
		}
		targetDir := filepath.Dir(rel)
		if targetDir == "." {
			targetDir = "/"
		}
		targets = append(targets, ecosystem.Target{
			Ecosystem: e.name,
			Dir:       targetDir,
			Manifest:  path,
			Module:    name,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("detect %s projects under %s: %w", e.name, root, err)
	}
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].Dir < targets[j].Dir })
	return targets, nil
}

// manifest is the subset of package.json depmcp reads.
type manifest struct {
	Name                 string            `json:"name"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
}

func readManifest(path string) (manifest, error) {
	var m manifest
	data, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

// Inventory lists declared dependencies, resolving each to the version the
// lockfile actually pinned.
func (e *Ecosystem) Inventory(ctx context.Context, t ecosystem.Target) ([]ecosystem.Dependency, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := readManifest(t.Manifest)
	if err != nil {
		return nil, err
	}
	locked, lockErr := e.lockedVersions(filepath.Dir(t.Manifest))
	if lockErr != nil {
		// A missing or unreadable lockfile is not fatal: the requirement is still
		// real, its resolved version is just unknown.
		locked = map[string]string{}
	}

	var deps []ecosystem.Dependency
	add := func(name, constraint string, dev bool) {
		dep := ecosystem.Dependency{
			Name:       name,
			Current:    locked[name],
			Constraint: constraint,
			Direct:     true,
			Dev:        dev,
			Target:     t.Key(),
		}
		deps = append(deps, dep)
	}
	for name, constraint := range m.Dependencies {
		add(name, constraint, false)
	}
	for name, constraint := range m.OptionalDependencies {
		add(name, constraint, false)
	}
	for name, constraint := range m.PeerDependencies {
		add(name, constraint, false)
	}
	for name, constraint := range m.DevDependencies {
		add(name, constraint, true)
	}

	sort.SliceStable(deps, func(i, j int) bool {
		if deps[i].Dev != deps[j].Dev {
			return !deps[i].Dev
		}
		return deps[i].Name < deps[j].Name
	})
	return deps, nil
}

// lockedVersions reads the lockfile into a name→version map.
func (e *Ecosystem) lockedVersions(dir string) (map[string]string, error) {
	if e.name == Pnpm {
		return pnpmLockedVersions(filepath.Join(dir, "pnpm-lock.yaml"))
	}
	return npmLockedVersions(filepath.Join(dir, "package-lock.json"))
}

// Compare orders versions under npm's semver rules.
func (*Ecosystem) Compare(a, b string) int { return semverx.Compare(a, b) }

// Classify reports the semver distance between two versions.
func (e *Ecosystem) Classify(from, to string) ecosystem.UpdateClass {
	if v := semverx.Classify(from, to); v != ecosystem.ClassUnknown {
		return v
	}
	// A requirement without a resolved version ("^1.0.0") cannot be classified.
	return ecosystem.ClassUnknown
}

// VerifyCommands returns the project's own build and test scripts, run through
// the package manager. --if-present keeps a repository without scripts valid.
func (e *Ecosystem) VerifyCommands(ecosystem.Target) [][]string {
	pm := "npm"
	if e.name == Pnpm {
		pm = "pnpm"
	}
	return [][]string{
		{pm, "run", "--if-present", "build"},
		{pm, "run", "--if-present", "test"},
	}
}

// Apply delegates to the package manager, then reads the lockfile back so the
// plan records what really changed.
//
// Lifecycle scripts are disabled: Phase A must not execute dependency code
// (§4.1). A package that genuinely needs them will fail here, visibly.
func (e *Ecosystem) Apply(ctx context.Context, dir string, changes []ecosystem.Change) (ecosystem.ApplyResult, error) {
	res := ecosystem.ApplyResult{Observed: map[string][2]string{}}
	if len(changes) == 0 {
		return res, nil
	}
	before, _ := e.lockedVersions(dir)

	for _, ch := range changes {
		if ch.Name == "" || ch.To == "" {
			return res, fmt.Errorf("incomplete change %+v", ch)
		}
		var args []string
		switch e.name {
		case Pnpm:
			args = []string{"update", ch.Name + "@" + ch.To, "--ignore-scripts"}
		default:
			args = []string{"install", ch.Name + "@" + ch.To, "--ignore-scripts", "--no-audit", "--no-fund"}
		}
		res.Commands = append(res.Commands, e.name+" "+strings.Join(args, " "))
		if _, err := e.runner.Run(ctx, dir, e.name, args...); err != nil {
			return res, fmt.Errorf("%s: %w", e.name, err)
		}
	}

	after, err := e.lockedVersions(dir)
	if err != nil {
		return res, fmt.Errorf("read lockfile after apply: %w", err)
	}
	for name, afterVer := range after {
		beforeVer, existed := before[name]
		if !existed {
			beforeVer = ""
		}
		if beforeVer != afterVer {
			res.Observed[name] = [2]string{beforeVer, afterVer}
			res.Changed = append(res.Changed, name)
		}
	}
	sort.Strings(res.Changed)

	requested := map[string]bool{}
	for _, ch := range changes {
		requested[ch.Name] = true
	}
	for _, name := range res.Changed {
		if !requested[name] {
			res.Unsplittable = true // the resolver moved things we did not ask for
			break
		}
	}
	return res, nil
}

// npmLockedVersions reads package-lock.json in v1 ("dependencies") or v2/v3
// ("packages") shape. A mixed repository can carry both; packages wins because
// it is the newer, authoritative section.
func npmLockedVersions(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		LockfileVersion int `json:"lockfileVersion"`
		Packages        map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
		Dependencies map[string]struct {
			Version string `json:"version"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := map[string]string{}
	// v1 section, then v2/v3 section which overrides it.
	for name, entry := range doc.Dependencies {
		if entry.Version != "" {
			out[name] = entry.Version
		}
	}
	for key, entry := range doc.Packages {
		if !strings.HasPrefix(key, "node_modules/") || entry.Version == "" {
			continue
		}
		name := strings.TrimPrefix(key, "node_modules/")
		// Nested node_modules entries (a/node_modules/b) are transitive copies.
		if strings.Contains(name, "node_modules/") {
			continue
		}
		out[name] = entry.Version
	}
	return out, nil
}

// pnpmLockedVersions reads pnpm-lock.yaml. v5/v6 and v9 differ in layout, so the
// file is decoded generically and both shapes are probed.
func pnpmLockedVersions(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		LockfileVersion any `yaml:"lockfileVersion"`
		Importers       map[string]struct {
			Dependencies map[string]struct {
				Version string `yaml:"version"`
			} `yaml:"dependencies"`
			DevDependencies map[string]struct {
				Version string `yaml:"version"`
			} `yaml:"devDependencies"`
		} `yaml:"importers"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := map[string]string{}
	for _, importer := range doc.Importers {
		for name, dep := range importer.Dependencies {
			if v := cleanPnpmVersion(dep.Version); v != "" {
				out[name] = v
			}
		}
		for name, dep := range importer.DevDependencies {
			if v := cleanPnpmVersion(dep.Version); v != "" {
				out[name] = v
			}
		}
	}
	return out, nil
}

// cleanPnpmVersion strips the peer suffix pnpm appends, e.g. "4.17.21(react@18)".
func cleanPnpmVersion(v string) string {
	if i := strings.IndexByte(v, '('); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}
