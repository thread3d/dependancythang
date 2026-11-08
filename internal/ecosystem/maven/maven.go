// Package maven implements the ecosystem interface for Maven reactors.
//
// Maven breaks an assumption the other ecosystems share: there is no lockfile.
// Versions resolve from ranges at build time, so "what am I actually running?"
// cannot be answered by reading files — it requires asking Maven. That is why the
// inventory here delegates to `mvn dependency:list` instead of parsing poms, and
// why the unit of work is the reactor root rather than individual modules:
// dependencyManagement, BOM imports, and property indirection all mean the
// effective version of a dependency may be declared somewhere other than the
// element that names it.
package maven

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/execx"
	"golang.org/x/mod/semver"
)

// Ecosystem is the Maven implementation.
type Ecosystem struct {
	runner execx.Runner
}

// New returns the Maven ecosystem.
func New() *Ecosystem { return &Ecosystem{runner: execx.Exec{}} }

// NewWithRunner builds a test-friendly instance.
func NewWithRunner(r execx.Runner) *Ecosystem { return &Ecosystem{runner: r} }

// Name implements ecosystem.Ecosystem.
func (*Ecosystem) Name() string { return "maven" }

// Detect finds reactor roots and standalone poms.
//
// A pom that lists <modules> is a reactor root and is the only target reported for
// that tree: Maven resolves the whole reactor at once, and reporting each module
// separately would count the same dependency many times and plan conflicting bumps.
func (m *Ecosystem) Detect(ctx context.Context, root string) ([]ecosystem.Target, error) {
	var roots, standalone []ecosystem.Target

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
			if strings.Count(rel, string(os.PathSeparator)) >= 4 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "pom.xml" {
			return nil
		}
		dir := filepath.Dir(rel)
		if dir == "." {
			dir = "/"
		}
		target := ecosystem.Target{Ecosystem: "maven", Dir: dir, Manifest: path}
		if isReactor(path) {
			roots = append(roots, target)
			// Do not report the modules of a reactor independently.
			return filepath.SkipDir
		}
		standalone = append(standalone, target)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(roots) > 0 {
		sort.SliceStable(roots, func(i, j int) bool { return roots[i].Manifest < roots[j].Manifest })
		return roots, nil
	}
	sort.SliceStable(standalone, func(i, j int) bool { return standalone[i].Manifest < standalone[j].Manifest })
	return standalone, nil
}

// isReactor reports whether a pom declares modules.
func isReactor(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "<modules>")
}

// parseDepLine reads one `mvn dependency:list` line, e.g.
//
//	[INFO]   org.junit.jupiter:junit-jupiter:jar:5.10.0:test
//
// The classifier field is omitted when there is no classifier, so the shape is
// groupId:artifactId:type[:classifier]:version:scope — five or six fields. A fixed
// regex gets this wrong, which is how the first version of this parser silently
// reported zero dependencies.
func parseDepLine(line string) (group, artifact, version, scope string, ok bool) {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "[INFO]")
	line = strings.TrimSpace(line)
	if line == "" || strings.ContainsAny(line, " \t") {
		return "", "", "", "", false
	}
	fields := strings.Split(line, ":")
	switch len(fields) {
	case 5: // group:artifact:type:version:scope
		return fields[0], fields[1], fields[3], fields[4], true
	case 6: // group:artifact:type:classifier:version:scope
		return fields[0], fields[1], fields[4], fields[5], true
	default:
		return "", "", "", "", false
	}
}

// Inventory asks Maven for the resolved graph of the target.
func (m *Ecosystem) Inventory(ctx context.Context, t ecosystem.Target) ([]ecosystem.Dependency, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Dir(t.Manifest)
	if t.Dir == "/" {
		dir = filepath.Dir(t.Manifest)
	}
	// `compile` comes first on purpose. A bare `dependency:list` resolves each
	// module on its own, so a sibling artifact that lives only in the reactor
	// (dev.example:core) is looked for in Central and the build fails. Running a
	// lifecycle phase first puts the siblings in the reactor where they belong.
	//
	// The cost is real: a Maven inventory is a compile, not a file read. That is
	// the price of Maven having no lockfile.
	out, err := m.runner.Run(ctx, dir, "mvn",
		"--batch-mode", "--quiet", "compile", "dependency:list", "-DexcludeTransitive=false")
	if err != nil {
		// A reactor that cannot build (no network, a missing JDK, a broken module)
		// still has declared dependencies worth reporting. Fall back to the poms and
		// accept that property-indirected versions stay unresolved.
		fallback, ferr := m.declaredDependencies(ctx, t)
		if ferr == nil && len(fallback) > 0 {
			return fallback, nil
		}
		return nil, fmt.Errorf("mvn dependency:list in %s: %w", dir, err)
	}

	internal := internalGroupIDs(t.Manifest)
	seen := map[string]bool{}
	var deps []ecosystem.Dependency
	for _, line := range strings.Split(out, "\n") {
		group, artifact, version, scope, ok := parseDepLine(line)
		if !ok {
			continue
		}
		if internal[group] {
			continue // the reactor's own modules are not third-party dependencies
		}
		name := group + ":" + artifact
		if seen[name] || strings.Contains(version, "${") {
			continue
		}
		seen[name] = true
		deps = append(deps, ecosystem.Dependency{
			Name:       name,
			Current:    version,
			Constraint: version,
			Direct:     true,
			Dev:        scope == "test",
			Target:     t.Key(),
		})
	}
	sort.SliceStable(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	return deps, nil
}

// declaredDependencies reads the literal versions declared in a reactor's poms.
//
// It is the degraded path: no property resolution, no dependencyManagement, no
// transitive graph. It exists so a repository that cannot build still reports what
// it declares instead of reporting nothing.
func (m *Ecosystem) declaredDependencies(ctx context.Context, t ecosystem.Target) ([]ecosystem.Dependency, error) {
	root := filepath.Dir(t.Manifest)
	internal := internalGroupIDs(t.Manifest)
	seen := map[string]bool{}
	var deps []ecosystem.Dependency

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "target" || (strings.HasPrefix(d.Name(), ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "pom.xml" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, block := range dependencyRE.FindAllStringSubmatch(string(data), -1) {
			body := block[1]
			g := groupIDRE.FindStringSubmatch(body)
			a := artifactIDRE.FindStringSubmatch(body)
			if g == nil || a == nil {
				continue
			}
			group, artifact := string(g[1]), string(a[1])
			if internal[group] {
				continue
			}
			name := group + ":" + artifact
			if seen[name] {
				continue
			}
			version := ""
			if v := versionRE.FindStringSubmatch(body); v != nil {
				version = string(v[1])
				if strings.HasPrefix(version, "${") {
					version = "" // property-indirected; unresolved without a build
				}
			}
			seen[name] = true
			deps = append(deps, ecosystem.Dependency{
				Name: name, Current: version, Constraint: version,
				Direct: true, Target: t.Key(),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	return deps, nil
}

// internalGroupIDs collects the groupIds declared by poms in this reactor, so the
// project's own modules are not reported as outdated dependencies.
func internalGroupIDs(manifestPath string) map[string]bool {
	groups := map[string]bool{}
	root := filepath.Dir(manifestPath)
	data, err := os.ReadFile(manifestPath)
	if err == nil {
		if g := groupIDRE.FindSubmatch(data); g != nil {
			groups[string(g[1])] = true
		}
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		child, err := os.ReadFile(filepath.Join(root, e.Name(), "pom.xml"))
		if err != nil {
			continue
		}
		if g := groupIDRE.FindSubmatch(child); g != nil {
			groups[string(g[1])] = true
		}
	}
	return groups
}

var groupIDRE = regexp.MustCompile(`<groupId>\s*([^<\s]+)\s*</groupId>`)

// Compare orders Maven versions.
//
// This is a simplified ComparableVersion: numeric segments compare numerically,
// and the qualifiers that show up in practice (alpha, beta, milestone, rc,
// snapshot) sort before a release. Maven's own algorithm has more cases than this;
// the doc comment says so rather than implying exact parity.
func (*Ecosystem) Compare(a, b string) int { return compareVersions(a, b) }

// Classify reports the update distance for a Maven version pair.
func (*Ecosystem) Classify(from, to string) ecosystem.UpdateClass {
	if compareVersions(from, to) == 0 {
		return ecosystem.ClassNone
	}
	// A SNAPSHOT is a pre-release of its series, so moving to or from one is not a
	// semver step.
	if strings.Contains(strings.ToUpper(from), "SNAPSHOT") || strings.Contains(strings.ToUpper(to), "SNAPSHOT") {
		return ecosystem.ClassPatch
	}
	if a, b := normalizeSemver(from), normalizeSemver(to); semver.IsValid(a) && semver.IsValid(b) {
		switch {
		case semver.Major(a) != semver.Major(b):
			return ecosystem.ClassMajor
		case semver.MajorMinor(a) != semver.MajorMinor(b):
			return ecosystem.ClassMinor
		default:
			return ecosystem.ClassPatch
		}
	}
	// Non-semver versions (dates, "RELEASE", qualifiers) are still orderable here,
	// but the distance is not something a semver word can describe honestly.
	return ecosystem.ClassPin
}

// normalizeSemver renders a Maven version as the v-prefixed form x/mod expects.
func normalizeSemver(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// qualifierRank orders the qualifiers Maven treats as pre-release.
//
// Qualifiers carry a trailing number ("rc1", "beta2"); stripping it is what makes a
// release outrank its own candidate, which a first version of this function got
// wrong by comparing only exact words.
func qualifierRank(q string) int {
	q = strings.ToLower(strings.TrimSpace(q))
	q = strings.TrimRightFunc(q, func(r rune) bool { return (r >= '0' && r <= '9') || r == '.' })
	switch q {
	case "alpha", "a":
		return -5
	case "beta", "b":
		return -4
	case "milestone", "m":
		return -3
	case "rc", "cr":
		return -2
	case "snapshot":
		return -1
	case "", "release", "final", "ga":
		return 0
	case "sp":
		return 1
	default:
		return 0
	}
}

// compareVersions implements the ordering the package documents.
func compareVersions(a, b string) int {
	as, bs := splitVersion(a), splitVersion(b)
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				if xn < yn {
					return -1
				}
				return 1
			}
		case xerr == nil:
			// A numeric segment outranks a qualifier: 1.0 > 1.0-rc1.
			if qualifierRank(y) < 0 {
				return 1
			}
			return 1
		case yerr == nil:
			if qualifierRank(x) < 0 {
				return -1
			}
			return -1
		default:
			if r := qualifierRank(x) - qualifierRank(y); r != 0 {
				if r < 0 {
					return -1
				}
				return 1
			}
			if x != y {
				return strings.Compare(x, y)
			}
		}
	}
	return 0
}

// splitVersion breaks a version on its separators.
func splitVersion(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return strings.FieldsFunc(v, func(r rune) bool {
		return r == '.' || r == '-' || r == '_' || r == '+'
	})
}

// Apply rewrites a literal <version> in the pom that declares the dependency.
//
// It declines a version that is property-indirected: the real value lives in a
// <properties> block, possibly in a parent pom, and splicing the element would
// leave two sources of truth. It also declines a dependency whose version is
// managed centrally, because the module's pom does not carry the version at all.
// Both are reported in ApplyResult.Skipped, so a run says what it could not do
// instead of quietly committing part of the plan.
//
// A literal that holds a version other than the one the plan was built against is
// a different matter: the branch is not the tree the plan describes, so the whole
// apply fails with ecosystem.PlanMismatchError (§8).
func (m *Ecosystem) Apply(ctx context.Context, dir string, changes []ecosystem.Change) (ecosystem.ApplyResult, error) {
	res := ecosystem.ApplyResult{Observed: map[string][2]string{}}
	if len(changes) == 0 {
		return res, nil
	}
	_ = ctx

	targets, err := m.Detect(context.Background(), dir)
	if err != nil {
		return res, err
	}

	type pom struct {
		path     string
		original string
		updated  string
	}
	files := make([]pom, 0, len(targets))
	applied, held, declined := map[string]bool{}, map[string]string{}, map[string]string{}

	for _, t := range targets {
		data, err := os.ReadFile(t.Manifest)
		if err != nil {
			return res, err
		}
		text := string(data)
		updated := rewritePom(text, changes, &res, applied, held, declined)
		files = append(files, pom{path: t.Manifest, original: text, updated: updated})
	}

	// A pom that holds a different version than the plan expects means applying
	// would produce a diff nobody reviewed: refuse the whole plan instead (§8).
	// A shape this applier cannot edit is a decline, reported as such, not a
	// mismatch — the plan was right, the applier is limited.
	if mismatches := unappliedArtifacts(changes, applied, held, declined); len(mismatches) > 0 {
		return ecosystem.ApplyResult{Observed: map[string][2]string{}},
			&ecosystem.PlanMismatchError{Where: "pom.xml", Mismatches: mismatches}
	}
	for _, ch := range changes {
		// A coordinate another pom did bump is not declined, even when this one
		// carries only an indirection for it.
		if reason, ok := declined[ch.Name]; ok && !applied[ch.Name] {
			res.Skipped = append(res.Skipped, ecosystem.SkippedChange{Name: ch.Name, Reason: reason})
		}
	}

	for _, f := range files {
		if f.updated == f.original {
			continue
		}
		if err := os.WriteFile(f.path, []byte(f.updated), 0o644); err != nil {
			return res, fmt.Errorf("write %s: %w", f.path, err)
		}
		res.Commands = append(res.Commands, "edit "+filepath.Base(f.path))
	}
	sort.Strings(res.Changed)
	return res, nil
}

// unappliedArtifacts lists the coordinates no pom accepted, in the plan's order.
// A coordinate the applier declined is not listed: the plan is not wrong about it,
// so it is reported through ApplyResult.Skipped instead of failing the apply.
func unappliedArtifacts(changes []ecosystem.Change, applied map[string]bool, held, declined map[string]string) []ecosystem.VersionMismatch {
	var out []ecosystem.VersionMismatch
	for _, ch := range changes {
		if applied[ch.Name] {
			continue
		}
		if _, declined := declined[ch.Name]; declined {
			continue
		}
		out = append(out, ecosystem.VersionMismatch{Name: ch.Name, Want: ch.From, Got: held[ch.Name]})
	}
	return out
}

// dependencyRE matches one <dependency>…</dependency> block.
var dependencyRE = regexp.MustCompile(`(?s)<dependency>(.*?)</dependency>`)

// rewritePom bumps literal versions for the requested coordinates, recording what
// each one was and declining the ones this applier cannot edit safely.
//
// A literal is rewritten only when it still holds the version the plan was built
// against; a different literal goes into held, and the caller refuses the plan
// rather than rewriting a version nobody planned.
func rewritePom(text string, changes []ecosystem.Change, res *ecosystem.ApplyResult, applied map[string]bool, held, declined map[string]string) string {
	want := map[string]ecosystem.Change{}
	for _, ch := range changes {
		want[ch.Name] = ch
	}
	out := dependencyRE.ReplaceAllStringFunc(text, func(block string) string {
		g := groupIDRE.FindStringSubmatch(block)
		a := artifactIDRE.FindStringSubmatch(block)
		if g == nil || a == nil {
			return block
		}
		coord := string(g[1]) + ":" + string(a[1])
		ch, ok := want[coord]
		if !ok {
			return block
		}
		v := versionRE.FindStringSubmatch(block)
		if v == nil {
			// Version is managed centrally; the module's pom has nothing to edit.
			if _, seen := declined[coord]; !seen {
				declined[coord] = "the version is managed centrally, so this pom carries none to edit"
			}
			return block
		}
		current := string(v[1])
		if strings.HasPrefix(current, "${") {
			// Property indirection: refuse rather than create a second source of truth.
			if _, seen := declined[coord]; !seen {
				declined[coord] = "the version is property-indirected (" + current + "); editing this element would create a second source of truth"
			}
			return block
		}
		if current == ch.To {
			// Already where the plan wants it: another pom was rewritten, or the
			// base branch moved. Satisfied, not missing.
			res.Observed[coord] = [2]string{ch.From, ch.To}
			applied[coord] = true
			return block
		}
		if ch.From != "" && current != ch.From {
			if _, seen := held[coord]; !seen {
				held[coord] = current
			}
			return block
		}
		res.Observed[coord] = [2]string{current, ch.To}
		res.Changed = append(res.Changed, coord)
		applied[coord] = true
		return versionRE.ReplaceAllString(block, "<version>"+ch.To+"</version>")
	})
	return out
}

var (
	artifactIDRE = regexp.MustCompile(`<artifactId>\s*([^<\s]+)\s*</artifactId>`)
	versionRE    = regexp.MustCompile(`<version>\s*([^<\s]+)\s*</version>`)
)

// VerifyCommands runs the reactor's tests. Offline flags are the caller's job: the
// sandbox has no network, so the local repository must have been warmed by apply.
func (*Ecosystem) VerifyCommands(ecosystem.Target) [][]string {
	return [][]string{{"mvn", "--batch-mode", "test"}}
}
