package planner_test

import (
	"testing"
	"time"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/planner"
	"github.com/thread3d/dependancythang/internal/policy"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func decision(name, from, to string, group string) policy.Decision {
	return policy.Decision{
		Dependency: name, From: from, To: to,
		Class: ecosystem.ClassPatch, Include: true, Group: group, Direct: true,
	}
}

func TestBuildGroupsChangesAndCounts(t *testing.T) {
	decisions := []policy.Decision{
		decision("golang.org/x/text", "v0.13.0", "v0.14.0", "golang-x"),
		decision("golang.org/x/sync", "v0.5.0", "v0.6.0", "golang-x"),
		decision("github.com/pkg/errors", "v0.9.0", "v0.9.1", ""),
		{
			Dependency: "example.com/deferred", From: "v1.0.0", To: "v2.0.0",
			Class: ecosystem.ClassMajor, Include: false,
			SkipReason: policy.SkipCooldown, Reason: "published today",
		},
	}
	p := planner.Build(decisions, "gomod", "repo-1", now)

	if p.TotalChanges != 3 {
		t.Errorf("total changes = %d, want 3", p.TotalChanges)
	}
	if p.TotalDeferred != 1 {
		t.Errorf("total deferred = %d, want 1", p.TotalDeferred)
	}
	if len(p.Groups) != 2 {
		t.Fatalf("groups = %d, want 2 (golang-x and default)", len(p.Groups))
	}
	if p.Groups[0].Name != "default" || p.Groups[1].Name != "golang-x" {
		t.Errorf("group names = %q,%q, want default,golang-x (sorted)",
			p.Groups[0].Name, p.Groups[1].Name)
	}
	if len(p.Groups[1].Changes) != 2 {
		t.Errorf("golang-x changes = %d, want 2", len(p.Groups[1].Changes))
	}
	if p.Deferred[0].Reason != policy.SkipCooldown {
		t.Errorf("deferred reason = %q, want %q", p.Deferred[0].Reason, policy.SkipCooldown)
	}
}

func TestPlanIDIsDeterministic(t *testing.T) {
	decisions := []policy.Decision{
		decision("golang.org/x/text", "v0.13.0", "v0.14.0", "golang-x"),
	}
	a := planner.Build(decisions, "gomod", "repo-1", now)
	b := planner.Build(decisions, "gomod", "repo-1", now.Add(time.Hour))
	if a.PlanID != b.PlanID {
		t.Errorf("plan id depends on wall clock: %q vs %q", a.PlanID, b.PlanID)
	}
	if a.PlanID == "" {
		t.Error("plan id is empty")
	}

	// A different change must produce a different id.
	other := planner.Build([]policy.Decision{
		decision("golang.org/x/text", "v0.13.0", "v0.15.0", "golang-x"),
	}, "gomod", "repo-1", now)
	if other.PlanID == a.PlanID {
		t.Error("different changes produced the same plan id")
	}
}

func TestValidateCatchesUnsoundPlans(t *testing.T) {
	bad := planner.Plan{
		PlanID: "abc",
		Groups: []planner.Group{{
			Name: "default",
			Changes: []planner.Change{
				{Dependency: "a", From: "v1.0.0", To: "v1.0.0", Class: ecosystem.ClassPatch},
				{Dependency: "b", From: "v1.0.0", To: "v1.1.0", Class: ecosystem.ClassUnknown},
			},
		}},
	}
	problems := planner.Validate(bad, now)
	if len(problems) != 2 {
		t.Fatalf("problems = %d (%v), want 2 (no-op change and unknown class)", len(problems), problems)
	}

	good := planner.Plan{
		PlanID: "abc",
		Groups: []planner.Group{{
			Name:    "default",
			Changes: []planner.Change{{Dependency: "a", From: "v1.0.0", To: "v1.1.0", Class: ecosystem.ClassMinor}},
		}},
	}
	if problems := planner.Validate(good, now); len(problems) != 0 {
		t.Errorf("sound plan reported problems: %v", problems)
	}
}
