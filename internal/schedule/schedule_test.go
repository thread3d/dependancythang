package schedule_test

import (
	"strings"
	"testing"

	"github.com/thread3d/dependancythang/internal/config"
	"github.com/thread3d/dependancythang/internal/schedule"
)

func spec() schedule.Spec {
	return schedule.Spec{
		RepoID:     "abc123def456",
		RepoPath:   "/home/u/src/app",
		Binary:     "/home/u/.local/bin/depmcp",
		ConfigPath: "/home/u/.config/depmcp/config.yaml",
		StateDir:   "/home/u/.local/share/depmcp",
		Path:       "/usr/bin:/bin:/home/u/.local/share/pnpm/bin",
		Schedule: config.Schedule{
			Interval: "weekly", Day: "monday", Time: "06:00", Timezone: "Europe/Copenhagen",
		},
	}
}

func TestSystemdUnits(t *testing.T) {
	artifacts, err := schedule.Generate(spec(), schedule.KindSystemd)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("artifacts = %d, want a service and a timer", len(artifacts))
	}
	byName := map[string]string{}
	for _, a := range artifacts {
		byName[a.Name] = a.Body
		if !strings.HasSuffix(a.Path, a.Name) {
			t.Errorf("artifact %s has path %s", a.Name, a.Path)
		}
	}

	service, ok := byName["depmcp-abc123def456.service"]
	if !ok {
		t.Fatalf("service unit missing; got %v", keys(byName))
	}
	for _, want := range []string{
		"Type=oneshot",
		"ExecStart=/home/u/.local/bin/depmcp run --repo /home/u/src/app",
		"Environment=PATH=/usr/bin:/bin:/home/u/.local/share/pnpm/bin", // pnpm must be reachable
		"Environment=TZ=Europe/Copenhagen",
		"Environment=DEPMCP_CONFIG=/home/u/.config/depmcp/config.yaml",
	} {
		if !strings.Contains(service, want) {
			t.Errorf("service unit missing %q:\n%s", want, service)
		}
	}

	timer, ok := byName["depmcp-abc123def456.timer"]
	if !ok {
		t.Fatalf("timer unit missing; got %v", keys(byName))
	}
	for _, want := range []string{
		"OnCalendar=Mon 06:00:00",
		"Persistent=true",        // missed slots fire once on wake
		"RandomizedDelaySec=10m", // do not hammer registries at :00
		"Unit=depmcp-abc123def456.service",
		"WantedBy=timers.target",
	} {
		if !strings.Contains(timer, want) {
			t.Errorf("timer unit missing %q:\n%s", want, timer)
		}
	}
}

func TestSystemdCalendars(t *testing.T) {
	cases := map[string]struct {
		s    config.Schedule
		want string
	}{
		"daily":   {config.Schedule{Interval: "daily", Time: "07:30"}, "*-*-* 07:30:00"},
		"monthly": {config.Schedule{Interval: "monthly", Time: "01:15"}, "*-*-01 01:15:00"},
		"weekly":  {config.Schedule{Interval: "weekly", Day: "friday", Time: "23:05"}, "Fri 23:05:00"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := spec()
			s.Schedule = tc.s
			artifacts, err := schedule.Generate(s, schedule.KindSystemd)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			body := artifacts[1].Body
			if !strings.Contains(body, "OnCalendar="+tc.want) {
				t.Errorf("timer OnCalendar missing %q:\n%s", tc.want, body)
			}
		})
	}
}

func TestCronRefusesUnknownInterval(t *testing.T) {
	s := spec()
	s.Schedule = config.Schedule{Interval: "cron", Cron: "*/5 * * * *"}
	if _, err := schedule.Generate(s, schedule.KindSystemd); err == nil {
		t.Error("a cron expression was accepted as an OnCalendar timer")
	}
	// ... but the cron artifact handles it.
	artifacts, err := schedule.Generate(s, schedule.KindCron)
	if err != nil {
		t.Fatalf("Generate cron: %v", err)
	}
	if !strings.Contains(artifacts[0].Body, "*/5 * * * * /home/u/.local/bin/depmcp run") {
		t.Errorf("cron line = %q", artifacts[0].Body)
	}
}

func TestCronLineWeekly(t *testing.T) {
	artifacts, err := schedule.Generate(spec(), schedule.KindCron)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := "0 6 * * 1 /home/u/.local/bin/depmcp run --repo /home/u/src/app"
	if !strings.Contains(artifacts[0].Body, want) {
		t.Errorf("cron line = %q, want it to contain %q", artifacts[0].Body, want)
	}
	if artifacts[0].Path != "" {
		t.Error("a cron artifact should be printed, not written to a file")
	}
}

func TestLaunchdPlist(t *testing.T) {
	artifacts, err := schedule.Generate(spec(), schedule.KindLaunchd)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body := artifacts[0].Body
	for _, want := range []string{
		"<string>dev.depmcp.abc123def456</string>",
		"<key>Weekday</key>", "<integer>1</integer>", // monday
		"<key>Hour</key>", "<integer>6</integer>",
		"<key>Minute</key>", "<integer>0</integer>",
		"<key>TZ</key>", "<string>Europe/Copenhagen</string>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plist missing %q:\n%s", want, body)
		}
	}
	if !strings.HasSuffix(artifacts[0].Path, ".plist") {
		t.Errorf("launchd path = %q", artifacts[0].Path)
	}
}

func TestMissingScheduleIsAnError(t *testing.T) {
	s := spec()
	s.Schedule = config.Schedule{}
	if _, err := schedule.Generate(s, schedule.KindSystemd); err == nil {
		t.Error("expected an error for an unscheduled repository")
	}
}

func TestDefaultPathIncludesPnpm(t *testing.T) {
	got := schedule.DefaultPath()
	if !strings.Contains(got, "/usr/bin") {
		t.Errorf("PATH %q lacks the system path", got)
	}
	if !strings.Contains(got, "pnpm") {
		t.Errorf("PATH %q lacks the pnpm location systemd would miss", got)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
