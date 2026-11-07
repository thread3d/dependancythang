// Package schedule turns a repository's schedule into something the operating
// system can run on its own.
//
// There is no daemon (§2), so a schedule is not a goroutine: it is a systemd
// user timer, a launchd agent, or a crontab line that invokes a one-shot
// `depmcp run`. This package only generates and installs those artifacts; the
// work itself lives in the run command.
package schedule

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/thread3d/dependancythang/internal/config"
)

// Spec is one repository's scheduling intent, resolved from configuration.
type Spec struct {
	RepoID   string
	RepoPath string
	Schedule config.Schedule
	// Binary is the absolute path to the depmcp executable.
	Binary string
	// ConfigPath is passed through so the timer reads the same configuration an
	// interactive session would.
	ConfigPath string
	// StateDir is passed through for the same reason.
	StateDir string
	// Path is the PATH the unit should use. systemd and launchd start with a
	// minimal environment, and the ecosystem CLIs (notably pnpm, which commonly
	// lives under ~/.local/share) are often not on it.
	Path string
	// Jitter is the randomised delay applied to the timer.
	Jitter string
}

// Kind identifies a generated artifact.
type Kind string

// Supported kinds.
const (
	KindSystemd Kind = "systemd"
	KindLaunchd Kind = "launchd"
	KindCron    Kind = "cron"
)

// Artifact is one generated file.
type Artifact struct {
	Kind Kind   `json:"kind"`
	Name string `json:"name"`
	Path string `json:"path"` // empty for cron, which is pasted into a crontab
	Body string `json:"body"`
}

// DefaultPath is the PATH baked into units: the toolchain first, then the system
// path, then the per-user locations that package managers use.
func DefaultPath() string {
	parts := []string{"/usr/lib/golang/bin", "/usr/local/bin", "/usr/bin", "/bin"}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		parts = append(parts,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, ".local", "share", "pnpm", "bin"),
			filepath.Join(home, "go", "bin"),
		)
	}
	return strings.Join(parts, ":")
}

// Generate renders the artifacts for a repository's schedule on this platform.
func Generate(spec Spec, kind Kind) ([]Artifact, error) {
	if spec.Schedule.Interval == "" {
		return nil, fmt.Errorf("repository %s has no schedule configured", spec.RepoPath)
	}
	if spec.Binary == "" {
		return nil, fmt.Errorf("cannot schedule %s: the depmcp binary path is unknown", spec.RepoPath)
	}
	if spec.Jitter == "" {
		spec.Jitter = "10m"
	}
	if spec.Path == "" {
		spec.Path = DefaultPath()
	}
	switch kind {
	case KindSystemd:
		return systemdArtifacts(spec)
	case KindLaunchd:
		return launchdArtifacts(spec)
	case KindCron:
		return cronArtifacts(spec)
	default:
		return nil, fmt.Errorf("unknown schedule kind %q", kind)
	}
}

// unitName derives a stable, unit-safe name from the repository id.
func unitName(spec Spec) string {
	id := spec.RepoID
	if id == "" {
		id = "repo"
	}
	return "depmcp-" + sanitize(id)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, s)
}

// execArgs renders the one-shot command the unit will run.
//
// The configuration and state locations travel as environment variables rather
// than flags: the unit already sets them, and a duplicated source of truth in
// ExecStart is one more thing that can drift out of sync.
func execArgs(spec Spec) []string {
	return []string{spec.Binary, "run", "--repo", spec.RepoPath}
}

func systemdArtifacts(spec Spec) ([]Artifact, error) {
	calendar, err := systemdCalendar(spec.Schedule)
	if err != nil {
		return nil, err
	}
	name := unitName(spec)

	var service strings.Builder
	fmt.Fprintf(&service, "[Unit]\n")
	fmt.Fprintf(&service, "Description=depmcp scheduled run for %s\n", spec.RepoPath)
	service.WriteString("Documentation=https://github.com/thread3d/dependancythang\n\n")
	service.WriteString("[Service]\n")
	service.WriteString("Type=oneshot\n")
	fmt.Fprintf(&service, "ExecStart=%s\n", strings.Join(execArgs(spec), " "))
	// The minimal environment is the whole reason this file exists: systemd
	// does not read the shell profile, so a pnpm under ~/.local would otherwise
	// be invisible to the scheduled run.
	fmt.Fprintf(&service, "Environment=PATH=%s\n", spec.Path)
	if tz := strings.TrimSpace(spec.Schedule.Timezone); tz != "" {
		fmt.Fprintf(&service, "Environment=TZ=%s\n", tz)
	}
	if spec.ConfigPath != "" {
		fmt.Fprintf(&service, "Environment=%s=%s\n", config.EnvConfig, spec.ConfigPath)
	}
	if spec.StateDir != "" {
		fmt.Fprintf(&service, "Environment=%s=%s\n", config.EnvStateDir, spec.StateDir)
	}
	service.WriteString("Nice=10\n")
	service.WriteString("StandardOutput=journal\nStandardError=journal\n\n")
	service.WriteString("[Install]\nWantedBy=default.target\n")

	var timer strings.Builder
	fmt.Fprintf(&timer, "[Unit]\n")
	fmt.Fprintf(&timer, "Description=depmcp timer for %s\n\n", spec.RepoPath)
	timer.WriteString("[Timer]\n")
	fmt.Fprintf(&timer, "OnCalendar=%s\n", calendar)
	// Persistent=true is what makes a missed slot fire once on the next wake,
	// which is the whole "laptop was asleep" requirement (§12).
	timer.WriteString("Persistent=true\n")
	fmt.Fprintf(&timer, "RandomizedDelaySec=%s\n", spec.Jitter)
	fmt.Fprintf(&timer, "Unit=%s.service\n\n", name)
	timer.WriteString("[Install]\nWantedBy=timers.target\n")

	dir := userSystemdDir()
	return []Artifact{
		{Kind: KindSystemd, Name: name + ".service", Path: filepath.Join(dir, name+".service"), Body: service.String()},
		{Kind: KindSystemd, Name: name + ".timer", Path: filepath.Join(dir, name+".timer"), Body: timer.String()},
	}, nil
}

// userSystemdDir returns the user unit directory.
func userSystemdDir() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "systemd", "user")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".config/systemd/user"
	}
	return filepath.Join(home, ".config", "systemd", "user")
}

// systemdCalendar renders the schedule as an OnCalendar expression.
func systemdCalendar(s config.Schedule) (string, error) {
	at, err := clock(s.Time)
	if err != nil {
		return "", err
	}
	switch s.Interval {
	case "daily":
		return "*-*-* " + at, nil
	case "monthly":
		return "*-*-01 " + at, nil
	case "weekly":
		day, err := weekdayShort(s.Day)
		if err != nil {
			return "", err
		}
		return day + " " + at, nil
	case "cron":
		return "", fmt.Errorf("a cron expression cannot be expressed as an OnCalendar timer; use `depmcp schedule print --kind cron` instead")
	default:
		return "", fmt.Errorf("unknown schedule interval %q", s.Interval)
	}
}

// clock normalises "06:00" and "6:00:00" to "HH:MM:SS".
func clock(t string) (string, error) {
	t = strings.TrimSpace(t)
	if t == "" {
		t = "06:00"
	}
	parts := strings.Split(t, ":")
	if len(parts) < 2 {
		return "", fmt.Errorf("time %q must be HH:MM", t)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return "", fmt.Errorf("time %q has an invalid hour", t)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return "", fmt.Errorf("time %q has an invalid minute", t)
	}
	sec := 0
	if len(parts) > 2 {
		if sec, err = strconv.Atoi(parts[2]); err != nil {
			return "", fmt.Errorf("time %q has an invalid second", t)
		}
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, sec), nil
}

var weekdays = map[string]string{
	"monday": "Mon", "tuesday": "Tue", "wednesday": "Wed", "thursday": "Thu",
	"friday": "Fri", "saturday": "Sat", "sunday": "Sun",
}

func weekdayShort(day string) (string, error) {
	if d, ok := weekdays[strings.ToLower(strings.TrimSpace(day))]; ok {
		return d, nil
	}
	return "", fmt.Errorf("unknown weekday %q", day)
}

// launchdArtifacts renders a LaunchAgent plist.
func launchdArtifacts(spec Spec) ([]Artifact, error) {
	label := "dev.depmcp." + sanitize(spec.RepoID)
	h, m, err := clockParts(spec.Schedule.Time)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	fmt.Fprintf(&b, "\t<key>Label</key>\n\t<string>%s</string>\n", label)
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range execArgs(spec) {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", xmlEscape(arg))
	}
	b.WriteString("\t</array>\n")

	b.WriteString("\t<key>StartCalendarInterval</key>\n\t<dict>\n")
	if spec.Schedule.Interval == "weekly" {
		wd, err := launchdWeekday(spec.Schedule.Day)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "\t\t<key>Weekday</key>\n\t\t<integer>%d</integer>\n", wd)
	} else if spec.Schedule.Interval == "monthly" {
		b.WriteString("\t\t<key>Day</key>\n\t\t<integer>1</integer>\n")
	}
	fmt.Fprintf(&b, "\t\t<key>Hour</key>\n\t\t<integer>%d</integer>\n", h)
	fmt.Fprintf(&b, "\t\t<key>Minute</key>\n\t\t<integer>%d</integer>\n", m)
	b.WriteString("\t</dict>\n")

	if spec.Schedule.Timezone != "" {
		fmt.Fprintf(&b, "\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>TZ</key>\n\t\t<string>%s</string>\n\t</dict>\n", xmlEscape(spec.Schedule.Timezone))
	}
	b.WriteString("\t<key>StandardOutPath</key>\n\t<string>/tmp/depmcp.out.log</string>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n\t<string>/tmp/depmcp.err.log</string>\n")
	b.WriteString("</dict>\n</plist>\n")

	path := filepath.Join(userLaunchAgentsDir(), label+".plist")
	return []Artifact{{Kind: KindLaunchd, Name: label + ".plist", Path: path, Body: b.String()}}, nil
}

func userLaunchAgentsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "Library/LaunchAgents"
	}
	return filepath.Join(home, "Library", "LaunchAgents")
}

// launchdWeekday maps a day name to launchd's 0=Sunday numbering.
func launchdWeekday(day string) (int, error) {
	order := map[string]int{
		"sunday": 0, "monday": 1, "tuesday": 2, "wednesday": 3,
		"thursday": 4, "friday": 5, "saturday": 6,
	}
	if d, ok := order[strings.ToLower(strings.TrimSpace(day))]; ok {
		return d, nil
	}
	return 0, fmt.Errorf("unknown weekday %q", day)
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// launchd ignores the PATH variable for ProgramArguments lookups, so the binary
// must already be absolute; that is enforced in Generate.

// cronArtifacts renders a crontab line.
func cronArtifacts(spec Spec) ([]Artifact, error) {
	line, err := cronLine(spec)
	if err != nil {
		return nil, err
	}
	return []Artifact{{Kind: KindCron, Name: "crontab", Body: line + "\n"}}, nil
}

// cronLine renders the five cron fields plus the command.
func cronLine(spec Spec) (string, error) {
	s := spec.Schedule
	if s.Interval == "cron" {
		if strings.TrimSpace(s.Cron) == "" {
			return "", fmt.Errorf("interval is cron but no expression is set")
		}
		return fmt.Sprintf("%s %s", strings.TrimSpace(s.Cron), strings.Join(execArgs(spec), " ")), nil
	}
	h, m, err := clockParts(s.Time)
	if err != nil {
		return "", err
	}
	var dom, mon, dow string
	switch s.Interval {
	case "daily":
		dom, mon, dow = "*", "*", "*"
	case "monthly":
		dom, mon, dow = "1", "*", "*"
	case "weekly":
		wd, err := cronWeekday(s.Day)
		if err != nil {
			return "", err
		}
		dom, mon, dow = "*", "*", wd
	default:
		return "", fmt.Errorf("unknown schedule interval %q", s.Interval)
	}
	return fmt.Sprintf("%d %d %s %s %s %s", m, h, dom, mon, dow,
		strings.Join(execArgs(spec), " ")), nil
}

// cronWeekday maps a day name to cron's 0=Sunday numbering.
func cronWeekday(day string) (string, error) {
	order := map[string]int{
		"sunday": 0, "monday": 1, "tuesday": 2, "wednesday": 3,
		"thursday": 4, "friday": 5, "saturday": 6,
	}
	if d, ok := order[strings.ToLower(strings.TrimSpace(day))]; ok {
		return strconv.Itoa(d), nil
	}
	return "", fmt.Errorf("unknown weekday %q", day)
}

// clockParts splits a HH:MM[:SS] string.
func clockParts(t string) (hour, minute int, err error) {
	normalized, err := clock(t)
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Split(normalized, ":")
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	return h, m, nil
}

// Install writes the artifacts and returns the paths written. Files are written
// with 0644 and their directory created if needed; nothing is enabled, because
// enabling a timer is the user's decision, not a side effect.
func Install(artifacts []Artifact) ([]string, error) {
	var written []string
	sort.SliceStable(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	for _, a := range artifacts {
		if a.Path == "" {
			continue // cron lines are printed, not written
		}
		if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
			return written, fmt.Errorf("create %s: %w", filepath.Dir(a.Path), err)
		}
		if err := os.WriteFile(a.Path, []byte(a.Body), 0o644); err != nil {
			return written, fmt.Errorf("write %s: %w", a.Path, err)
		}
		written = append(written, a.Path)
	}
	if len(written) == 0 {
		return nil, fmt.Errorf("nothing to install: the selected kind has no files (cron lines are printed instead)")
	}
	return written, nil
}

// EnableHint returns the command a user runs to activate the installed units.
func EnableHint(kind Kind, spec Spec) string {
	switch kind {
	case KindSystemd:
		name := unitName(spec)
		return fmt.Sprintf("systemctl --user daemon-reload && systemctl --user enable --now %s.timer", name)
	case KindLaunchd:
		return "launchctl load -w ~/Library/LaunchAgents/*.plist"
	case KindCron:
		return "crontab -e   # then paste the line printed above"
	default:
		return ""
	}
}
