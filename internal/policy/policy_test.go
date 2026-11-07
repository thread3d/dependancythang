package policy_test

import (
	"testing"
	"time"

	"github.com/thread3d/dependancythang/internal/advisory"
	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/ecosystem/gomod"
	"github.com/thread3d/dependancythang/internal/policy"
)

// now is a fixed clock so cooldown arithmetic is deterministic.
var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// day returns an RFC3339 timestamp n days before now.
func day(n int) string { return now.AddDate(0, 0, -n).Format(time.RFC3339) }

// testRules mirrors the specification defaults.
func testRules() policy.Rules {
	return policy.Rules{
		Now:            now,
		Cooldown:       policy.Cooldown{PatchDays: 1, MinorDays: 3, MajorDays: 7},
		BypassCooldown: []advisory.Severity{advisory.SeverityCritical, advisory.SeverityHigh},
	}
}

// decide is the standard entry point for these tests.
func decide(t *testing.T, dep ecosystem.Dependency, candidates []ecosystem.Version, advisories []advisory.Advisory, rules policy.Rules) policy.Decision {
	t.Helper()
	eco := gomod.New()
	return policy.Decide(policy.Input{
		Dep:        dep,
		Candidates: candidates,
		Advisories: advisories,
		Classify:   eco.Classify,
		Compare:    eco.Compare,
	}, rules)
}

func dep(name, current string) ecosystem.Dependency {
	return ecosystem.Dependency{Name: name, Current: current, Direct: true, Target: "gomod:/"}
}

func TestCooldownBlocksFreshVersions(t *testing.T) {
	cases := []struct {
		name      string
		current   string
		candidate string
		published string
		wantInc   bool
		wantSkip  string
	}{
		{"patch older than 1 day is allowed", "v1.2.3", "v1.2.4", day(2), true, ""},
		{"patch from today is too fresh", "v1.2.3", "v1.2.4", day(0), false, policy.SkipCooldown},
		{"minor older than 3 days is allowed", "v1.2.3", "v1.3.0", day(4), true, ""},
		{"minor from 2 days ago is too fresh", "v1.2.3", "v1.3.0", day(2), false, policy.SkipCooldown},
		{"major older than 7 days is allowed", "v1.2.3", "v2.0.0", day(8), true, ""},
		{"major from 6 days ago is too fresh", "v1.2.3", "v2.0.0", day(6), false, policy.SkipCooldown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decide(t, dep("example.com/x", tc.current),
				[]ecosystem.Version{{Raw: tc.candidate, PublishedAt: tc.published}}, nil, testRules())
			if got.Include != tc.wantInc {
				t.Fatalf("include = %v (%s %s), want %v", got.Include, got.SkipReason, got.Reason, tc.wantInc)
			}
			if got.SkipReason != tc.wantSkip {
				t.Errorf("skip reason = %q, want %q", got.SkipReason, tc.wantSkip)
			}
			if tc.wantInc && got.To != tc.candidate {
				t.Errorf("to = %q, want %q", got.To, tc.candidate)
			}
		})
	}
}

func TestClassifiesUpdateDistance(t *testing.T) {
	cases := []struct {
		from, to string
		want     ecosystem.UpdateClass
	}{
		{"v1.2.3", "v1.2.4", ecosystem.ClassPatch},
		{"v1.2.3", "v1.3.0", ecosystem.ClassMinor},
		{"v1.2.3", "v2.0.0", ecosystem.ClassMajor},
		{"v1.2.3", "v1.2.3", ecosystem.ClassNone},
	}
	for _, tc := range cases {
		got := decide(t, dep("example.com/x", tc.from),
			[]ecosystem.Version{{Raw: tc.to, PublishedAt: day(30)}}, nil, testRules())
		if got.Class != tc.want {
			t.Errorf("%s -> %s classified %q, want %q", tc.from, tc.to, got.Class, tc.want)
		}
	}
}

func TestIndirectDependenciesAreNotProposed(t *testing.T) {
	indirect := ecosystem.Dependency{Name: "example.com/x", Current: "v1.0.0", Direct: false}
	got := decide(t, indirect,
		[]ecosystem.Version{{Raw: "v1.1.0", PublishedAt: day(30)}}, nil, testRules())
	if got.Include {
		t.Fatal("indirect dependency was proposed without an advisory")
	}
	if got.SkipReason != policy.SkipIndirect {
		t.Errorf("skip reason = %q, want %q", got.SkipReason, policy.SkipIndirect)
	}
}

func TestIndirectDependencyWithAdvisoryIsProposed(t *testing.T) {
	indirect := ecosystem.Dependency{Name: "example.com/x", Current: "v1.0.0", Direct: false}
	advs := []advisory.Advisory{{
		ID: "GO-2026-0001", Severity: advisory.SeverityHigh, FixedVersions: []string{"v1.0.1"},
	}}
	got := decide(t, indirect,
		[]ecosystem.Version{{Raw: "v1.0.1", PublishedAt: day(0)}}, advs, testRules())
	if !got.Include {
		t.Fatalf("advisory fix on an indirect dependency was not proposed: %s", got.SkipReason)
	}
	if got.To != "v1.0.1" {
		t.Errorf("to = %q, want v1.0.1", got.To)
	}
	if len(got.Advisories) != 1 || got.Advisories[0] != "GO-2026-0001" {
		t.Errorf("advisories = %v, want the driving advisory", got.Advisories)
	}
}

func TestAdvisoryBypassesCooldown(t *testing.T) {
	advs := []advisory.Advisory{{
		ID: "GO-2026-0002", Severity: advisory.SeverityCritical, FixedVersions: []string{"v1.2.4"},
	}}
	// Published today: cooldown would normally block a patch.
	got := decide(t, dep("example.com/x", "v1.2.3"),
		[]ecosystem.Version{{Raw: "v1.2.4", PublishedAt: day(0)}}, advs, testRules())
	if !got.Include {
		t.Fatalf("critical advisory did not bypass cooldown: %s", got.SkipReason)
	}
	if got.To != "v1.2.4" {
		t.Errorf("to = %q, want the fixed version", got.To)
	}
}

func TestLowSeverityAdvisoryDoesNotBypassCooldown(t *testing.T) {
	advs := []advisory.Advisory{{
		ID: "GO-2026-0003", Severity: advisory.SeverityLow, FixedVersions: []string{"v1.2.4"},
	}}
	got := decide(t, dep("example.com/x", "v1.2.3"),
		[]ecosystem.Version{{Raw: "v1.2.4", PublishedAt: day(0)}}, advs, testRules())
	if got.Include {
		t.Fatal("low severity advisory should still respect cooldown")
	}
	if got.SkipReason != policy.SkipCooldown {
		t.Errorf("skip reason = %q, want %q", got.SkipReason, policy.SkipCooldown)
	}
}

func TestWithdrawnAdvisoryIsIgnored(t *testing.T) {
	advs := []advisory.Advisory{{
		ID: "GO-2026-0004", Severity: advisory.SeverityCritical,
		FixedVersions: []string{"v1.2.4"}, Withdrawn: day(1),
	}}
	got := decide(t, dep("example.com/x", "v1.2.3"),
		[]ecosystem.Version{{Raw: "v1.2.4", PublishedAt: day(0)}}, advs, testRules())
	if got.Include {
		t.Fatal("a withdrawn advisory must not drive an update")
	}
	if len(got.Advisories) != 0 {
		t.Errorf("withdrawn advisory was still reported: %v", got.Advisories)
	}
}

func TestIgnoreRuleSuppressesEverything(t *testing.T) {
	rules := testRules()
	rules.Ignore = []policy.IgnoreRule{{
		Dependency: "example.com/*",
		Reason:     "v3 drops our Go version",
	}}
	got := decide(t, dep("example.com/x", "v1.0.0"),
		[]ecosystem.Version{{Raw: "v3.0.0", PublishedAt: day(60)}}, nil, rules)
	if got.Include {
		t.Fatal("ignored dependency was proposed")
	}
	if got.SkipReason != policy.SkipIgnored {
		t.Errorf("skip reason = %q, want %q", got.SkipReason, policy.SkipIgnored)
	}
	if got.Reason != "v3 drops our Go version" {
		t.Errorf("reason = %q, want the rule's reason surfaced", got.Reason)
	}
}

func TestExpiredIgnoreRuleNoLongerApplies(t *testing.T) {
	rules := testRules()
	rules.Ignore = []policy.IgnoreRule{{
		Dependency: "example.com/x",
		Reason:     "temporary hold",
		Until:      now.AddDate(0, 0, -1).Format("2006-01-02"),
	}}
	got := decide(t, dep("example.com/x", "v1.0.0"),
		[]ecosystem.Version{{Raw: "v1.1.0", PublishedAt: day(30)}}, nil, rules)
	if !got.Include {
		t.Fatalf("expired ignore rule still blocked the update: %s", got.SkipReason)
	}
}

func TestVersionRangeIgnoreRule(t *testing.T) {
	rules := testRules()
	rules.Ignore = []policy.IgnoreRule{{
		Dependency: "example.com/x",
		Versions:   []string{">=3.0.0"},
		Reason:     "v3 is not adopted yet",
	}}
	// Current version is above the threshold, so the rule applies.
	got := decide(t, dep("example.com/x", "v3.1.0"),
		[]ecosystem.Version{{Raw: "v3.2.0", PublishedAt: day(30)}}, nil, rules)
	if got.SkipReason != policy.SkipIgnored {
		t.Errorf("skip reason = %q, want %q", got.SkipReason, policy.SkipIgnored)
	}
	// Current version is below it, so the rule does not.
	got = decide(t, dep("example.com/y", "v1.0.0"),
		[]ecosystem.Version{{Raw: "v1.1.0", PublishedAt: day(30)}}, nil, rules)
	if !got.Include {
		t.Errorf("rule for example.com/x suppressed example.com/y: %s", got.SkipReason)
	}
}

func TestPrereleaseSkippedUnlessCurrentIsPrerelease(t *testing.T) {
	stable := []ecosystem.Version{
		{Raw: "v1.3.0-rc1", PublishedAt: day(30)},
		{Raw: "v1.2.4", PublishedAt: day(30)},
	}
	got := decide(t, dep("example.com/x", "v1.2.3"), stable, nil, testRules())
	if got.To != "v1.2.4" {
		t.Errorf("to = %q, want the newest stable v1.2.4", got.To)
	}

	pre := decide(t, dep("example.com/x", "v1.3.0-rc0"), stable, nil, testRules())
	if pre.To != "v1.3.0-rc1" {
		t.Errorf("to = %q, want the newer prerelease v1.3.0-rc1", pre.To)
	}
}

func TestGroupingIsAssignedAfterApproval(t *testing.T) {
	rules := testRules()
	rules.Groups = []policy.Group{
		{Name: "golang-x", Patterns: []string{"golang.org/x/*"}},
	}
	got := decide(t, dep("golang.org/x/text", "v0.3.0"),
		[]ecosystem.Version{{Raw: "v0.4.0", PublishedAt: day(30)}}, nil, rules)
	if !got.Include {
		t.Fatalf("expected inclusion: %s", got.SkipReason)
	}
	if got.Group != "golang-x" {
		t.Errorf("group = %q, want golang-x", got.Group)
	}
}

func TestAlreadyCurrentAndNoCandidates(t *testing.T) {
	got := decide(t, dep("example.com/x", "v1.2.3"),
		[]ecosystem.Version{{Raw: "v1.2.3", PublishedAt: day(30)}}, nil, testRules())
	if got.SkipReason != policy.SkipNoCandidates {
		t.Errorf("skip reason = %q, want %q", got.SkipReason, policy.SkipNoCandidates)
	}
}

func TestClassNotAllowed(t *testing.T) {
	rules := testRules()
	rules.Cooldown = policy.Cooldown{} // cooldown off, so class is the only gate
	rules.AllowedClasses = []ecosystem.UpdateClass{ecosystem.ClassPatch, ecosystem.ClassMinor}
	got := decide(t, dep("example.com/x", "v1.0.0"),
		[]ecosystem.Version{{Raw: "v2.0.0", PublishedAt: day(30)}}, nil, rules)
	if got.Include {
		t.Fatal("major update admitted despite being disallowed")
	}
	if got.SkipReason != policy.SkipNotAllowed {
		t.Errorf("skip reason = %q, want %q", got.SkipReason, policy.SkipNotAllowed)
	}
}
