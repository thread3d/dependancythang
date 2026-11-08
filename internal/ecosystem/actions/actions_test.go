package actions_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/ecosystem/actions"
)

// seedWorkflow writes one workflow file and returns the repository root.
func seedWorkflow(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(content), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return root
}

func readWorkflow(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(data)
}

// TestWorkflowGolden is the §6 matrix row for GitHub Actions.
func TestWorkflowGolden(t *testing.T) {
	const before = `name: ci
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: stable
      - uses: ./local-action
      - uses: docker://alpine:3.20
`
	const want = `name: ci
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version: stable
      - uses: ./local-action
      - uses: docker://alpine:3.20
`
	root := seedWorkflow(t, before)
	res, err := actions.New().Apply(context.Background(), root, []ecosystem.Change{
		{Name: "actions/checkout", From: "v4", To: "v5"},
		{Name: "actions/setup-go", From: "v5", To: "v6"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readWorkflow(t, root); got != want {
		t.Errorf("workflow diff mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if len(res.Changed) != 2 {
		t.Errorf("changed = %v, want two actions", res.Changed)
	}
}

// TestWorkflowSHAPinPreservesVersionComment covers the hardened form: an
// immutable SHA with the human-readable tag kept as a comment.
func TestWorkflowSHAPinPreservesVersionComment(t *testing.T) {
	const before = `jobs:
  build:
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5 # pinned
`
	const want = `jobs:
  build:
    steps:
      - uses: actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683 # v4.2.2
      - uses: actions/setup-go@0aaccfd150d50ccaeb58ebd88d36e91967a5f35b # v5.4.0
`
	root := seedWorkflow(t, before)
	_, err := actions.New().Apply(context.Background(), root, []ecosystem.Change{
		{Name: "actions/checkout", From: "v4", To: "v4.2.2", Digest: "11bd71901bbe5b1630ceea73d27597364c9af683"},
		{Name: "actions/setup-go", From: "v5", To: "v5.4.0", Digest: "0aaccfd150d50ccaeb58ebd88d36e91967a5f35b"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readWorkflow(t, root); got != want {
		t.Errorf("sha pin diff mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestInventorySkipsLocalAndDockerRefs(t *testing.T) {
	root := seedWorkflow(t, `jobs:
  a:
    steps:
      - uses: actions/checkout@v4
      - uses: actions/checkout@v4
      - uses: ./reusable.yml
      - uses: docker://ghcr.io/owner/img:1.0
      - uses: owner/repo/sub/path@v2
`)
	targets, err := actions.New().Detect(context.Background(), root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	deps, err := actions.New().Inventory(context.Background(), targets[0])
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Current
	}
	want := map[string]string{
		"actions/checkout":    "v4",
		"owner/repo/sub/path": "v2",
	}
	if len(got) != len(want) {
		t.Fatalf("deps = %v, want %v", got, want)
	}
	for name, ref := range want {
		if got[name] != ref {
			t.Errorf("%s = %q, want %q", name, got[name], ref)
		}
	}
}

func TestNoWorkflowsIsNotAnError(t *testing.T) {
	targets, err := actions.New().Detect(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Detect on a repository without workflows: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("targets = %d, want 0", len(targets))
	}
}

func TestClassifyRefs(t *testing.T) {
	eco := actions.New()
	cases := []struct {
		from, to string
		want     ecosystem.UpdateClass
	}{
		{"v4", "v5", ecosystem.ClassMajor},
		{"v4.1.0", "v4.2.0", ecosystem.ClassMinor},
		{"v4", "main", ecosystem.ClassPin},
		{"v4", "11bd71901bbe5b1630ceea73d27597364c9af683", ecosystem.ClassDigest},
		{"v4", "v4", ecosystem.ClassNone},
	}
	for _, tc := range cases {
		if got := eco.Classify(tc.from, tc.to); got != tc.want {
			t.Errorf("Classify(%q,%q) = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

// seedWorkflows writes one workflow file per name and returns the repository root.
func seedWorkflows(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return root
}

func readWorkflowFile(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// TestApplyHonoursThePlannedFromRef is the regression for DEP-D516B032: the
// applier rewrote any ref it found, so a plan built from another tree produced a
// diff nobody reviewed and a record of a change that never happened.
func TestApplyHonoursThePlannedFromRef(t *testing.T) {
	const planned = `jobs:
  build:
    steps:
      - uses: actions/checkout@v4
`
	const drifted = `jobs:
  build:
    steps:
      - uses: actions/checkout@v5
`
	root := seedWorkflows(t, map[string]string{"ci.yml": planned, "lean.yml": drifted})

	res, err := actions.New().Apply(context.Background(), root, []ecosystem.Change{
		{Name: "actions/checkout", From: "v4", To: "v5"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readWorkflowFile(t, root, "ci.yml"); !strings.Contains(got, "actions/checkout@v5") {
		t.Errorf("the planned line was not bumped:\n%s", got)
	}
	if got := readWorkflowFile(t, root, "lean.yml"); got != drifted {
		t.Errorf("a line the plan did not predict was rewritten:\n%s", got)
	}
	if len(res.Changed) != 1 || res.Changed[0] != "actions/checkout" {
		t.Errorf("changed = %v, want one", res.Changed)
	}
	if got := res.Observed["actions/checkout"]; got != [2]string{"v4", "v5"} {
		t.Errorf("observed = %v, want v4 -> v5", got)
	}
}

// TestApplyRefusesARefThePlanDidNotPredict covers the reported shape: the tree
// holds a different ref than the plan was built against, so nothing is written and
// the caller learns what the branch actually has.
func TestApplyRefusesARefThePlanDidNotPredict(t *testing.T) {
	const body = `jobs:
  build:
    steps:
      - uses: actions/checkout@v7
`
	root := seedWorkflows(t, map[string]string{"ci.yml": body})

	res, err := actions.New().Apply(context.Background(), root, []ecosystem.Change{
		{Name: "actions/checkout", From: "v4", To: "v7.0.1"},
	})
	if err == nil {
		t.Fatal("a drifted ref was rewritten")
	}
	mismatch, ok := ecosystem.PlanMismatch(err)
	if !ok {
		t.Fatalf("error is not a PlanMismatchError: %v", err)
	}
	if len(mismatch.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want one", mismatch.Mismatches)
	}
	if got := mismatch.Mismatches[0]; got.Name != "actions/checkout" || got.Want != "v4" || got.Got != "v7" {
		t.Errorf("mismatch = %+v, want actions/checkout v7 where v4 was planned", got)
	}
	if got := readWorkflowFile(t, root, "ci.yml"); got != body {
		t.Errorf("the workflow was written despite the refusal:\n%s", got)
	}
	if len(res.Observed) != 0 {
		t.Errorf("observed = %v, want nothing reported", res.Observed)
	}
}

// TestApplyTreatsTheTargetRefAsApplied keeps the applier idempotent: when the base
// branch already holds the planned ref, the change is satisfied rather than
// missing, which is what lets one plan cover two workflow files.
func TestApplyTreatsTheTargetRefAsApplied(t *testing.T) {
	const body = `jobs:
  build:
    steps:
      - uses: actions/checkout@v5
`
	root := seedWorkflows(t, map[string]string{"ci.yml": body})

	res, err := actions.New().Apply(context.Background(), root, []ecosystem.Change{
		{Name: "actions/checkout", From: "v4", To: "v5"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readWorkflowFile(t, root, "ci.yml"); got != body {
		t.Errorf("an already-planned ref was rewritten:\n%s", got)
	}
	if got := res.Observed["actions/checkout"]; got != [2]string{"v4", "v5"} {
		t.Errorf("observed = %v, want the change reported as satisfied", got)
	}
	if len(res.Changed) != 0 {
		t.Errorf("changed = %v, want none (nothing moved in this call)", res.Changed)
	}
}
