// Package actions implements the ecosystem interface for GitHub Actions
// references in workflow files.
//
// An Action ref is a tag, a branch, or a commit SHA — never a version triple in
// general — so references are classified as pins, and "hardening" a workflow
// means replacing a mutable tag with a SHA while keeping the tag as a comment.
package actions

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
)

// Name is the configuration identifier.
const Name = "actions"

// Ecosystem is the GitHub Actions implementation.
type Ecosystem struct{}

// New returns the Actions ecosystem.
func New() *Ecosystem { return &Ecosystem{} }

// Name implements ecosystem.Ecosystem.
func (*Ecosystem) Name() string { return Name }

// usesRE matches a `uses:` entry, capturing indentation, the action, the ref and
// any trailing comment.
var usesRE = regexp.MustCompile(`^(\s*(?:-\s+)?uses:\s*)(["']?)([^@"'\s]+)@([^"'\s]+)(["']?)(\s*(?:#.*)?)$`)

// Detect finds workflow files.
func (e *Ecosystem) Detect(ctx context.Context, root string) ([]ecosystem.Target, error) {
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // a repository without workflows is not an error
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var targets []ecosystem.Target
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() {
			continue
		}
		name := strings.ToLower(entry.Name())
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		targets = append(targets, ecosystem.Target{
			Ecosystem: Name,
			Dir:       "/.github/workflows",
			Manifest:  filepath.Join(dir, entry.Name()),
			Module:    entry.Name(),
		})
	}
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].Manifest < targets[j].Manifest })
	return targets, nil
}

// use is one parsed `uses:` entry.
type use struct {
	// Action is "owner/repo".
	Action string
	Ref    string
	// Comment is the trailing comment, usually the human-readable version.
	Comment string
}

// parseUses extracts a uses entry from a line.
func parseUses(line string) (use, bool) {
	m := usesRE.FindStringSubmatch(line)
	if m == nil {
		return use{}, false
	}
	action, ref := m[3], m[4]
	// Local actions and Docker references are not updatable: their "ref" is not
	// a published version at all.
	if strings.HasPrefix(action, "./") || strings.HasPrefix(action, "docker://") {
		return use{}, false
	}
	var comment string
	if i := strings.Index(m[6], "#"); i >= 0 {
		comment = strings.TrimSpace(m[6][i+1:])
	}
	return use{Action: action, Ref: ref, Comment: comment}, true
}

// Inventory lists action references.
func (e *Ecosystem) Inventory(ctx context.Context, t ecosystem.Target) ([]ecosystem.Dependency, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(t.Manifest)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.Manifest, err)
	}
	var deps []ecosystem.Dependency
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		u, ok := parseUses(line)
		if !ok || seen[u.Action] {
			continue // one entry per action, however many jobs use it
		}
		seen[u.Action] = true
		deps = append(deps, ecosystem.Dependency{
			Name: u.Action, Current: u.Ref, Constraint: u.Action + "@" + u.Ref,
			Direct: true, Target: t.Key(),
		})
	}
	sort.SliceStable(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	return deps, nil
}

// Apply rewrites refs in every workflow file.
//
// When a change carries a digest (a commit SHA) the reference is rewritten to the
// SHA and the previous tag is kept as a trailing comment, which is the hardened
// form: immutable pin, human-readable version.
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
		lines := strings.Split(original, "\n")
		for i, line := range lines {
			m := usesRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			ch, ok := want[m[3]]
			if !ok {
				continue
			}
			next := ch.To
			if ch.Digest != "" {
				next = ch.Digest
			}
			comment := ""
			if c := strings.TrimSpace(m[6]); strings.HasPrefix(c, "#") {
				comment = " " + strings.TrimSpace(c)
			}
			if ch.Digest != "" {
				// Pin to the SHA, keep the tag visible.
				label := ch.To
				if label == "" {
					label = ch.From
				}
				comment = " # " + label
			}
			lines[i] = m[1] + m[2] + m[3] + "@" + next + m[5] + comment
			res.Observed[m[3]] = [2]string{m[4], next}
			res.Changed = append(res.Changed, m[3])
		}
		updated := strings.Join(lines, "\n")
		if updated != original {
			if err := os.WriteFile(t.Manifest, []byte(updated), 0o644); err != nil {
				return res, fmt.Errorf("write %s: %w", t.Manifest, err)
			}
		}
	}
	sort.Strings(res.Changed)
	return res, nil
}

// Compare orders refs: semver tags numerically, everything else as strings so
// behavior stays deterministic.
func (*Ecosystem) Compare(a, b string) int { return semverx.Compare(a, b) }

// Classify reports a ref change as a pin (or a digest when moving to a SHA).
func (*Ecosystem) Classify(from, to string) ecosystem.UpdateClass {
	if from == to {
		return ecosystem.ClassNone
	}
	if isSHA(to) || isSHA(from) {
		return ecosystem.ClassDigest
	}
	if class := semverx.Classify(from, to); class != ecosystem.ClassUnknown {
		return class
	}
	return ecosystem.ClassPin
}

// isSHA reports whether a ref looks like a commit SHA.
func isSHA(ref string) bool {
	if len(ref) < 7 || len(ref) > 40 {
		return false
	}
	for _, r := range ref {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// VerifyCommands: a workflow file has nothing to build locally.
func (*Ecosystem) VerifyCommands(ecosystem.Target) [][]string { return nil }
