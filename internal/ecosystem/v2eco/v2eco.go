// Package v2eco holds the v2 breadth ecosystems: Python (pip/uv/poetry), Cargo,
// and yarn.
//
// They share a package because they are small and differ only in manifest
// format; splitting three two-hundred-line implementations across three packages
// would add ceremony without adding clarity. Each still satisfies the same
// Ecosystem interface as the v1 ones.
package v2eco

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
	"github.com/thread3d/dependancythang/internal/execx"
	"gopkg.in/yaml.v3"
)

// ---- Python -------------------------------------------------------------

// Python covers requirements.txt, PEP 621 pyproject.toml, and poetry's tables.
type Python struct {
	runner execx.Runner
	// Manager selects the apply command: pip, uv, or poetry. Empty defers to what
	// the repository contains.
	Manager string
}

// NewPython returns the Python ecosystem.
func NewPython() *Python { return &Python{runner: execx.Exec{}} }

// NewPythonWithRunner builds a test-friendly instance.
func NewPythonWithRunner(r execx.Runner, manager string) *Python {
	return &Python{runner: r, Manager: manager}
}

// Name implements ecosystem.Ecosystem.
func (*Python) Name() string { return "pip" }

// Detect finds requirements files and pyproject.toml.
func (p *Python) Detect(ctx context.Context, root string) ([]ecosystem.Target, error) {
	var targets []ecosystem.Target
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		depth := strings.Count(rel, string(os.PathSeparator))
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == ".venv") {
				return filepath.SkipDir
			}
			if depth >= 3 {
				return filepath.SkipDir
			}
			return nil
		}
		switch base := d.Name(); {
		case base == "requirements.txt", strings.HasPrefix(base, "requirements-") && strings.HasSuffix(base, ".txt"):
		case base == "pyproject.toml":
		default:
			return nil
		}
		dir := filepath.Dir(rel)
		if dir == "." {
			dir = "/"
		}
		targets = append(targets, ecosystem.Target{Ecosystem: "pip", Dir: dir, Manifest: path})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].Manifest < targets[j].Manifest })
	return targets, nil
}

// requirementRE reads "name==1.2.3" (and the other operators) from a line.
var requirementRE = regexp.MustCompile(`^\s*([A-Za-z0-9._-]+)(\s*\[[^\]]*\])?\s*(==|>=|<=|~=|>|<)?\s*([A-Za-z0-9._*+-]+)?`)

// pythonDepRE reads a quoted dependency specifier inside a TOML array, as PEP 621
// uses: "requests==2.31.0".
var pythonDepRE = regexp.MustCompile(`"([A-Za-z0-9._-]+)(\[[^\]]*\])?\s*(==|>=|<=|~=|>|<)\s*([A-Za-z0-9._*+-]+)"`)

// poetryDepRE reads poetry's table form: httpx = "0.27.0".
var poetryDepRE = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9._-]+)\s*=\s*\{?\s*(?:version\s*=\s*)?"([\^~>=<]*[0-9][^"]*)"`)

// Inventory lists Python requirements.
func (p *Python) Inventory(ctx context.Context, t ecosystem.Target) ([]ecosystem.Dependency, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(t.Manifest)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.Manifest, err)
	}
	var deps []ecosystem.Dependency
	seen := map[string]bool{}
	add := func(name, constraint, current string) {
		if name == "" || seen[strings.ToLower(name)] {
			return
		}
		seen[strings.ToLower(name)] = true
		deps = append(deps, ecosystem.Dependency{
			Name: name, Current: current, Constraint: constraint, Direct: true, Target: t.Key(),
		})
	}

	if strings.HasSuffix(t.Manifest, ".toml") {
		for _, m := range pythonDepRE.FindAllStringSubmatch(string(data), -1) {
			// Only an exact pin gives a current version; a range does not.
			current := ""
			if m[3] == "==" {
				current = m[4]
			}
			add(m[1], m[3]+m[4], current)
		}
		// poetry writes dependencies as a table rather than a specifier string.
		for _, m := range poetryDepRE.FindAllStringSubmatch(string(data), -1) {
			spec := strings.TrimLeft(m[2], "^~>=<")
			current := ""
			if !strings.ContainsAny(m[2], "^~><") && strings.Count(spec, ".") >= 1 {
				current = spec
			}
			add(m[1], m[2], current)
		}
	} else {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
				continue
			}
			if i := strings.Index(line, " #"); i >= 0 {
				line = line[:i]
			}
			m := requirementRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			current := ""
			if m[3] == "==" {
				current = m[4]
			}
			add(m[1], m[3]+m[4], current)
		}
	}
	sort.SliceStable(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	return deps, nil
}

// Compare orders Python versions. Python versions are semver-ish but not semver
// (no leading v, no required patch), so the tolerant comparator is used.
func (*Python) Compare(a, b string) int { return semverx.Compare(a, b) }

// Classify reports the update distance.
func (*Python) Classify(from, to string) ecosystem.UpdateClass {
	if class := semverx.Classify(from, to); class != ecosystem.ClassUnknown {
		return class
	}
	return ecosystem.ClassUnknown
}

// manager decides which tool to drive.
func (p *Python) manager(dir string) string {
	if p.Manager != "" {
		return p.Manager
	}
	if fileExists(filepath.Join(dir, "uv.lock")) {
		return "uv"
	}
	if fileExists(filepath.Join(dir, "poetry.lock")) {
		return "poetry"
	}
	return "pip"
}

// Apply rewrites a requirements file's pins directly and delegates everything
// else to the project's own tool, which is the §6 rule.
func (p *Python) Apply(ctx context.Context, dir string, changes []ecosystem.Change) (ecosystem.ApplyResult, error) {
	res := ecosystem.ApplyResult{Observed: map[string][2]string{}}
	if len(changes) == 0 {
		return res, nil
	}
	if len(changes) == 1 && changes[0].Name == "" {
		return res, fmt.Errorf("incomplete change %+v", changes[0])
	}

	manager := p.manager(dir)
	// A requirements file can be edited safely: it is the manifest, not a lock.
	reqPath := filepath.Join(dir, "requirements.txt")
	if fileExists(reqPath) && manager == "pip" {
		data, err := os.ReadFile(reqPath)
		if err != nil {
			return res, err
		}
		lines := strings.Split(string(data), "\n")
		want := map[string]ecosystem.Change{}
		for _, ch := range changes {
			want[strings.ToLower(ch.Name)] = ch
		}
		for i, line := range lines {
			m := requirementRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			ch, ok := want[strings.ToLower(m[1])]
			if !ok || m[3] != "==" {
				continue
			}
			lines[i] = strings.Replace(line, m[3]+m[4], "=="+ch.To, 1)
			res.Observed[ch.Name] = [2]string{m[4], ch.To}
			res.Changed = append(res.Changed, ch.Name)
		}
		if len(res.Changed) > 0 {
			if err := os.WriteFile(reqPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
				return res, err
			}
			res.Commands = append(res.Commands, "edit requirements.txt")
		}
	}

	// With a requirements.txt and plain pip, the manifest edit is the whole job.
	// Delegating the leftovers to `pip install` would install packages for a
	// constraint depmcp deliberately left alone.
	if manager == "pip" && fileExists(reqPath) {
		sort.Strings(res.Changed)
		return res, nil
	}

	// Anything not handled by the manifest edit is delegated.
	for _, ch := range changes {
		if _, done := res.Observed[ch.Name]; done {
			continue
		}
		var args []string
		switch manager {
		case "uv":
			args = []string{"add", ch.Name + "==" + ch.To}
		case "poetry":
			args = []string{"add", ch.Name + "@" + ch.To}
		default:
			args = []string{"install", ch.Name + "==" + ch.To}
		}
		res.Commands = append(res.Commands, manager+" "+strings.Join(args, " "))
		if _, err := p.runner.Run(ctx, dir, manager, args...); err != nil {
			return res, err
		}
		res.Observed[ch.Name] = [2]string{ch.From, ch.To}
		res.Changed = append(res.Changed, ch.Name)
	}
	sort.Strings(res.Changed)
	return res, nil
}

// VerifyCommands runs the project's tests if a test runner is configured.
func (*Python) VerifyCommands(ecosystem.Target) [][]string {
	return [][]string{{"python3", "-m", "pytest", "-q"}}
}

// ---- Cargo --------------------------------------------------------------

// Cargo implements the ecosystem interface for Rust.
type Cargo struct{ runner execx.Runner }

// NewCargo returns the Cargo ecosystem.
func NewCargo() *Cargo { return &Cargo{runner: execx.Exec{}} }

// NewCargoWithRunner builds a test-friendly instance.
func NewCargoWithRunner(r execx.Runner) *Cargo { return &Cargo{runner: r} }

// Name implements ecosystem.Ecosystem.
func (*Cargo) Name() string { return "cargo" }

// Detect finds Cargo.toml files.
func (c *Cargo) Detect(ctx context.Context, root string) ([]ecosystem.Target, error) {
	return walkFor(ctx, root, "Cargo.toml", "cargo", 3)
}

// cargoDepRE reads `name = "1.2.3"` inside a dependencies table.
var cargoDepRE = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9_-]+)\s*=\s*(?:"([^"]+)"|\{[^}]*version\s*=\s*"([^"]+)"[^}]*\})`)

// Inventory lists crate requirements, resolved through Cargo.lock when present.
func (c *Cargo) Inventory(ctx context.Context, t ecosystem.Target) ([]ecosystem.Dependency, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(t.Manifest)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.Manifest, err)
	}
	locked := cargoLocked(filepath.Join(filepath.Dir(t.Manifest), "Cargo.lock"))

	// Only dependency tables hold dependencies: [package] also has `name` and
	// `version` keys, and reading them as crates invents phantom dependencies.
	var deps []ecosystem.Dependency
	seen := map[string]bool{}
	inDeps := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			section := strings.Trim(trimmed, "[]")
			inDeps = strings.Contains(section, "dependencies")
			continue
		}
		if !inDeps {
			continue
		}
		m := cargoDepRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		constraint := m[2]
		if constraint == "" {
			constraint = m[3]
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		deps = append(deps, ecosystem.Dependency{
			Name: name, Current: locked[name], Constraint: constraint,
			Direct: true, Target: t.Key(),
		})
	}
	sort.SliceStable(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	return deps, nil
}

// cargoLocked reads Cargo.lock into a crate→version map.
func cargoLocked(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var name string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "name = ") {
			name = strings.Trim(strings.TrimPrefix(line, "name = "), `"`)
			continue
		}
		if strings.HasPrefix(line, "version = ") && name != "" {
			out[name] = strings.Trim(strings.TrimPrefix(line, "version = "), `"`)
			name = ""
		}
	}
	return out
}

// Compare orders crate versions.
func (*Cargo) Compare(a, b string) int { return semverx.Compare(a, b) }

// Classify reports the update distance.
func (*Cargo) Classify(from, to string) ecosystem.UpdateClass {
	if class := semverx.Classify(from, to); class != ecosystem.ClassUnknown {
		return class
	}
	return ecosystem.ClassUnknown
}

// Apply delegates to cargo: the lockfile is cargo's to write.
func (c *Cargo) Apply(ctx context.Context, dir string, changes []ecosystem.Change) (ecosystem.ApplyResult, error) {
	res := ecosystem.ApplyResult{Observed: map[string][2]string{}}
	before := cargoLocked(filepath.Join(dir, "Cargo.lock"))
	for _, ch := range changes {
		args := []string{"update", "-p", ch.Name, "--precise", ch.To}
		res.Commands = append(res.Commands, "cargo "+strings.Join(args, " "))
		if _, err := c.runner.Run(ctx, dir, "cargo", args...); err != nil {
			return res, err
		}
		res.Observed[ch.Name] = [2]string{before[ch.Name], ch.To}
		res.Changed = append(res.Changed, ch.Name)
	}
	sort.Strings(res.Changed)
	return res, nil
}

// VerifyCommands builds and tests the crate.
func (*Cargo) VerifyCommands(ecosystem.Target) [][]string {
	return [][]string{{"cargo", "build", "--all-targets"}, {"cargo", "test"}}
}

// ---- yarn ---------------------------------------------------------------

// Yarn covers yarn v1 only. Berry uses a different lockfile and install model and
// is refused rather than misread (§6).
type Yarn struct{ runner execx.Runner }

// NewYarn returns the yarn ecosystem.
func NewYarn() *Yarn { return &Yarn{runner: execx.Exec{}} }

// NewYarnWithRunner builds a test-friendly instance.
func NewYarnWithRunner(r execx.Runner) *Yarn { return &Yarn{runner: r} }

// Name implements ecosystem.Ecosystem.
func (*Yarn) Name() string { return "yarn" }

// Detect finds yarn v1 projects: a yarn.lock with no npm or pnpm lockfile.
func (y *Yarn) Detect(ctx context.Context, root string) ([]ecosystem.Target, error) {
	var targets []ecosystem.Target
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if strings.Count(rel, string(os.PathSeparator)) >= 3 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "yarn.lock" {
			return nil
		}
		dir := filepath.Dir(path)
		if fileExists(filepath.Join(dir, "package-lock.json")) || fileExists(filepath.Join(dir, "pnpm-lock.yaml")) {
			return nil // not the active manager
		}
		if isBerryLockfile(path) {
			return nil // berry owns this file, not v1
		}
		targetDir := filepath.Dir(rel)
		if targetDir == "." {
			targetDir = "/"
		}
		targets = append(targets, ecosystem.Target{Ecosystem: "yarn", Dir: targetDir, Manifest: path})
		return nil
	})
	return targets, err
}

// yarnEntryRE reads a v1 lockfile header: `name@range:` followed by `version`.
var yarnEntryRE = regexp.MustCompile(`(?m)^"?([A-Za-z0-9._/-]+)@[^:\n]+:\s*\n\s+version\s+"?([0-9][^"\n]*)"?`)

// Inventory lists packages resolved by the lockfile.
func (y *Yarn) Inventory(ctx context.Context, t ecosystem.Target) ([]ecosystem.Dependency, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(t.Manifest)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.Manifest, err)
	}
	text := string(data)
	if strings.Contains(text, "__metadata") {
		return nil, fmt.Errorf("%s looks like a yarn berry lockfile; only yarn v1 is supported", t.Manifest)
	}
	var deps []ecosystem.Dependency
	seen := map[string]bool{}
	for _, m := range yarnEntryRE.FindAllStringSubmatch(text, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		deps = append(deps, ecosystem.Dependency{
			Name: name, Current: strings.TrimSpace(m[2]), Direct: false, Target: t.Key(),
		})
	}
	sort.SliceStable(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	return deps, nil
}

// Compare orders package versions.
func (*Yarn) Compare(a, b string) int { return semverx.Compare(a, b) }

// Classify reports the update distance.
func (*Yarn) Classify(from, to string) ecosystem.UpdateClass {
	if class := semverx.Classify(from, to); class != ecosystem.ClassUnknown {
		return class
	}
	return ecosystem.ClassUnknown
}

// Apply delegates to yarn.
func (y *Yarn) Apply(ctx context.Context, dir string, changes []ecosystem.Change) (ecosystem.ApplyResult, error) {
	res := ecosystem.ApplyResult{Observed: map[string][2]string{}}
	for _, ch := range changes {
		args := []string{"upgrade", ch.Name + "@" + ch.To, "--ignore-scripts"}
		res.Commands = append(res.Commands, "yarn "+strings.Join(args, " "))
		if _, err := y.runner.Run(ctx, dir, "yarn", args...); err != nil {
			return res, err
		}
		res.Observed[ch.Name] = [2]string{ch.From, ch.To}
		res.Changed = append(res.Changed, ch.Name)
	}
	sort.Strings(res.Changed)
	return res, nil
}

// VerifyCommands runs the project's scripts.
func (*Yarn) VerifyCommands(ecosystem.Target) [][]string {
	return [][]string{{"yarn", "run", "build"}, {"yarn", "run", "test"}}
}

// ---- shared -------------------------------------------------------------

// fileExists reports whether path is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// walkFor finds files named name, bounded in depth.
func walkFor(ctx context.Context, root, name, ecosystemName string, maxDepth int) ([]ecosystem.Target, error) {
	var targets []ecosystem.Target
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "target" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if strings.Count(rel, string(os.PathSeparator)) >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != name {
			return nil
		}
		dir := filepath.Dir(rel)
		if dir == "." {
			dir = "/"
		}
		targets = append(targets, ecosystem.Target{Ecosystem: ecosystemName, Dir: dir, Manifest: path})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].Manifest < targets[j].Manifest })
	return targets, nil
}

// ---- yarn berry ---------------------------------------------------------

// Berry covers yarn 2+ ("berry"), which has its own lockfile layout: a
// `__metadata` header and descriptor keys such as `"lodash@npm:^4.17.21"`.
//
// It is a separate ecosystem rather than a branch inside Yarn because the two
// formats share nothing beyond the filename; the v1 reader deliberately refuses
// berry, and this one refuses v1, so neither can silently misread the other.
type Berry struct{ runner execx.Runner }

// NewBerry returns the yarn berry ecosystem.
func NewBerry() *Berry { return &Berry{runner: execx.Exec{}} }

// NewBerryWithRunner builds a test-friendly instance.
func NewBerryWithRunner(r execx.Runner) *Berry { return &Berry{runner: r} }

// Name implements ecosystem.Ecosystem.
func (*Berry) Name() string { return "yarn-berry" }

// isBerryLockfile reports whether a yarn.lock is berry's format.
func isBerryLockfile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "__metadata")
}

// Detect finds berry lockfiles.
func (b *Berry) Detect(ctx context.Context, root string) ([]ecosystem.Target, error) {
	var targets []ecosystem.Target
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if strings.Count(rel, string(os.PathSeparator)) >= 3 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "yarn.lock" || !isBerryLockfile(path) {
			return nil
		}
		dir := filepath.Dir(path)
		if fileExists(filepath.Join(dir, "package-lock.json")) || fileExists(filepath.Join(dir, "pnpm-lock.yaml")) {
			return nil
		}
		targetDir := filepath.Dir(rel)
		if targetDir == "." {
			targetDir = "/"
		}
		targets = append(targets, ecosystem.Target{Ecosystem: "yarn-berry", Dir: targetDir, Manifest: path})
		return nil
	})
	return targets, err
}

// berryName extracts the package name from a registry descriptor such as
// `"@scope/pkg@npm:^1.0.0"`.
//
// Only `@npm:` yields a name. A berry lockfile also carries `@workspace:`,
// `@patch:`, `@link:`, and `@file:` descriptors, which name local packages rather
// than published ones; treating those as dependencies would invent requirements
// nobody can update. A leading `@` belongs to the scope, so the separator — not the
// first `@` — is the split point.
func berryName(descriptor string) string {
	descriptor = strings.Trim(strings.TrimSpace(descriptor), `"`)
	if i := strings.Index(descriptor, "@npm:"); i > 0 {
		return descriptor[:i]
	}
	return ""
}

// berryVersions parses a berry lockfile into name→version.
func berryVersions(text string) (map[string]string, error) {
	var doc map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("parse berry lockfile: %w", err)
	}
	out := map[string]string{}
	for key, node := range doc {
		if key == "__metadata" {
			continue
		}
		var entry struct {
			Version string `yaml:"version"`
		}
		if err := node.Decode(&entry); err != nil || entry.Version == "" {
			continue
		}
		// A key can list several descriptors for one resolution.
		for _, descriptor := range strings.Split(key, ",") {
			name := berryName(descriptor)
			if name == "" {
				continue
			}
			if _, seen := out[name]; !seen {
				out[name] = entry.Version
			}
		}
	}
	return out, nil
}

// Inventory lists packages resolved by the berry lockfile.
func (b *Berry) Inventory(ctx context.Context, t ecosystem.Target) ([]ecosystem.Dependency, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(t.Manifest)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.Manifest, err)
	}
	locked, err := berryVersions(string(data))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(locked))
	for name := range locked {
		names = append(names, name)
	}
	sort.Strings(names)

	deps := make([]ecosystem.Dependency, 0, len(names))
	for _, name := range names {
		deps = append(deps, ecosystem.Dependency{
			Name: name, Current: locked[name], Direct: false, Target: t.Key(),
		})
	}
	return deps, nil
}

// Compare orders package versions.
func (*Berry) Compare(a, b string) int { return semverx.Compare(a, b) }

// Classify reports the update distance.
func (*Berry) Classify(from, to string) ecosystem.UpdateClass {
	if class := semverx.Classify(from, to); class != ecosystem.ClassUnknown {
		return class
	}
	return ecosystem.ClassUnknown
}

// Apply delegates to berry's own command: `yarn up` is the berry spelling.
func (b *Berry) Apply(ctx context.Context, dir string, changes []ecosystem.Change) (ecosystem.ApplyResult, error) {
	res := ecosystem.ApplyResult{Observed: map[string][2]string{}}
	for _, ch := range changes {
		args := []string{"up", ch.Name + "@" + ch.To}
		res.Commands = append(res.Commands, "yarn "+strings.Join(args, " "))
		if _, err := b.runner.Run(ctx, dir, "yarn", args...); err != nil {
			return res, err
		}
		res.Observed[ch.Name] = [2]string{ch.From, ch.To}
		res.Changed = append(res.Changed, ch.Name)
	}
	sort.Strings(res.Changed)
	return res, nil
}

// VerifyCommands runs the project's scripts. Berry has no --ignore-scripts at
// apply time here because `yarn up` only rewrites the lock.
func (*Berry) VerifyCommands(ecosystem.Target) [][]string {
	return [][]string{{"yarn", "run", "build"}, {"yarn", "run", "test"}}
}
