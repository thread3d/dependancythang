package docker_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/ecosystem/docker"
)

// applyIn runs Apply over a temp directory seeded with one file and returns the
// resulting content — a golden diff by construction.
func applyIn(t *testing.T, name, content string, changes ...ecosystem.Change) (string, ecosystem.ApplyResult) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	res, err := docker.New().Apply(context.Background(), dir, changes)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return string(got), res
}

// TestDockerfileGolden is the §6 matrix row for Dockerfiles.
func TestDockerfileGolden(t *testing.T) {
	const before = `# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
FROM alpine:3.20 AS runtime
LABEL org.opencontainers.image.base.name="alpine:3.20"
COPY --from=build /src/app /app
ENTRYPOINT ["/app"]
FROM scratch
`
	const want = `# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
FROM alpine:3.21 AS runtime
LABEL org.opencontainers.image.base.name="alpine:3.20"
COPY --from=build /src/app /app
ENTRYPOINT ["/app"]
FROM scratch
`
	got, res := applyIn(t, "Dockerfile", before,
		ecosystem.Change{Name: "golang", From: "1.25-alpine", To: "1.26-alpine"},
		ecosystem.Change{Name: "alpine", From: "3.20", To: "3.21"},
	)
	if got != want {
		t.Errorf("dockerfile diff mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if len(res.Changed) != 2 {
		t.Errorf("changed = %v, want golang and alpine", res.Changed)
	}
	// The LABEL mentions alpine:3.20 and must not be rewritten: only FROM lines
	// and Compose image keys are manifests.
	if strings.Count(got, "alpine:3.20") != 1 {
		t.Errorf("a non-manifest mention was rewritten:\n%s", got)
	}
}

func TestDockerfileDigestPin(t *testing.T) {
	const before = "FROM alpine:3.20\n"
	const want = "FROM alpine:3.21@sha256:abc123\n"
	got, _ := applyIn(t, "Dockerfile", before, ecosystem.Change{
		Name: "alpine", From: "3.20", To: "3.21", Digest: "sha256:abc123",
	})
	if got != want {
		t.Errorf("digest pin diff\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestComposeGolden is the §6 matrix row for Compose files.
func TestComposeGolden(t *testing.T) {
	const before = `services:
  db:
    image: "postgres:16.2"
    environment:
      POSTGRES_PASSWORD: example
  cache:
    image: redis:7.2
`
	const want = `services:
  db:
    image: "postgres:16.4"
    environment:
      POSTGRES_PASSWORD: example
  cache:
    image: redis:7.4
`
	got, _ := applyIn(t, "docker-compose.yml", before,
		ecosystem.Change{Name: "postgres", From: "16.2", To: "16.4"},
		ecosystem.Change{Name: "redis", From: "7.2", To: "7.4"},
	)
	if got != want {
		t.Errorf("compose diff mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestInventoryReadsReferences(t *testing.T) {
	dir := t.TempDir()
	content := `FROM golang:1.25 AS build
FROM localhost:5000/team/app:2.1.0
FROM scratch
FROM alpine
`
	path := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	deps, err := docker.New().Inventory(context.Background(), ecosystem.Target{
		Ecosystem: "docker", Dir: "/", Manifest: path,
	})
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Current
	}
	want := map[string]string{
		"golang":                  "1.25",   // AS alias does not leak into the name
		"localhost:5000/team/app": "2.1.0",  // registry port is not a tag
		"alpine":                  "latest", // an unqualified image implies latest
	}
	if len(got) != len(want) {
		t.Fatalf("deps = %v, want %v (scratch must be skipped)", got, want)
	}
	for name, version := range want {
		if got[name] != version {
			t.Errorf("%s = %q, want %q", name, got[name], version)
		}
	}
}

func TestClassifyTagsAreNotSemver(t *testing.T) {
	eco := docker.New()
	cases := []struct {
		from, to string
		want     ecosystem.UpdateClass
	}{
		{"3.20", "3.21", ecosystem.ClassMinor},
		{"1.25-alpine", "1.26-alpine", ecosystem.ClassMinor},
		{"3.20", "4.0", ecosystem.ClassMajor},
		{"latest", "3.20", ecosystem.ClassPin},
		{"stable", "edge", ecosystem.ClassPin},
		{"3.20", "sha256:abc", ecosystem.ClassDigest},
		{"3.20", "3.20", ecosystem.ClassNone},
	}
	for _, tc := range cases {
		if got := eco.Classify(tc.from, tc.to); got != tc.want {
			t.Errorf("Classify(%q,%q) = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

// seedDockerRepo writes one file into a temp repository and returns its root.
func seedDockerRepo(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	return dir
}

// TestApplyRefusesATagThePlanDidNotPredict mirrors the Actions defect: an applier
// that rewrites any tag it finds produces a diff nobody reviewed when the branch is
// not the tree the plan was built from (DEP-D516B032).
func TestApplyRefusesATagThePlanDidNotPredict(t *testing.T) {
	const body = "FROM alpine:3.20 AS build\n"
	dir := seedDockerRepo(t, "Dockerfile", body)

	_, err := docker.New().Apply(context.Background(), dir, []ecosystem.Change{
		{Name: "alpine", From: "3.14", To: "3.21"},
	})
	mismatch, ok := ecosystem.PlanMismatch(err)
	if !ok {
		t.Fatalf("error is not a PlanMismatchError: %v", err)
	}
	if len(mismatch.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want one", mismatch.Mismatches)
	}
	if got := mismatch.Mismatches[0]; got.Got != "3.20" || got.Want != "3.14" {
		t.Errorf("mismatch = %+v, want the actual tag 3.20 instead of 3.14", got)
	}
	got, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != body {
		t.Errorf("the Dockerfile was written despite the refusal:\n%s", got)
	}
}

// TestApplyRewritesOnlyThePlannedTag: the plan names one version, so only the
// reference that holds it may move.
func TestApplyRewritesOnlyThePlannedTag(t *testing.T) {
	const body = "FROM alpine:3.14 AS base\nFROM alpine:3.19 AS newer\n"
	dir := seedDockerRepo(t, "Dockerfile", body)

	if _, err := docker.New().Apply(context.Background(), dir, []ecosystem.Change{
		{Name: "alpine", From: "3.14", To: "3.20"},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "FROM alpine:3.20 AS base\nFROM alpine:3.19 AS newer\n"
	got, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != want {
		t.Errorf("dockerfile =\n%s want\n%s", got, want)
	}
}
