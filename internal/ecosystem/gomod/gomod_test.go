package gomod_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/ecosystem/gomod"
)

const goModFixture = `module example.com/project

go 1.25

require (
	github.com/pkg/errors v0.9.1
	golang.org/x/text v0.14.0
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
`

func writeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(goModFixture), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	// A nested module must be discovered too, while vendor/ must be skipped.
	nested := filepath.Join(root, "cmd", "tool")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "go.mod"),
		[]byte("module example.com/tool\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatalf("write nested go.mod: %v", err)
	}
	vendor := filepath.Join(root, "vendor", "x")
	if err := os.MkdirAll(vendor, 0o755); err != nil {
		t.Fatalf("mkdir vendor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(vendor, "go.mod"),
		[]byte("module vendored.example/x\n"), 0o644); err != nil {
		t.Fatalf("write vendor go.mod: %v", err)
	}
	return root
}

func TestDetectFindsModulesAndSkipsVendor(t *testing.T) {
	root := writeRepo(t)
	targets, err := gomod.New().Detect(context.Background(), root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d (%+v), want 2 (root and nested, not vendor)", len(targets), targets)
	}
	dirs := map[string]bool{}
	for _, tg := range targets {
		dirs[tg.Dir] = true
		if tg.Ecosystem != "gomod" {
			t.Errorf("ecosystem = %q, want gomod", tg.Ecosystem)
		}
	}
	if !dirs["/"] {
		t.Errorf("root target missing; got dirs %v", dirs)
	}
	if !dirs["cmd/tool"] {
		t.Errorf("nested target missing; got dirs %v", dirs)
	}
}

func TestInventoryMarksIndirectDependencies(t *testing.T) {
	root := writeRepo(t)
	eco := gomod.New()
	targets, err := eco.Detect(context.Background(), root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	var rootTarget ecosystem.Target
	for _, tg := range targets {
		if tg.Dir == "/" {
			rootTarget = tg
		}
	}
	deps, err := eco.Inventory(context.Background(), rootTarget)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(deps) != 4 {
		t.Fatalf("deps = %d, want 4", len(deps))
	}

	byName := map[string]ecosystem.Dependency{}
	for _, d := range deps {
		byName[d.Name] = d
	}
	if d := byName["github.com/pkg/errors"]; !d.Direct || d.Current != "v0.9.1" {
		t.Errorf("github.com/pkg/errors = %+v, want direct v0.9.1", d)
	}
	if d := byName["github.com/davecgh/go-spew"]; d.Direct {
		t.Errorf("go-spew should be indirect, got %+v", d)
	}
	// Direct dependencies must sort before indirect ones.
	if !deps[0].Direct || !deps[1].Direct {
		t.Errorf("direct dependencies did not sort first: %+v", deps[:2])
	}
}

func TestClassifyGoSemantics(t *testing.T) {
	eco := gomod.New()
	cases := []struct {
		from, to string
		want     ecosystem.UpdateClass
	}{
		{"v1.2.3", "v1.2.4", ecosystem.ClassPatch},
		{"v1.2.3", "v1.3.0", ecosystem.ClassMinor},
		{"v1.9.9", "v2.0.0", ecosystem.ClassMajor},
		{"v0.1.0", "v0.1.1", ecosystem.ClassPatch},
		{"v1.2.3", "v1.2.3", ecosystem.ClassNone},
		{"not-a-version", "v1.0.0", ecosystem.ClassUnknown},
	}
	for _, tc := range cases {
		if got := eco.Classify(tc.from, tc.to); got != tc.want {
			t.Errorf("Classify(%s, %s) = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestCompareOrdersVersions(t *testing.T) {
	eco := gomod.New()
	if eco.Compare("v1.2.3", "v1.2.4") >= 0 {
		t.Error("v1.2.3 should sort before v1.2.4")
	}
	if eco.Compare("v2.0.0", "v1.9.9") <= 0 {
		t.Error("v2.0.0 should sort after v1.9.9")
	}
}

func TestInventoryRejectsUnparseableManifest(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "go.mod")
	if err := os.WriteFile(path, []byte("this is not a go.mod\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := gomod.New().Inventory(context.Background(), ecosystem.Target{
		Ecosystem: "gomod", Dir: "/", Manifest: path,
	})
	if err == nil {
		t.Fatal("expected a parse error")
	}
}
