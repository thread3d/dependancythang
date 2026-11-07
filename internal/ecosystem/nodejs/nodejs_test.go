package nodejs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/ecosystem/nodejs"
)

// seed writes files into a temp directory and returns its path.
func seed(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return dir
}

const packageJSON = `{
  "name": "example-app",
  "dependencies": {"left-pad": "^1.3.0", "lodash": "^4.17.20"},
  "devDependencies": {"typescript": "^5.3.0"}
}
`

// The three package-lock formats in the §6 matrix, each resolving the same
// requirements to the same versions through a different shape.
var lockfixtures = map[string]string{
	"v1 (dependencies only)": `{
  "name": "example-app",
  "lockfileVersion": 1,
  "dependencies": {
    "left-pad": {"version": "1.3.0"},
    "lodash": {"version": "4.17.21"},
    "typescript": {"version": "5.3.3"}
  }
}`,
	"v2 (dependencies + packages)": `{
  "name": "example-app",
  "lockfileVersion": 2,
  "packages": {
    "": {"name": "example-app"},
    "node_modules/left-pad": {"version": "1.3.0"},
    "node_modules/lodash": {"version": "4.17.21"},
    "node_modules/typescript": {"version": "5.3.3"}
  },
  "dependencies": {
    "left-pad": {"version": "1.3.0"},
    "lodash": {"version": "4.17.21"}
  }
}`,
	"v3 (packages only)": `{
  "name": "example-app",
  "lockfileVersion": 3,
  "packages": {
    "": {"name": "example-app"},
    "node_modules/left-pad": {"version": "1.3.0"},
    "node_modules/lodash": {"version": "4.17.21"},
    "node_modules/typescript": {"version": "5.3.3"},
    "node_modules/left-pad/node_modules/nested": {"version": "9.9.9"}
  }
}`,
}

func TestNpmInventoryAcrossLockfileFormats(t *testing.T) {
	for name, lock := range lockfixtures {
		t.Run(name, func(t *testing.T) {
			dir := seed(t, map[string]string{
				"package.json":      packageJSON,
				"package-lock.json": lock,
			})
			eco := nodejs.NewNpm()
			targets, err := eco.Detect(context.Background(), dir)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if len(targets) != 1 {
				t.Fatalf("targets = %d, want 1", len(targets))
			}
			deps, err := eco.Inventory(context.Background(), targets[0])
			if err != nil {
				t.Fatalf("Inventory: %v", err)
			}
			got := map[string]ecosystem.Dependency{}
			for _, d := range deps {
				got[d.Name] = d
			}
			if len(got) != 3 {
				t.Fatalf("deps = %v, want left-pad, lodash, typescript", got)
			}
			if got["lodash"].Current != "4.17.21" {
				t.Errorf("lodash = %q, want 4.17.21 from the lockfile", got["lodash"].Current)
			}
			if got["lodash"].Constraint != "^4.17.20" {
				t.Errorf("lodash constraint = %q, want the declared range", got["lodash"].Constraint)
			}
			if !got["typescript"].Dev {
				t.Error("typescript should be marked as a dev dependency")
			}
			if got["lodash"].Dev {
				t.Error("lodash is a runtime dependency")
			}
			// A nested transitive copy must not shadow the top-level version.
			if v, ok := got["nested"]; ok {
				t.Errorf("nested transitive entry leaked into inventory: %+v", v)
			}
		})
	}
}

var pnpmFixtures = map[string]string{
	"v6": `lockfileVersion: '6.0'

importers:

  .:
    dependencies:
      left-pad:
        specifier: ^1.3.0
        version: 1.3.0
      lodash:
        specifier: ^4.17.20
        version: 4.17.21
    devDependencies:
      typescript:
        specifier: ^5.3.0
        version: 5.3.3
`,
	"v9": `lockfileVersion: '9.0'

importers:

  .:
    dependencies:
      left-pad:
        specifier: ^1.3.0
        version: 1.3.0(react@18.2.0)
      lodash:
        specifier: ^4.17.20
        version: 4.17.21
    devDependencies:
      typescript:
        specifier: ^5.3.0
        version: 5.3.3
`,
}

func TestPnpmInventoryAcrossLockfileFormats(t *testing.T) {
	for name, lock := range pnpmFixtures {
		t.Run(name, func(t *testing.T) {
			dir := seed(t, map[string]string{
				"package.json":   packageJSON,
				"pnpm-lock.yaml": lock,
			})
			eco := nodejs.NewPnpm()
			targets, err := eco.Detect(context.Background(), dir)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if len(targets) != 1 {
				t.Fatalf("targets = %d, want 1", len(targets))
			}
			deps, err := eco.Inventory(context.Background(), targets[0])
			if err != nil {
				t.Fatalf("Inventory: %v", err)
			}
			got := map[string]string{}
			for _, d := range deps {
				got[d.Name] = d.Current
			}
			if got["lodash"] != "4.17.21" {
				t.Errorf("lodash = %q, want 4.17.21", got["lodash"])
			}
			// pnpm appends a peer suffix; it must be stripped.
			if got["left-pad"] != "1.3.0" {
				t.Errorf("left-pad = %q, want 1.3.0 with the peer suffix removed", got["left-pad"])
			}
		})
	}
}

func TestDetectionIsMutuallyExclusive(t *testing.T) {
	dir := seed(t, map[string]string{
		"package.json":      packageJSON,
		"pnpm-lock.yaml":    pnpmFixtures["v9"],
		"package-lock.json": lockfixtures["v3 (packages only)"],
	})
	npmTargets, err := nodejs.NewNpm().Detect(context.Background(), dir)
	if err != nil {
		t.Fatalf("npm Detect: %v", err)
	}
	if len(npmTargets) != 0 {
		t.Errorf("npm claimed a pnpm project: %+v", npmTargets)
	}
	pnpmTargets, err := nodejs.NewPnpm().Detect(context.Background(), dir)
	if err != nil {
		t.Fatalf("pnpm Detect: %v", err)
	}
	if len(pnpmTargets) != 1 {
		t.Errorf("pnpm targets = %d, want 1", len(pnpmTargets))
	}
}

// fakeRunner records the command and simulates the package manager by rewriting
// the lockfile, so Apply's observed delta can be asserted without a network.
type fakeRunner struct {
	commands []string
	rewrite  func(dir string, args []string) error
}

func (f *fakeRunner) Run(_ context.Context, dir, name string, args ...string) (string, error) {
	f.commands = append(f.commands, name+" "+strings.Join(args, " "))
	if f.rewrite != nil {
		return "", f.rewrite(dir, args)
	}
	return "", nil
}

func TestNpmApplyDelegatesToThePackageManager(t *testing.T) {
	dir := seed(t, map[string]string{
		"package.json":      packageJSON,
		"package-lock.json": lockfixtures["v3 (packages only)"],
	})
	runner := &fakeRunner{
		rewrite: func(dir string, args []string) error {
			// npm install lodash@4.17.22 -> the lockfile records the new version
			path := filepath.Join(dir, "package-lock.json")
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			updated := strings.Replace(string(data), `"4.17.21"`, `"4.17.22"`, 1)
			return os.WriteFile(path, []byte(updated), 0o644)
		},
	}
	eco := nodejs.NewWithRunner(nodejs.Npm, runner)

	res, err := eco.Apply(context.Background(), dir, []ecosystem.Change{
		{Name: "lodash", From: "4.17.21", To: "4.17.22"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(runner.commands) != 1 {
		t.Fatalf("commands = %v, want exactly one", runner.commands)
	}
	cmd := runner.commands[0]
	for _, want := range []string{"npm install lodash@4.17.22", "--ignore-scripts"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command %q missing %q (lifecycle scripts must be disabled)", cmd, want)
		}
	}
	if got := res.Observed["lodash"]; got != [2]string{"4.17.21", "4.17.22"} {
		t.Errorf("observed delta = %v, want the lockfile's before/after", got)
	}
	if res.Unsplittable {
		t.Error("a scoped change was reported as unsplittable")
	}
}

func TestPnpmApplyUsesUpdate(t *testing.T) {
	dir := seed(t, map[string]string{
		"package.json":   packageJSON,
		"pnpm-lock.yaml": pnpmFixtures["v6"],
	})
	runner := &fakeRunner{}
	eco := nodejs.NewWithRunner(nodejs.Pnpm, runner)

	if _, err := eco.Apply(context.Background(), dir, []ecosystem.Change{
		{Name: "lodash", From: "4.17.21", To: "4.17.22"},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(runner.commands) != 1 || !strings.HasPrefix(runner.commands[0], "pnpm update lodash@4.17.22") {
		t.Errorf("commands = %v, want a single pnpm update", runner.commands)
	}
}

func TestClassifyNodeVersions(t *testing.T) {
	eco := nodejs.NewNpm()
	cases := []struct {
		from, to string
		want     ecosystem.UpdateClass
	}{
		{"4.17.20", "4.17.22", ecosystem.ClassPatch},
		{"4.17.20", "4.18.0", ecosystem.ClassMinor},
		{"4.17.20", "5.0.0", ecosystem.ClassMajor},
		{"4.17.20", "4.17.20", ecosystem.ClassNone},
		{"^4.17.20", "4.17.22", ecosystem.ClassUnknown}, // a range is not a version
	}
	for _, tc := range cases {
		if got := eco.Classify(tc.from, tc.to); got != tc.want {
			t.Errorf("Classify(%q,%q) = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestVerifyCommandsUseTheRightManager(t *testing.T) {
	npmCmds := nodejs.NewNpm().VerifyCommands(ecosystem.Target{})
	pnpmCmds := nodejs.NewPnpm().VerifyCommands(ecosystem.Target{})
	if len(npmCmds) == 0 || npmCmds[0][0] != "npm" {
		t.Errorf("npm verify commands = %v, want npm", npmCmds)
	}
	if len(pnpmCmds) == 0 || pnpmCmds[0][0] != "pnpm" {
		t.Errorf("pnpm verify commands = %v, want pnpm", pnpmCmds)
	}
	// --if-present keeps a repository without scripts valid rather than failing.
	for _, argv := range append(npmCmds, pnpmCmds...) {
		if !contains(argv, "--if-present") {
			t.Errorf("command %v should tolerate missing scripts", argv)
		}
	}
}

func contains(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}
