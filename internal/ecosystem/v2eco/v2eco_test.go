package v2eco_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/ecosystem/v2eco"
)

func seed(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return dir
}

func inventory(t *testing.T, eco ecosystem.Ecosystem, dir string) map[string]ecosystem.Dependency {
	t.Helper()
	targets, err := eco.Detect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(targets) == 0 {
		t.Fatalf("no targets detected in %s", dir)
	}
	deps, err := eco.Inventory(context.Background(), targets[0])
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	out := map[string]ecosystem.Dependency{}
	for _, d := range deps {
		out[d.Name] = d
	}
	return out
}

// ---- Python -------------------------------------------------------------

func TestPythonRequirementsInventory(t *testing.T) {
	dir := seed(t, map[string]string{
		"requirements.txt": `# runtime
requests==2.31.0
flask>=2.0,<3.0
numpy==1.26.4  # pinned for CI
-r dev-requirements.txt
`,
	})
	deps := inventory(t, v2eco.NewPython(), dir)

	if len(deps) != 3 {
		t.Fatalf("deps = %d (%v), want requests, flask, numpy", len(deps), deps)
	}
	if got := deps["requests"]; got.Current != "2.31.0" {
		t.Errorf("requests = %+v, want the pinned version", got)
	}
	// A range has no resolved version; inventing one would be a lie.
	if got := deps["flask"]; got.Current != "" {
		t.Errorf("flask = %+v, want no current version from a range", got)
	}
	if got := deps["numpy"]; got.Current != "1.26.4" {
		t.Errorf("numpy = %+v, want the pin with the trailing comment ignored", got)
	}
}

func TestPythonPyprojectInventory(t *testing.T) {
	dir := seed(t, map[string]string{
		"pyproject.toml": `[project]
name = "demo"
dependencies = [
  "requests==2.31.0",
  "click>=8.1",
]

[tool.poetry.dependencies]
python = "^3.11"
httpx = "0.27.0"
`,
	})
	deps := inventory(t, v2eco.NewPython(), dir)
	if got := deps["requests"]; got.Current != "2.31.0" {
		t.Errorf("requests = %+v", got)
	}
	if _, ok := deps["click"]; !ok {
		t.Error("click was not discovered in the PEP 621 array")
	}
	if got, ok := deps["httpx"]; !ok || got.Current != "0.27.0" {
		t.Errorf("httpx = %+v, want the poetry pin", got)
	}
}

// TestPythonApplyRewritesRequirements is the pip golden: a requirements file is a
// manifest, so editing it directly is allowed (§6).
func TestPythonApplyRewritesRequirements(t *testing.T) {
	dir := seed(t, map[string]string{
		"requirements.txt": "requests==2.31.0\nflask>=2.0\n",
	})
	res, err := v2eco.NewPython().Apply(context.Background(), dir, []ecosystem.Change{
		{Name: "requests", From: "2.31.0", To: "2.32.3"},
		{Name: "flask", From: "2.0", To: "2.3.0"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "requirements.txt"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	want := "requests==2.32.3\nflask>=2.0\n"
	if string(got) != want {
		t.Errorf("requirements.txt = %q, want %q", got, want)
	}
	if _, ok := res.Observed["requests"]; !ok {
		t.Error("observed delta missing for requests")
	}
	// A range is left alone rather than silently rewritten to a pin.
	if strings.Contains(string(got), "flask==2.3.0") {
		t.Error("a range constraint was rewritten into a pin")
	}
}

// ---- Cargo --------------------------------------------------------------

func TestCargoInventoryUsesLockfile(t *testing.T) {
	dir := seed(t, map[string]string{
		"Cargo.toml": `[package]
name = "demo"
version = "0.1.0"

[dependencies]
serde = "1.0.100"
regex = { version = "1.10", features = ["unicode"] }
`,
		"Cargo.lock": `[[package]]
name = "serde"
version = "1.0.210"

[[package]]
name = "regex"
version = "1.10.6"
`,
	})
	deps := inventory(t, v2eco.NewCargo(), dir)
	if got := deps["serde"]; got.Current != "1.0.210" {
		t.Errorf("serde = %+v, want the locked version", got)
	}
	if got := deps["regex"]; got.Current != "1.10.6" {
		t.Errorf("regex = %+v, want the locked version from the table form", got)
	}
	// The package's own version line must not be mistaken for a dependency.
	if _, ok := deps["version"]; ok {
		t.Error("the package version field leaked into inventory")
	}
}

// fakeRunner records commands and simulates the toolchain.
type fakeRunner struct{ commands []string }

func (f *fakeRunner) Run(_ context.Context, dir, name string, args ...string) (string, error) {
	f.commands = append(f.commands, name+" "+strings.Join(args, " "))
	return "", nil
}

func TestCargoApplyDelegates(t *testing.T) {
	dir := seed(t, map[string]string{
		"Cargo.toml": "[dependencies]\nserde = \"1.0\"\n",
		"Cargo.lock": "[[package]]\nname = \"serde\"\nversion = \"1.0.210\"\n",
	})
	runner := &fakeRunner{}
	eco := v2eco.NewCargoWithRunner(runner)

	res, err := eco.Apply(context.Background(), dir, []ecosystem.Change{
		{Name: "serde", From: "1.0.210", To: "1.0.215"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("commands = %v, want one", runner.commands)
	}
	for _, want := range []string{"cargo update", "-p serde", "--precise 1.0.215"} {
		if !strings.Contains(runner.commands[0], want) {
			t.Errorf("command %q missing %q", runner.commands[0], want)
		}
	}
	if got := res.Observed["serde"]; got != [2]string{"1.0.210", "1.0.215"} {
		t.Errorf("observed = %v", got)
	}
}

// ---- yarn ---------------------------------------------------------------

const yarnLock = `# yarn lockfile v1

left-pad@^1.3.0:
  version "1.3.0"
  resolved "https://registry.yarnpkg.com/left-pad/-/left-pad-1.3.0.tgz"

lodash@^4.17.20, lodash@^4.17.21:
  version "4.17.21"
  resolved "https://registry.yarnpkg.com/lodash/-/lodash-4.17.21.tgz"
`

func TestYarnInventory(t *testing.T) {
	dir := seed(t, map[string]string{"yarn.lock": yarnLock})
	deps := inventory(t, v2eco.NewYarn(), dir)
	if len(deps) != 2 {
		t.Fatalf("deps = %d (%v), want 2", len(deps), deps)
	}
	if got := deps["lodash"].Current; got != "4.17.21" {
		t.Errorf("lodash = %q, want 4.17.21 from a multi-range entry", got)
	}
}

func TestYarnRefusesBerry(t *testing.T) {
	dir := seed(t, map[string]string{
		"yarn.lock": "__metadata:\n  version: 8\n  cacheKey: 10\n",
	})
	targets, err := v2eco.NewYarn().Detect(context.Background(), dir)
	if err != nil || len(targets) != 1 {
		t.Fatalf("Detect: %v, %d targets", err, len(targets))
	}
	if _, err := v2eco.NewYarn().Inventory(context.Background(), targets[0]); err == nil {
		t.Error("a yarn berry lockfile was read as if it were v1")
	}
}

func TestYarnYieldsToOtherManagers(t *testing.T) {
	dir := seed(t, map[string]string{
		"yarn.lock":         yarnLock,
		"package-lock.json": "{}",
	})
	targets, err := v2eco.NewYarn().Detect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("yarn claimed a project that has a package-lock.json")
	}
}

func TestYarnApplyDelegates(t *testing.T) {
	dir := seed(t, map[string]string{"yarn.lock": yarnLock})
	runner := &fakeRunner{}
	if _, err := v2eco.NewYarnWithRunner(runner).Apply(context.Background(), dir, []ecosystem.Change{
		{Name: "lodash", From: "4.17.21", To: "4.17.22"},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(runner.commands) != 1 || !strings.Contains(runner.commands[0], "yarn upgrade lodash@4.17.22") {
		t.Errorf("commands = %v", runner.commands)
	}
	if !strings.Contains(runner.commands[0], "--ignore-scripts") {
		t.Error("lifecycle scripts must be disabled in Phase A")
	}
}
