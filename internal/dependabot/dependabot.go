// Package dependabot reads an existing .github/dependabot.yml so adopting depmcp
// costs nothing (§3 row 17).
//
// The mapping is deliberately one-way and reporting-friendly: keys that cannot
// be expressed are returned to the caller rather than dropped silently, because
// "we imported your config" is a promise that should be checkable.
package dependabot

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the subset of dependabot.yml this importer understands.
type Config struct {
	Version int      `yaml:"version"`
	Updates []Update `yaml:"updates"`
}

// Update is one `updates:` entry.
type Update struct {
	PackageEcosystem string `yaml:"package-ecosystem"`
	Directory        string `yaml:"directory"`
	Schedule         struct {
		Interval string `yaml:"interval"`
		Day      string `yaml:"day"`
		Time     string `yaml:"time"`
		Timezone string `yaml:"timezone"`
		Cronjob  string `yaml:"cronjob"`
	} `yaml:"schedule"`
	Groups map[string]struct {
		Patterns []string `yaml:"patterns"`
	} `yaml:"groups"`
	Ignore []struct {
		DependencyName string   `yaml:"dependency-name"`
		Versions       []string `yaml:"versions"`
		UpdateTypes    []string `yaml:"update-types"`
	} `yaml:"ignore"`
	Allow []struct {
		DependencyType string `yaml:"dependency-type"`
		DependencyName string `yaml:"dependency-name"`
		UpdateType     string `yaml:"update-type"`
	} `yaml:"allow"`
	Cooldown struct {
		DefaultDays int `yaml:"default-days"`
		SemverMajor int `yaml:"semver-major-days"`
		SemverMinor int `yaml:"semver-minor-days"`
		SemverPatch int `yaml:"semver-patch-days"`
	} `yaml:"cooldown"`
	// Everything else is reported as unmapped.
	OpenPullRequestsLimit    int            `yaml:"open-pull-requests-limit"`
	Labels                   []string       `yaml:"labels"`
	Reviewers                []string       `yaml:"reviewers"`
	Assignees                []string       `yaml:"assignees"`
	CommitMessage            map[string]any `yaml:"commit-message"`
	Groups2                  map[string]any `yaml:"group"`
	TargetBranch             string         `yaml:"target-branch"`
	Vendor                   *bool          `yaml:"vendor"`
	InsecureExternalCodeExec bool           `yaml:"insecure-external-code-execution"`
	Registries               []any          `yaml:"registries"`
	RebaseStrategy           string         `yaml:"rebase-strategy"`
	PullRequestBranchName    map[string]any `yaml:"pull-request-branch-name"`
	Milestone                int            `yaml:"milestone"`
}

// Ecosystem maps dependabot's names onto ours.
func Ecosystem(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "gomod", "go", "go_modules":
		return "gomod", true
	case "npm":
		return "npm", true
	case "pnpm":
		return "pnpm", true
	case "docker":
		return "docker", true
	case "github_actions", "github-actions":
		return "actions", true
	default:
		return "", false
	}
}

// Imported is one target translated into depmcp terms.
type Imported struct {
	Ecosystem string `json:"ecosystem"`
	Directory string `json:"directory"`
	Schedule  struct {
		Interval string `json:"interval,omitempty"`
		Day      string `json:"day,omitempty"`
		Time     string `json:"time,omitempty"`
		Timezone string `json:"timezone,omitempty"`
		Cron     string `json:"cron,omitempty"`
	} `json:"schedule"`
	Groups   map[string][]string `json:"groups,omitempty"`
	Ignore   []IgnoreOut         `json:"ignore,omitempty"`
	Allow    []AllowOut          `json:"allow,omitempty"`
	Cooldown *CooldownOut        `json:"cooldown,omitempty"`
}

// IgnoreOut is an ignore rule in our shape.
type IgnoreOut struct {
	Dependency  string   `json:"dependency"`
	Versions    []string `json:"versions,omitempty"`
	UpdateTypes []string `json:"update_types,omitempty"`
	// Reason is mandatory in depmcp, and dependabot does not record one, so the
	// importer supplies an honest placeholder the user is expected to replace.
	Reason string `json:"reason"`
}

// AllowOut is an allow rule in our shape.
type AllowOut struct {
	DependencyType string `json:"dependency_type,omitempty"`
	DependencyName string `json:"dependency_name,omitempty"`
	UpdateType     string `json:"update_type,omitempty"`
}

// CooldownOut is a cooldown in our shape.
type CooldownOut struct {
	PatchDays int `json:"patch_days"`
	MinorDays int `json:"minor_days"`
	MajorDays int `json:"major_days"`
}

// Result is the outcome of an import.
type Result struct {
	Source   string     `json:"source"`
	Version  int        `json:"version"`
	Imported []Imported `json:"imported"`
	// Unmapped names keys that exist in the source but have no depmcp equivalent.
	Unmapped []string `json:"unmapped,omitempty"`
	// Warnings are notes about values that were mapped lossily.
	Warnings []string `json:"warnings,omitempty"`
}

// ImportPath reads and imports a dependabot.yml.
func ImportPath(path string) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", path, err)
	}
	return Import(data, path)
}

// ImportPathForRepo looks for .github/dependabot.yml under a repository root.
func ImportPathForRepo(root string) (Result, error) {
	candidates := []string{
		filepath.Join(root, ".github", "dependabot.yml"),
		filepath.Join(root, ".github", "dependabot.yaml"),
		filepath.Join(root, "dependabot.yml"),
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return ImportPath(path)
		}
	}
	return Result{}, fmt.Errorf("no dependabot.yml found under %s", root)
}

// Import parses a dependabot document.
func Import(data []byte, source string) (Result, error) {
	var doc Config
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Result{}, fmt.Errorf("parse %s: %w", source, err)
	}
	res := Result{Source: source, Version: doc.Version}
	if len(doc.Updates) == 0 {
		res.Warnings = append(res.Warnings, "no updates entries found; nothing to import")
		return res, nil
	}

	for _, u := range doc.Updates {
		eco, ok := Ecosystem(u.PackageEcosystem)
		if !ok {
			res.Unmapped = append(res.Unmapped,
				fmt.Sprintf("package-ecosystem %q (not supported in v1)", u.PackageEcosystem))
			continue
		}
		item := Imported{Ecosystem: eco, Directory: u.Directory}
		if item.Directory == "" {
			item.Directory = "/"
			res.Warnings = append(res.Warnings, eco+": directory defaults to /")
		}
		item.Schedule.Interval = u.Schedule.Interval
		item.Schedule.Day = u.Schedule.Day
		item.Schedule.Time = u.Schedule.Time
		item.Schedule.Timezone = u.Schedule.Timezone
		item.Schedule.Cron = u.Schedule.Cronjob
		if u.Schedule.Timezone == "" {
			res.Warnings = append(res.Warnings, eco+": schedule has no timezone; depmcp requires an explicit one")
		}

		if len(u.Groups) > 0 {
			item.Groups = map[string][]string{}
			for name, g := range u.Groups {
				item.Groups[name] = g.Patterns
			}
		}
		for _, rule := range u.Ignore {
			out := IgnoreOut{
				Dependency:  rule.DependencyName,
				Versions:    rule.Versions,
				UpdateTypes: rule.UpdateTypes,
			}
			if out.Dependency == "" {
				res.Unmapped = append(res.Unmapped, eco+": ignore rule with no dependency-name")
				continue
			}
			out.Reason = "imported from dependabot.yml; add a reason"
			item.Ignore = append(item.Ignore, out)
		}
		for _, rule := range u.Allow {
			item.Allow = append(item.Allow, AllowOut{
				DependencyType: rule.DependencyType,
				DependencyName: rule.DependencyName,
				UpdateType:     rule.UpdateType,
			})
		}
		if u.Cooldown.DefaultDays > 0 || u.Cooldown.SemverMajor > 0 ||
			u.Cooldown.SemverMinor > 0 || u.Cooldown.SemverPatch > 0 {
			cd := CooldownOut{PatchDays: u.Cooldown.DefaultDays, MinorDays: u.Cooldown.DefaultDays, MajorDays: u.Cooldown.DefaultDays}
			if u.Cooldown.SemverPatch > 0 {
				cd.PatchDays = u.Cooldown.SemverPatch
			}
			if u.Cooldown.SemverMinor > 0 {
				cd.MinorDays = u.Cooldown.SemverMinor
			}
			if u.Cooldown.SemverMajor > 0 {
				cd.MajorDays = u.Cooldown.SemverMajor
			}
			item.Cooldown = &cd
		}

		res.Unmapped = append(res.Unmapped, unmappedKeys(u, eco)...)
		res.Imported = append(res.Imported, item)
	}

	sort.Strings(res.Unmapped)
	sort.SliceStable(res.Imported, func(i, j int) bool {
		if res.Imported[i].Ecosystem != res.Imported[j].Ecosystem {
			return res.Imported[i].Ecosystem < res.Imported[j].Ecosystem
		}
		return res.Imported[i].Directory < res.Imported[j].Directory
	})
	return res, nil
}

// unmappedKeys reports keys this importer cannot express.
func unmappedKeys(u Update, eco string) []string {
	var out []string
	add := func(cond bool, key string) {
		if cond {
			out = append(out, fmt.Sprintf("%s: %s (not mapped)", eco, key))
		}
	}
	add(len(u.Labels) > 0, "labels")
	add(len(u.Reviewers) > 0, "reviewers")
	add(len(u.Assignees) > 0, "assignees")
	add(len(u.CommitMessage) > 0, "commit-message")
	add(u.OpenPullRequestsLimit > 0, "open-pull-requests-limit")
	add(u.TargetBranch != "", "target-branch")
	add(u.Vendor != nil, "vendor")
	add(u.InsecureExternalCodeExec, "insecure-external-code-execution")
	add(len(u.Registries) > 0, "registries")
	add(u.RebaseStrategy != "", "rebase-strategy")
	add(u.Milestone > 0, "milestone")
	return out
}
