package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thread3d/dependancythang/internal/audit"
	"github.com/thread3d/dependancythang/internal/config"
	"github.com/thread3d/dependancythang/internal/notify"
	"github.com/thread3d/dependancythang/internal/schedule"
	"github.com/thread3d/dependancythang/internal/store"
)

// Codes introduced by scheduling.
const (
	CodeRepoSuspended  = "REPO_SUSPENDED"
	CodeNotConfigured  = "SCHEDULE_NOT_CONFIGURED"
	CodeNothingToDo    = "NOTHING_TO_DO"
	CodeAutoPRDisabled = "AUTO_PR_DISABLED"
)

// RunOptions tunes an unattended run. AutoPR is reserved for M6, where a forge
// can actually be opened; today the run always stops at a verified branch.
type RunOptions struct {
	AutoPR bool
}

// PauseAfterFailures is how many consecutive failures suspend a repository
// before a human looks at it (§12).
const PauseAfterFailures = 3

// RunOutcome reports what one unattended run did.
type RunOutcome struct {
	Repository RepoView        `json:"repository"`
	Plan       *PlannedSummary `json:"plan,omitempty"`
	Run        *store.Run      `json:"run,omitempty"`
	Verified   bool            `json:"verified"`
	Failures   int             `json:"consecutive_failures"`
	Suspended  bool            `json:"suspended,omitempty"`
	Notified   bool            `json:"notified"`
	Summary    string          `json:"summary"`
}

// PlannedSummary is the small part of a plan a notification needs.
type PlannedSummary struct {
	PlanID  string `json:"plan_id"`
	Changes int    `json:"changes"`
	Groups  int    `json:"groups"`
}

// notifier assembles the configured notification channels.
func (s *Service) notifier() notify.Notifier {
	if s.Notifier != nil {
		return s.Notifier
	}
	var channels []notify.Notifier
	// A durable log is always written: an unattended run on a headless machine
	// has no popup to rely on.
	channels = append(channels, notify.File{Dir: filepath.Join(s.stateDir, "notifications")})
	if s.Cfg.Notify.Desktop {
		channels = append(channels, notify.Desktop{})
	}
	if s.Cfg.Notify.Ntfy != nil && strings.TrimSpace(s.Cfg.Notify.Ntfy.Topic) != "" {
		channels = append(channels, &notify.Ntfy{
			Server: s.Cfg.Notify.Ntfy.Server,
			Topic:  s.Cfg.Notify.Ntfy.Topic,
		})
	}
	return notify.Multi{Notifiers: channels}
}

// RunOnce is the scheduled entry point: plan, apply, verify, and tell somebody.
//
// It deliberately stops before opening a pull request unless `auto_pr` is set,
// because an unattended run should not speak to a remote on its own (§12).
func (s *Service) RunOnce(ctx context.Context, repoRef string, _ RunOptions) (RunOutcome, error) {
	repo, repoPath, err := s.repoByID(repoRef)
	if err != nil {
		return RunOutcome{}, err
	}
	repoID := config.RepoID(repoPath)
	out := RunOutcome{
		Repository: RepoView{ID: repoID, Path: repoPath, Forge: s.Cfg.ForgeFor(repo)},
	}

	failures, err := s.Store.FailureCount(ctx, repoID)
	if err != nil {
		return out, err
	}
	if failures >= PauseAfterFailures {
		out.Failures = failures
		out.Suspended = true
		out.Summary = fmt.Sprintf("suspended after %d consecutive failures", failures)
		_ = s.notifyRun(ctx, notify.Message{
			Title: "depmcp: run suspended", Repo: repoPath, Status: "suspended",
			Body: out.Summary + "; fix the cause, then reset with `depmcp resume`",
		})
		return out, coded(CodeRepoSuspended, fmt.Errorf("%s: %s", repoPath, out.Summary))
	}

	plan, err := s.PlanUpdates(ctx, repoID)
	if err != nil {
		return s.recordFailure(ctx, out, repoID, "planning", err)
	}
	out.Plan = &PlannedSummary{PlanID: plan.PlanID, Changes: plan.TotalChanges, Groups: len(plan.Groups)}

	if plan.TotalChanges == 0 {
		out.Summary = "no updates planned"
		if _, rerr := s.Store.RecordRunOutcome(ctx, repoID, true, out.Summary); rerr != nil {
			return out, rerr
		}
		out.Notified = s.notifyRun(ctx, notify.Message{
			Title: "depmcp: nothing to do", Repo: repoPath, Status: "ok", Body: out.Summary,
		})
		return out, nil
	}

	run, err := s.ApplyPlan(ctx, plan.PlanID, true)
	if err != nil {
		return s.recordFailure(ctx, out, repoID, "apply", err)
	}
	out.Run = &run

	verifiedRun, _, verr := s.VerifyRun(ctx, run.RunID)
	out.Run = &verifiedRun
	if verr != nil {
		// A failing verification is a normal result, not a crash.
		failed, ferr := s.recordFailure(ctx, out, repoID, "verify", verr)
		if ferr != nil {
			return failed, ferr
		}
		return failed, nil
	}
	out.Verified = true

	if _, rerr := s.Store.RecordRunOutcome(ctx, repoID, true, "verified"); rerr != nil {
		return out, rerr
	}
	out.Summary = fmt.Sprintf("%d change(s) applied on %s and verified", plan.TotalChanges, run.Branch)
	if !s.Cfg.Defaults.AutoPR {
		out.Summary += "; no pull request (auto_pr is false)"
	}
	out.Notified = s.notifyRun(ctx, notify.Message{
		Title: "depmcp: update ready", Repo: repoPath, RunID: run.RunID,
		Status: "verified", Body: out.Summary,
	})
	return out, nil
}

// recordFailure bookkeeps a failed unattended run and reports it.
func (s *Service) recordFailure(ctx context.Context, out RunOutcome, repoID, stage string, cause error) (RunOutcome, error) {
	detail := stage + ": " + cause.Error()
	count, rerr := s.Store.RecordRunOutcome(ctx, repoID, false, detail)
	if rerr != nil {
		return out, rerr
	}
	out.Failures = count
	out.Summary = detail
	if count >= PauseAfterFailures {
		out.Suspended = true
		out.Summary += fmt.Sprintf(" (suspended after %d consecutive failures)", count)
	}
	out.Notified = s.notifyRun(ctx, notify.Message{
		Title: "depmcp: run failed", Repo: out.Repository.Path,
		Status: "failed", Body: out.Summary,
	})
	// A failing unattended run is a result, not a crash: report it in the outcome
	// so the caller can exit non-zero without an error string.
	return out, nil
}

// notifyRun sends one message and never fails the run for it.
func (s *Service) notifyRun(ctx context.Context, msg notify.Message) bool {
	msg.At = time.Now().UTC()
	if err := s.notifier().Send(ctx, msg); err != nil {
		_ = s.Audit.Write(audit.Record{Event: "notify.failed", Result: "error", Error: err.Error()})
		return false
	}
	return true
}

// ScheduleSpec resolves a repository's scheduling intent.
func (s *Service) ScheduleSpec(repoRef string) (schedule.Spec, error) {
	repo, repoPath, err := s.repoByID(repoRef)
	if err != nil {
		return schedule.Spec{}, err
	}
	sched := s.Cfg.Defaults.Schedule
	if repo.Schedule != nil {
		sched = *repo.Schedule
	}
	if strings.TrimSpace(sched.Interval) == "" {
		return schedule.Spec{}, coded(CodeNotConfigured, fmt.Errorf(
			"%s has no schedule; set defaults.schedule or a per-repository schedule", repoPath))
	}
	binary, err := os.Executable()
	if err != nil {
		return schedule.Spec{}, fmt.Errorf("resolve the depmcp binary: %w", err)
	}
	return schedule.Spec{
		RepoID:     config.RepoID(repoPath),
		RepoPath:   repoPath,
		Schedule:   sched,
		Binary:     binary,
		ConfigPath: s.ConfigPath,
		StateDir:   s.stateDir,
		Path:       schedule.DefaultPath(),
		Jitter:     "10m",
	}, nil
}

// ScheduleArtifacts generates the OS artifacts for a repository.
func (s *Service) ScheduleArtifacts(repoRef string, kind schedule.Kind) ([]schedule.Artifact, schedule.Spec, error) {
	spec, err := s.ScheduleSpec(repoRef)
	if err != nil {
		return nil, spec, err
	}
	artifacts, err := schedule.Generate(spec, kind)
	return artifacts, spec, err
}

// InstallSchedule writes the artifacts and returns where they went, plus the
// command that activates them.
func (s *Service) InstallSchedule(repoRef string, kind schedule.Kind) ([]string, string, error) {
	artifacts, spec, err := s.ScheduleArtifacts(repoRef, kind)
	if err != nil {
		return nil, "", err
	}
	written, err := schedule.Install(artifacts)
	if err != nil {
		return written, "", err
	}
	return written, schedule.EnableHint(kind, spec), nil
}

// Resume clears a repository's failure count after a human has looked at it.
func (s *Service) Resume(ctx context.Context, repoRef string) (int, error) {
	_, repoPath, err := s.repoByID(repoRef)
	if err != nil {
		return 0, err
	}
	repoID := config.RepoID(repoPath)
	if _, err := s.Store.RecordRunOutcome(ctx, repoID, true, "resumed by user"); err != nil {
		return 0, err
	}
	count, err := s.Store.FailureCount(ctx, repoID)
	return count, err
}

// ScheduledReports describes every tracked repository's scheduling state, for
// `status` and the MCP status tool.
type ScheduledReport struct {
	Repository RepoView `json:"repository"`
	Schedule   string   `json:"schedule,omitempty"`
	Failures   int      `json:"consecutive_failures"`
	Suspended  bool     `json:"suspended,omitempty"`
}

// Scheduling summarises scheduling state for all repositories.
func (s *Service) Scheduling(ctx context.Context) ([]ScheduledReport, error) {
	views, err := s.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ScheduledReport, 0, len(views))
	for _, v := range views {
		repo, ok := s.Cfg.Find(v.Path)
		if !ok {
			continue
		}
		sched := s.Cfg.Defaults.Schedule
		if repo.Schedule != nil {
			sched = *repo.Schedule
		}
		failures, ferr := s.Store.FailureCount(ctx, v.ID)
		if ferr != nil {
			return nil, ferr
		}
		out = append(out, ScheduledReport{
			Repository: v,
			Schedule:   describe(sched),
			Failures:   failures,
			Suspended:  failures >= PauseAfterFailures,
		})
	}
	return out, nil
}

// describe renders a schedule as one line.
func describe(s config.Schedule) string {
	if strings.TrimSpace(s.Interval) == "" {
		return ""
	}
	if s.Interval == "cron" {
		return "cron " + s.Cron
	}
	parts := []string{s.Interval}
	if s.Day != "" {
		parts = append(parts, s.Day)
	}
	if s.Time != "" {
		parts = append(parts, s.Time)
	}
	if s.Timezone != "" {
		parts = append(parts, "("+s.Timezone+")")
	}
	return strings.Join(parts, " ")
}
