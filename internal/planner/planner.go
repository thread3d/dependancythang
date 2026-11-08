// Package planner turns policy decisions into an immutable UpdatePlan (§8).
//
// A plan is a hypothesis about the registry at a point in time, so it records
// everything needed to re-validate it later: the target versions chosen, the
// classes, and the advisories driving each change.
package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/thread3d/dependancythang/internal/advisory"
	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/policy"
)

// Change is one proposed update.
type Change struct {
	Dependency string                `json:"dependency"`
	From       string                `json:"from"`
	To         string                `json:"to"`
	Class      ecosystem.UpdateClass `json:"class"`
	Direct     bool                  `json:"direct"`
	Target     string                `json:"target,omitempty"`
	// Digest carries a pinned secondary identity (image digest, commit SHA).
	Digest     string            `json:"digest,omitempty"`
	Advisories []string          `json:"advisories,omitempty"`
	Severity   advisory.Severity `json:"severity,omitempty"`
}

// Deferred is a change policy declined to make now, with the reason.
type Deferred struct {
	Dependency string `json:"dependency"`
	To         string `json:"to,omitempty"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail,omitempty"`
}

// Group is a set of changes applied together on one branch.
type Group struct {
	Name      string     `json:"name"`
	Ecosystem string     `json:"ecosystem"`
	Target    string     `json:"target"`
	Changes   []Change   `json:"changes"`
	Deferred  []Deferred `json:"deferred,omitempty"`
}

// Plan is an immutable update plan.
type Plan struct {
	PlanID    string     `json:"plan_id"`
	CreatedAt string     `json:"created_at"`
	RepoID    string     `json:"repo_id,omitempty"`
	Ecosystem string     `json:"ecosystem"`
	Groups    []Group    `json:"groups"`
	Deferred  []Deferred `json:"deferred,omitempty"`
	// Summary counts, so a client can report without walking every change.
	TotalChanges  int `json:"total_changes"`
	TotalDeferred int `json:"total_deferred"`
}

// DefaultGroupName labels changes that match no configured group.
const DefaultGroupName = "default"

// Build assembles a plan from decisions. The plan id is a digest of its content,
// so identical inputs produce an identical plan and re-planning is idempotent.
func Build(decisions []policy.Decision, ecosystemName, repoID string, now time.Time) Plan {
	groups := map[string]*Group{}
	p := Plan{
		CreatedAt: now.UTC().Format(time.RFC3339),
		RepoID:    repoID,
		Ecosystem: ecosystemName,
	}

	for _, d := range decisions {
		if !d.Include {
			// Deferral is for things a human should look at: a real candidate
			// held back by policy. Skipping a transitive dependency or finding no
			// candidates is not news, and listing it buries the signal.
			switch d.SkipReason {
			case policy.SkipCooldown, policy.SkipNotAllowed, policy.SkipIgnored, policy.SkipUnknownClass:
				p.Deferred = append(p.Deferred, Deferred{
					Dependency: d.Dependency,
					To:         d.To,
					Reason:     d.SkipReason,
					Detail:     d.Reason,
				})
			}
			continue
		}
		name := d.Group
		if name == "" {
			name = DefaultGroupName
		}
		// Group per target: two go.mod files in one repository are separate
		// units of work even when they share a group name.
		key := d.Target + "|" + name
		g, ok := groups[key]
		if !ok {
			eco := d.Ecosystem
			if eco == "" {
				eco = ecosystemName
			}
			g = &Group{Name: name, Ecosystem: eco, Target: d.Target}
			groups[key] = g
		}
		g.Changes = append(g.Changes, Change{
			Dependency: d.Dependency,
			From:       d.From,
			To:         d.To,
			Class:      d.Class,
			Direct:     d.Direct,
			Target:     d.Target,
			Digest:     d.Digest,
			Advisories: d.Advisories,
			Severity:   d.Severity,
		})
	}

	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		g := groups[name]
		sort.SliceStable(g.Changes, func(i, j int) bool { return g.Changes[i].Dependency < g.Changes[j].Dependency })
		p.Groups = append(p.Groups, *g)
		p.TotalChanges += len(g.Changes)
	}
	sort.SliceStable(p.Deferred, func(i, j int) bool { return p.Deferred[i].Dependency < p.Deferred[j].Dependency })
	p.TotalDeferred = len(p.Deferred)
	p.PlanID = digest(p)
	return p
}

// digest derives the plan id from the plan's substance, excluding the id itself.
func digest(p Plan) string {
	clone := p
	clone.PlanID = ""
	clone.CreatedAt = ""
	data, err := json.Marshal(clone)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// SetTarget records which target a plan belongs to, then re-derives its id.
func (p *Plan) SetTarget(target string) {
	for i := range p.Groups {
		p.Groups[i].Target = target
	}
	p.PlanID = digest(*p)
}

// Validate re-checks a stored plan against the world as it is now (§8 TOCTOU).
// It returns the reasons the plan can no longer be applied, or nil when it is
// still sound. Actual re-resolution happens in M2; this covers the checks that
// need no network.
func Validate(p Plan, now time.Time) []error {
	var problems []error
	if p.PlanID == "" {
		problems = append(problems, fmt.Errorf("plan has no id"))
	}
	// A change is identified by the file it edits as well as by the dependency:
	// two workflow files can use the same action at the same ref, and two go.mod
	// files can need the same module at the same version. Those are two units of
	// work in two files — the way Build already groups them — not a duplicate
	// change, so the target is part of the key.
	seen := map[string]bool{}
	for _, g := range p.Groups {
		for _, c := range g.Changes {
			target := c.Target
			if target == "" {
				target = g.Target
			}
			key := target + "|" + c.Dependency + "@" + c.To
			if seen[key] {
				label := c.Dependency + "@" + c.To
				if target != "" {
					label += " in " + target
				}
				problems = append(problems, fmt.Errorf("duplicate change for %s", label))
			}
			seen[key] = true
			if c.From == c.To {
				problems = append(problems, fmt.Errorf("%s: no-op change (%s)", c.Dependency, c.From))
			}
			if c.Class == ecosystem.ClassUnknown {
				problems = append(problems, fmt.Errorf("%s: unknown update class", c.Dependency))
			}
		}
	}
	return problems
}
