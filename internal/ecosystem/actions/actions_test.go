package actions_test

import (
	"context"
	"os"
	"path/filepath"
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
