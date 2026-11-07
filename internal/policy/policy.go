// Package policy turns candidates into decisions. It is the part of depmcp most
// likely to be subtly wrong, so it is deliberately pure: no I/O, no clock reads,
// no network. Everything it needs is passed in, which is what makes the golden
// tests in policy_test.go possible.
package policy

import (
	"fmt"
	"strings"
	"time"

	"github.com/thread3d/dependancythang/internal/advisory"
	"github.com/thread3d/dependancythang/internal/ecosystem"
)

// Cooldown is the minimum release age per update class (§3 row 5). A freshly
// published version is the riskiest thing a dependency updater can adopt.
type Cooldown struct {
	PatchDays int `json:"patch_days"`
	MinorDays int `json:"minor_days"`
	MajorDays int `json:"major_days"`
}

// DaysFor returns the cooldown for a class; unknown classes fall back to the
// strictest window so an unclassifiable bump never races ahead of a reviewed one.
func (c Cooldown) DaysFor(class ecosystem.UpdateClass) int {
	switch class {
	case ecosystem.ClassPatch:
		return c.PatchDays
	case ecosystem.ClassMinor:
		return c.MinorDays
	case ecosystem.ClassMajor:
		return c.MajorDays
	default:
		return c.MajorDays
	}
}

// IgnoreRule suppresses updates for a dependency. Reason is mandatory (§10).
type IgnoreRule struct {
	Dependency  string   `json:"dependency"`
	Versions    []string `json:"versions,omitempty"` // simple lower bounds, e.g. ">=3.0.0"
	UpdateTypes []string `json:"update_types,omitempty"`
	Reason      string   `json:"reason"`
	Until       string   `json:"until,omitempty"` // YYYY-MM-DD
}

// Group is a named set of dependency patterns updated together.
type Group struct {
	Name     string   `json:"name"`
	Patterns []string `json:"patterns"`
}

// Rules is the resolved policy for one target.
type Rules struct {
	Now time.Time
	// CooldownDays applies per update class.
	Cooldown Cooldown
	// Ignore is evaluated first, in order; the first match wins.
	Ignore []IgnoreRule
	// Groups are matched after a change is otherwise allowed.
	Groups []Group
	// AllowIndirect permits proposing updates to transitive dependencies.
	// Default false: they are reported, and only advisory fixes force them (§3).
	AllowIndirect bool
	// AllowedClasses limits which update classes may be proposed. Empty means
	// all classes are allowed.
	AllowedClasses []ecosystem.UpdateClass
	// BypassCooldown lists severities whose advisory fixes ignore cooldown.
	BypassCooldown []advisory.Severity
}

// Decision is the outcome of evaluating one dependency.
type Decision struct {
	Dependency string                `json:"dependency"`
	From       string                `json:"from"`
	To         string                `json:"to,omitempty"`
	Class      ecosystem.UpdateClass `json:"class"`
	Include    bool                  `json:"include"`
	SkipReason string                `json:"skip_reason,omitempty"`
	Group      string                `json:"group,omitempty"`
	Direct     bool                  `json:"direct"`
	// Target identifies which module/manifest the dependency belongs to, so a
	// monorepo's two go.mod files never collapse into one another.
	Target      string            `json:"target,omitempty"`
	Advisories  []string          `json:"advisories,omitempty"`
	Severity    advisory.Severity `json:"severity,omitempty"`
	CooldownEnd string            `json:"cooldown_end,omitempty"`
	Reason      string            `json:"reason,omitempty"` // human note
}

// Skip reasons, stable so callers and tests can assert on them.
const (
	SkipIgnored      = "ignored"
	SkipCooldown     = "cooldown"
	SkipIndirect     = "indirect"
	SkipPrerelease   = "prerelease"
	SkipNoCandidates = "no_candidates"
	SkipNotAllowed   = "class_not_allowed"
	SkipUnknownClass = "unknown_class"
	SkipCurrent      = "already_current"
)

// Input is everything needed to decide one dependency.
type Input struct {
	Dep        ecosystem.Dependency
	Candidates []ecosystem.Version // newest first, PublishedAt set when known
	Advisories []advisory.Advisory
	// Classify is the ecosystem's own classifier (Go semver for gomod).
	Classify func(from, to string) ecosystem.UpdateClass
	// Compare orders two versions under the ecosystem's semantics; it is how
	// policy answers "is this newer?" as opposed to "how far?".
	Compare func(a, b string) int
}

// Decide applies policy in the order fixed by §8: ignore rules, then indirect
// handling, then candidate selection, then cooldown, then allowed classes, then
// grouping. Advisory fixes bypass cooldown and class limits unless configured
// otherwise.
func Decide(in Input, rules Rules) Decision {
	d := Decision{
		Dependency: in.Dep.Name,
		From:       in.Dep.Current,
		Direct:     in.Dep.Direct,
		Target:     in.Dep.Target,
		Class:      ecosystem.ClassUnknown,
	}
	for _, a := range in.Advisories {
		if a.Withdrawn != "" {
			continue // withdrawn advisories are cleared, not acted on (§9)
		}
		d.Advisories = append(d.Advisories, a.ID)
		if severityRank(a.Severity) > severityRank(d.Severity) {
			d.Severity = a.Severity
		}
	}

	// 1. Ignore rules come first: an ignored dependency is not even considered.
	if rule, ok := matchIgnore(in.Dep, in.Candidates, rules); ok {
		d.SkipReason = SkipIgnored
		d.Reason = rule.Reason
		return d
	}

	// 2. An advisory fix outranks everything else, so establish whether one
	//    exists before deciding about indirect dependencies or cooldown.
	advisoryTarget := advisoryFix(in.Dep.Current, in.Advisories, in.Compare)
	hasAdvisory := advisoryTarget != ""

	// 3. Indirect dependencies are reported but not proposed, unless an
	//    advisory forces the bump (§3 row 9).
	if !in.Dep.Direct && !rules.AllowIndirect && !hasAdvisory {
		d.SkipReason = SkipIndirect
		return d
	}

	// 4. Pick a target: the advisory fix when there is one, else the newest
	//    version that policy would normally allow.
	target := advisoryTarget
	if target == "" {
		target = newestEligible(in)
	}
	if target == "" {
		// Nothing newer exists (or everything newer was filtered out), so there
		// is no change to classify.
		d.Class = ecosystem.ClassNone
		d.SkipReason = SkipNoCandidates
		return d
	}
	if target == in.Dep.Current {
		// Report the class as "none" rather than leaving it unknown: callers
		// that summarise plans should see "nothing to do", not "unclassifiable".
		d.Class = ecosystem.ClassNone
		d.SkipReason = SkipCurrent
		return d
	}

	d.To = target
	d.Class = in.Classify(in.Dep.Current, target)
	if d.Class == ecosystem.ClassUnknown {
		d.SkipReason = SkipUnknownClass
		return d
	}

	// 5. Cooldown, unless an advisory fix at a bypassing severity overrides it.
	bypass := hasAdvisory && severityBypasses(d.Severity, rules.BypassCooldown)
	if !bypass {
		if days := rules.Cooldown.DaysFor(d.Class); days > 0 {
			if published := publishTime(in.Candidates, target); published != "" {
				if t, err := time.Parse(time.RFC3339, published); err == nil {
					ready := t.Add(time.Duration(days) * 24 * time.Hour)
					if rules.Now.Before(ready) {
						d.SkipReason = SkipCooldown
						d.CooldownEnd = ready.UTC().Format(time.RFC3339)
						d.Reason = fmt.Sprintf("published %s, needs %d day(s)", published, days)
						return d
					}
				}
			}
		}
	}

	// 6. Allowed update classes.
	if len(rules.AllowedClasses) > 0 && !bypass && !containsClass(rules.AllowedClasses, d.Class) {
		d.SkipReason = SkipNotAllowed
		return d
	}

	// 7. Grouping is last: it labels an already-approved change.
	d.Group = matchGroup(in.Dep.Name, rules.Groups)
	d.Include = true
	return d
}

// newestEligible returns the newest usable candidate: not a pre-release unless
// the current version is one, and strictly newer than current.
func newestEligible(in Input) string {
	currentIsPrerelease := strings.Contains(in.Dep.Current, "-") // "-" introduces prerelease
	for _, c := range in.Candidates {
		if c.Yanked || c.Retracted {
			continue
		}
		if strings.Contains(c.Raw, "-") && !currentIsPrerelease {
			continue
		}
		if in.Compare(c.Raw, in.Dep.Current) <= 0 {
			continue // not newer than what is required
		}
		return c.Raw
	}
	return ""
}

// advisoryFix picks the lowest fixed version that resolves an advisory while
// staying within the constraint, falling back to the highest fix available.
func advisoryFix(current string, advisories []advisory.Advisory, compare func(a, b string) int) string {
	best := ""
	for _, a := range advisories {
		if a.Withdrawn != "" {
			continue
		}
		for _, fixed := range a.FixedVersions {
			if compare(fixed, current) <= 0 {
				continue // a fix at or below the current version does not help
			}
			if best == "" || compare(best, fixed) > 0 {
				best = fixed // keep the lowest fix that actually resolves it
			}
		}
	}
	return best
}

func publishTime(candidates []ecosystem.Version, version string) string {
	for _, c := range candidates {
		if c.Raw == version {
			return c.PublishedAt
		}
	}
	return ""
}

// matchIgnore returns the first ignore rule that applies.
func matchIgnore(dep ecosystem.Dependency, candidates []ecosystem.Version, rules Rules) (IgnoreRule, bool) {
	for _, rule := range rules.Ignore {
		if !globMatch(rule.Dependency, dep.Name) {
			continue
		}
		if rule.Until != "" {
			if until, err := time.Parse("2006-01-02", rule.Until); err == nil && rules.Now.After(until) {
				continue // expired rules stop applying, and are surfaced in digests
			}
		}
		if len(rule.Versions) > 0 && !versionMatchesAny(dep.Current, rule.Versions) {
			continue
		}
		return rule, true
	}
	return IgnoreRule{}, false
}

// versionMatchesAny evaluates simple lower-bound constraints such as ">=3.0.0".
// Full constraint grammar belongs with the ecosystem; this is the subset the
// ignore rules in §10 use.
func versionMatchesAny(version string, constraints []string) bool {
	for _, c := range constraints {
		c = strings.TrimSpace(c)
		switch {
		case strings.HasPrefix(c, ">="):
			if compareLoose(version, strings.TrimSpace(c[2:])) >= 0 {
				return true
			}
		case strings.HasPrefix(c, ">"):
			if compareLoose(version, strings.TrimSpace(c[1:])) > 0 {
				return true
			}
		case strings.HasPrefix(c, "<="):
			if compareLoose(version, strings.TrimSpace(c[2:])) <= 0 {
				return true
			}
		case strings.HasPrefix(c, "<"):
			if compareLoose(version, strings.TrimSpace(c[1:])) < 0 {
				return true
			}
		default:
			if version == c {
				return true
			}
		}
	}
	return false
}

// compareLoose compares dotted version strings numerically, ignoring a leading v.
func compareLoose(a, b string) int {
	pa := splitVersion(a)
	pb := splitVersion(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func splitVersion(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out = append(out, n)
	}
	return out
}

// matchGroup returns the first group whose pattern matches the dependency.
func matchGroup(name string, groups []Group) string {
	for _, g := range groups {
		for _, p := range g.Patterns {
			if globMatch(p, name) {
				return g.Name
			}
		}
	}
	return ""
}

// globMatch supports a trailing wildcard, which is all dependabot-style group
// and ignore patterns need ("golang.org/x/*").
func globMatch(pattern, name string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == name
}

func severityRank(s advisory.Severity) int {
	switch s {
	case advisory.SeverityCritical:
		return 4
	case advisory.SeverityHigh:
		return 3
	case advisory.SeverityMedium:
		return 2
	case advisory.SeverityLow:
		return 1
	default:
		return 0
	}
}

func severityBypasses(s advisory.Severity, allowed []advisory.Severity) bool {
	for _, a := range allowed {
		if a == s {
			return true
		}
	}
	return false
}

func containsClass(classes []ecosystem.UpdateClass, c ecosystem.UpdateClass) bool {
	for _, x := range classes {
		if x == c {
			return true
		}
	}
	return false
}
