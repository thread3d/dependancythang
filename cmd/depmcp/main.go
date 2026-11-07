// Command depmcp is a local dependency updater exposed over the Model Context
// Protocol. This binary has no daemon and opens no socket (specification §2):
// `depmcp serve` speaks MCP on stdin/stdout, and the other subcommands are
// one-shot operations a systemd timer can invoke.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/thread3d/dependancythang/internal/app"
	"github.com/thread3d/dependancythang/internal/config"
	"github.com/thread3d/dependancythang/internal/mcpserver"
	"github.com/thread3d/dependancythang/internal/schedule"
	"github.com/thread3d/dependancythang/internal/store"
	"github.com/thread3d/dependancythang/internal/version"
)

const usage = `depmcp — local dependency updater over MCP

Usage:
  depmcp serve [--stdio]     Run the MCP server on stdin/stdout
  depmcp list                List tracked repositories (JSON)
  depmcp add <path> [--forge F] [--remote R]
                             Track a git repository
  depmcp remove <path>       Stop tracking a repository
  depmcp scan <repo>         Inventory dependencies (JSON)
  depmcp check <repo>        Available updates (JSON)
  depmcp vulns <repo>        Advisories affecting current versions (JSON)
  depmcp plan <repo>         Build an update plan (JSON)
  depmcp apply <plan-id> --confirm
                             Apply a plan on a new branch (worktree)
  depmcp verify <run-id>     Build and test a run's branch in the sandbox
  depmcp revert <run-id>     Delete a run's branch and worktree
  depmcp runs [repo]         Recent apply runs
  depmcp run --repo <repo>   One-shot scheduled run: plan, apply, verify, notify
  depmcp schedule install --repo <repo> [--kind systemd|launchd|cron]
                             Write the timer/agent that runs depmcp run
  depmcp schedule print --repo <repo> [--kind ...]
                             Print the artifact instead of writing it
  depmcp resume --repo <repo> Clear a repository's failure count
  depmcp doctor              Check the environment
  depmcp status              Show state directory and database statistics
  depmcp validate [file]     Validate a configuration file (default: the global one)
  depmcp version             Print version information

Environment:
  DEPMCP_CONFIG     Override the global config path
  DEPMCP_STATE_DIR  Override the state directory
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(ctx, os.Args[2:])
	case "list":
		err = runList(ctx)
	case "add":
		err = runAdd(ctx, os.Args[2:])
	case "remove":
		err = runRemove(ctx, os.Args[2:])
	case "scan":
		err = runScan(ctx, os.Args[2:])
	case "check":
		err = runCheck(ctx, os.Args[2:])
	case "vulns":
		err = runVulns(ctx, os.Args[2:])
	case "plan":
		err = runPlan(ctx, os.Args[2:])
	case "apply":
		err = runApply(ctx, os.Args[2:])
	case "verify":
		err = runVerify(ctx, os.Args[2:])
	case "revert":
		err = runRevert(ctx, os.Args[2:])
	case "runs":
		err = runRuns(ctx, os.Args[2:])
	case "run":
		err = runOneShot(ctx, os.Args[2:])
	case "schedule":
		err = runSchedule(ctx, os.Args[2:])
	case "resume":
		err = runResume(ctx, os.Args[2:])
	case "doctor":
		err = runDoctor(ctx)
	case "status":
		err = runStatus(ctx)
	case "validate":
		err = runValidate(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("depmcp %s (schema %d)\n", version.String(), store.SchemaVersion)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		code := app.CodeOf(err)
		if code != "" {
			fmt.Fprintf(os.Stderr, "depmcp: %s: %v\n", code, err)
		} else {
			fmt.Fprintf(os.Stderr, "depmcp: %v\n", err)
		}
		os.Exit(1)
	}
}

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	stdio := fs.Bool("stdio", true, "serve MCP over stdin/stdout (the only transport in v1)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*stdio {
		return errors.New("only --stdio is supported: rev 2 has no HTTP transport (§2)")
	}

	svc, err := app.Open(ctx)
	if err != nil {
		return err
	}
	defer svc.Close()

	// stdout carries protocol frames only; every diagnostic goes to stderr.
	fmt.Fprintf(os.Stderr, "depmcp %s serving MCP on stdio (state %s)\n",
		version.String(), svc.StateDir())

	server := mcpserver.New(svc)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
}

func runList(ctx context.Context) error {
	svc, err := app.Open(ctx)
	if err != nil {
		return err
	}
	defer svc.Close()

	repos, err := svc.ListRepositories(ctx)
	if err != nil {
		return err
	}
	return emitJSON(map[string]any{"repositories": repos, "total": len(repos)})
}

func runAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	forge := fs.String("forge", "", "local | github | gitlab | gitea")
	remote := fs.String("remote", "", "git remote name")
	if err := parseAnyOrder(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("add requires exactly one path")
	}

	svc, err := app.Open(ctx)
	if err != nil {
		return err
	}
	defer svc.Close()

	view, err := svc.AddRepository(ctx, app.AddRepositoryRequest{
		Path: fs.Arg(0), Forge: *forge, Remote: *remote,
	})
	if err != nil {
		return err
	}
	return emitJSON(view)
}

func runRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	if err := parseAnyOrder(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("remove requires exactly one path")
	}

	svc, err := app.Open(ctx)
	if err != nil {
		return err
	}
	defer svc.Close()

	changed, err := svc.RemoveRepository(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	return emitJSON(map[string]any{"removed": changed, "path": fs.Arg(0)})
}

func runDoctor(ctx context.Context) error {
	svc, err := app.Open(ctx)
	if err != nil {
		return err
	}
	defer svc.Close()

	checks := svc.Doctor(ctx)
	failed := 0
	for _, c := range checks {
		mark := "ok  "
		switch c.Status {
		case "warn":
			mark = "warn"
		case "fail":
			mark = "FAIL"
			failed++
		}
		fmt.Printf("%s  %-14s %s\n", mark, c.Name, c.Detail)
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

func runStatus(ctx context.Context) error {
	svc, err := app.Open(ctx)
	if err != nil {
		return err
	}
	defer svc.Close()

	stats, err := svc.Store.Stats(ctx)
	if err != nil {
		return err
	}
	return emitJSON(map[string]any{
		"state_dir":   svc.StateDir(),
		"config_path": svc.ConfigPath,
		"read_only":   svc.ReadOnly(),
		"stats":       stats,
		"held_locks":  svc.HeldLocks(),
	})
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	if err := parseAnyOrder(fs, args); err != nil {
		return err
	}
	path := fs.Arg(0)
	if path == "" {
		p, err := config.GlobalPath()
		if err != nil {
			return err
		}
		path = p
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	issues := config.Validate(data)
	if len(issues) == 0 {
		fmt.Printf("%s: valid\n", path)
		return nil
	}
	for _, issue := range issues {
		fmt.Printf("%s: %s\n", path, issue.String())
	}
	return fmt.Errorf("%d issue(s)", len(issues))
}

// parseAnyOrder parses flags and positional arguments regardless of order.
// Go's flag package stops at the first non-flag argument, which would make
// "depmcp add <path> --forge github" fail in a way users find baffling.
func parseAnyOrder(fs *flag.FlagSet, args []string) error {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") {
			continue
		}
		// A non-boolean flag consumes the following argument as its value.
		name := strings.TrimLeft(a, "-")
		if f := fs.Lookup(name); f != nil {
			if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !bf.IsBoolFlag() {
				if i+1 < len(args) {
					flags = append(flags, args[i+1])
					i++
				}
			}
		}
	}
	return fs.Parse(append(flags, positional...))
}

// withRepo runs a read-path operation for the repository named on the command
// line, opening and closing the service around it.
func withRepo(ctx context.Context, args []string, name string, fn func(*app.Service, string) error) error {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	if err := parseAnyOrder(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%s requires exactly one repository id or path", name)
	}
	svc, err := app.Open(ctx)
	if err != nil {
		return err
	}
	defer svc.Close()
	return fn(svc, fs.Arg(0))
}

func runScan(ctx context.Context, args []string) error {
	return withRepo(ctx, args, "scan", func(svc *app.Service, ref string) error {
		target, deps, err := svc.ScanDependencies(ctx, ref)
		if err != nil {
			return err
		}
		direct := 0
		for _, d := range deps {
			if d.Direct {
				direct++
			}
		}
		return emitJSON(map[string]any{
			"repository": target, "total": len(deps), "direct": direct,
			"dependencies": deps,
		})
	})
}

func runCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	indirect := fs.Bool("indirect", false, "also resolve transitive dependencies")
	limit := fs.Int("limit", 0, "maximum candidates to resolve")
	if err := parseAnyOrder(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("check requires exactly one repository id or path")
	}
	svc, err := app.Open(ctx)
	if err != nil {
		return err
	}
	defer svc.Close()

	_, cands, err := svc.CheckUpdates(ctx, fs.Arg(0), *indirect, *limit)
	if err != nil {
		return err
	}
	return emitJSON(map[string]any{"candidates": cands, "total": len(cands)})
}

func runVulns(ctx context.Context, args []string) error {
	return withRepo(ctx, args, "vulns", func(svc *app.Service, ref string) error {
		_, vulns, err := svc.CheckVulnerabilities(ctx, ref)
		if err != nil {
			return err
		}
		return emitJSON(map[string]any{"vulnerabilities": vulns, "total": len(vulns)})
	})
}

func runPlan(ctx context.Context, args []string) error {
	return withRepo(ctx, args, "plan", func(svc *app.Service, ref string) error {
		plan, err := svc.PlanUpdates(ctx, ref)
		if err != nil {
			return err
		}
		return emitJSON(plan)
	})
}

func runApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	confirm := fs.Bool("confirm", false, "confirm applying the plan on a new branch")
	if err := parseAnyOrder(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("apply requires exactly one plan id")
	}
	svc, err := app.Open(ctx)
	if err != nil {
		return err
	}
	defer svc.Close()

	run, err := svc.ApplyPlan(ctx, fs.Arg(0), *confirm)
	if err != nil {
		return err
	}
	return emitJSON(run)
}

func runVerify(ctx context.Context, args []string) error {
	return withRepo(ctx, args, "verify", func(svc *app.Service, runID string) error {
		run, result, err := svc.VerifyRun(ctx, runID)
		if err != nil {
			// Report the result even when verification failed: the failure is
			// the useful output, not an error to swallow.
			if result.Summary != "" {
				_ = emitJSON(map[string]any{"run": run, "result": result})
			}
			return err
		}
		return emitJSON(map[string]any{"run": run, "result": result})
	})
}

func runRevert(ctx context.Context, args []string) error {
	return withRepo(ctx, args, "revert", func(svc *app.Service, runID string) error {
		run, err := svc.RevertRun(ctx, runID)
		if err != nil {
			return err
		}
		return emitJSON(run)
	})
}

func runRuns(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("runs", flag.ExitOnError)
	limit := fs.Int("limit", 20, "maximum runs to list")
	if err := parseAnyOrder(fs, args); err != nil {
		return err
	}
	svc, err := app.Open(ctx)
	if err != nil {
		return err
	}
	defer svc.Close()

	repo := ""
	if fs.NArg() == 1 {
		repo = fs.Arg(0)
	}
	runs, err := svc.ListRuns(ctx, repo, *limit)
	if err != nil {
		return err
	}
	return emitJSON(map[string]any{"runs": runs, "total": len(runs)})
}

func runOneShot(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	repo := fs.String("repo", "", "repository id or path")
	autoPR := fs.Bool("auto-pr", false, "open a pull request when verification passes")
	if err := parseAnyOrder(fs, args); err != nil {
		return err
	}
	if strings.TrimSpace(*repo) == "" {
		return errors.New("run requires --repo")
	}
	svc, err := app.Open(ctx)
	if err != nil {
		return err
	}
	defer svc.Close()

	outcome, err := svc.RunOnce(ctx, *repo, app.RunOptions{AutoPR: *autoPR})
	if err != nil && outcome.Summary == "" {
		return err
	}
	if emitErr := emitJSON(outcome); emitErr != nil {
		return emitErr
	}
	// A failed or suspended run exits non-zero so a timer's journal shows it,
	// but the outcome has already been reported.
	if outcome.Suspended || (outcome.Run != nil && outcome.Run.Status != "verified") || err != nil {
		return errors.New("run did not complete successfully")
	}
	return nil
}

func runSchedule(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("schedule requires a subcommand: install or print")
	}
	sub := args[0]
	fs := flag.NewFlagSet("schedule "+sub, flag.ExitOnError)
	repo := fs.String("repo", "", "repository id or path")
	kind := fs.String("kind", "systemd", "systemd | launchd | cron")
	if err := parseAnyOrder(fs, args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*repo) == "" {
		return errors.New("schedule requires --repo")
	}
	svc, err := app.Open(ctx)
	if err != nil {
		return err
	}
	defer svc.Close()

	switch sub {
	case "install":
		written, hint, err := svc.InstallSchedule(*repo, schedule.Kind(*kind))
		if err != nil {
			return err
		}
		return emitJSON(map[string]any{
			"written": written, "kind": *kind, "activate": hint,
		})
	case "print":
		artifacts, _, err := svc.ScheduleArtifacts(*repo, schedule.Kind(*kind))
		if err != nil {
			return err
		}
		if *kind == "cron" {
			for _, a := range artifacts {
				fmt.Print(a.Body)
			}
			return nil
		}
		for _, a := range artifacts {
			fmt.Printf("# --- %s ---\n%s", a.Name, a.Body)
		}
		return nil
	default:
		return fmt.Errorf("unknown schedule subcommand %q", sub)
	}
}

func runResume(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	repo := fs.String("repo", "", "repository id or path")
	if err := parseAnyOrder(fs, args); err != nil {
		return err
	}
	if strings.TrimSpace(*repo) == "" {
		return errors.New("resume requires --repo")
	}
	svc, err := app.Open(ctx)
	if err != nil {
		return err
	}
	defer svc.Close()

	count, err := svc.Resume(ctx, *repo)
	if err != nil {
		return err
	}
	return emitJSON(map[string]any{"repository": *repo, "consecutive_failures": count})
}

func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
