// Package semverx provides version ordering and classification for ecosystems
// whose versions are semver but not Go's "v"-prefixed dialect — npm, pnpm, and
// the semver-looking subset of Docker tags.
//
// It reuses golang.org/x/mod/semver after normalizing the prefix, so ordering
// rules (including prerelease precedence) match a well-tested implementation
// instead of a hand-rolled one.
package semverx

import (
	"strings"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"golang.org/x/mod/semver"
)

// Normalize converts an ecosystem version to the "v"-prefixed form x/mod/semver
// expects. It returns "" for anything that is not semver.
func Normalize(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	// Ranges and tags are not versions.
	if strings.ContainsAny(v, "^*~<>|=, ") || strings.Contains(v, "||") {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	// x/mod/semver accepts vMAJOR[.MINOR[.PATCH]][-pre][+build]; pad shorthand
	// like "v1.2" so it parses.
	core, rest := v, ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		core, rest = v[:i], v[i:]
	}
	if strings.Count(core, ".") == 0 {
		core += ".0.0"
	} else if strings.Count(core, ".") == 1 {
		core += ".0"
	}
	v = core + rest
	if !semver.IsValid(v) {
		return ""
	}
	return v
}

// Compare orders two versions. Unparseable versions sort below parseable ones so
// a tag such as "latest" never wins on accident.
func Compare(a, b string) int {
	na, nb := Normalize(a), Normalize(b)
	switch {
	case na != "" && nb != "":
		return semver.Compare(na, nb)
	case na != "":
		return 1
	case nb != "":
		return -1
	default:
		return strings.Compare(a, b)
	}
}

// Classify reports the semver distance between two versions.
func Classify(from, to string) ecosystem.UpdateClass {
	nf, nt := Normalize(from), Normalize(to)
	if nf == "" || nt == "" {
		return ecosystem.ClassUnknown
	}
	if semver.Compare(nf, nt) == 0 {
		return ecosystem.ClassNone
	}
	switch {
	case semver.Major(nf) != semver.Major(nt):
		return ecosystem.ClassMajor
	case semver.MajorMinor(nf) != semver.MajorMinor(nt):
		return ecosystem.ClassMinor
	default:
		return ecosystem.ClassPatch
	}
}

// IsPrerelease reports whether v has a prerelease component.
func IsPrerelease(v string) bool {
	n := Normalize(v)
	return n != "" && semver.Prerelease(n) != ""
}
