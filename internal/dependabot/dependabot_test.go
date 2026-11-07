package dependabot_test

import (
	"strings"
	"testing"

	"github.com/thread3d/dependancythang/internal/dependabot"
)

const fixture = `version: 2
updates:
  - package-ecosystem: "gomod"
    directory: "/"
    schedule:
      interval: "weekly"
      day: "monday"
      time: "06:00"
      timezone: "Europe/Copenhagen"
    groups:
      golang-x:
        patterns: ["golang.org/x/*"]
    ignore:
      - dependency-name: "github.com/foo/bar"
        versions: [">=3.0.0"]
        update-types: ["version-update:semver-major"]
    cooldown:
      default-days: 3
      semver-major-days: 7
    labels: ["dependencies"]
    open-pull-requests-limit: 5

  - package-ecosystem: "npm"
    directory: "/web"
    schedule:
      interval: "daily"
    allow:
      - dependency-type: "production"

  - package-ecosystem: "pip"
    directory: "/"
    schedule:
      interval: "weekly"
`

func TestImportMapsSupportedEcosystems(t *testing.T) {
	res, err := dependabot.Import([]byte(fixture), "dependabot.yml")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Version != 2 {
		t.Errorf("version = %d, want 2", res.Version)
	}
	if len(res.Imported) != 2 {
		t.Fatalf("imported = %d, want 2 (pip is unsupported)", len(res.Imported))
	}

	gomod := res.Imported[0]
	if gomod.Ecosystem != "gomod" || gomod.Directory != "/" {
		t.Errorf("first entry = %+v, want gomod at /", gomod)
	}
	if gomod.Schedule.Interval != "weekly" || gomod.Schedule.Timezone != "Europe/Copenhagen" {
		t.Errorf("schedule = %+v", gomod.Schedule)
	}
	if len(gomod.Groups["golang-x"]) != 1 || gomod.Groups["golang-x"][0] != "golang.org/x/*" {
		t.Errorf("groups = %+v", gomod.Groups)
	}
	if len(gomod.Ignore) != 1 {
		t.Fatalf("ignore rules = %d, want 1", len(gomod.Ignore))
	}
	// depmcp requires a reason; dependabot has none, so the importer must say so
	// rather than inventing a meaningful one.
	if !strings.Contains(gomod.Ignore[0].Reason, "add a reason") {
		t.Errorf("ignore reason = %q, want a placeholder that asks for a reason", gomod.Ignore[0].Reason)
	}
	if gomod.Cooldown == nil || gomod.Cooldown.MajorDays != 7 || gomod.Cooldown.MinorDays != 3 {
		t.Errorf("cooldown = %+v, want major 7 and default 3 carried to minor", gomod.Cooldown)
	}

	npm := res.Imported[1]
	if npm.Ecosystem != "npm" || npm.Directory != "/web" {
		t.Errorf("second entry = %+v, want npm at /web", npm)
	}
	if len(npm.Allow) != 1 || npm.Allow[0].DependencyType != "production" {
		t.Errorf("allow = %+v", npm.Allow)
	}
}

func TestImportReportsUnmappedKeys(t *testing.T) {
	res, err := dependabot.Import([]byte(fixture), "dependabot.yml")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	joined := strings.Join(res.Unmapped, "\n")
	for _, want := range []string{"pip", "labels", "open-pull-requests-limit"} {
		if !strings.Contains(joined, want) {
			t.Errorf("unmapped list missing %q:\n%s", want, joined)
		}
	}
}

func TestImportWarnsAboutMissingTimezone(t *testing.T) {
	res, err := dependabot.Import([]byte(fixture), "dependabot.yml")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "timezone") {
			found = true
		}
	}
	if !found {
		t.Errorf("no timezone warning; warnings = %v", res.Warnings)
	}
}

func TestEcosystemMapping(t *testing.T) {
	cases := map[string]string{
		"gomod": "gomod", "go": "gomod", "go_modules": "gomod",
		"npm": "npm", "pnpm": "pnpm", "docker": "docker",
		"github_actions": "actions", "GitHub_Actions": "actions",
	}
	for in, want := range cases {
		if got, ok := dependabot.Ecosystem(in); !ok || got != want {
			t.Errorf("Ecosystem(%q) = %q,%v; want %q,true", in, got, ok, want)
		}
	}
	if _, ok := dependabot.Ecosystem("pip"); ok {
		t.Error("pip should not be supported in v1")
	}
}
