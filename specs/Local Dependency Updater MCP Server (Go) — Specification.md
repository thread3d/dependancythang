# Local Dependency Updater MCP Server (Go) — Specification

Nov 7, 2026 · @Julian Roland Parramore

## 1. Overview

`depmcp` (working name) is a single Go binary that lets any MCP client find outdated and vulnerable dependencies in your local repositories and turn them into tested, reviewable update branches: Dependabot's workflow, with no hosted service and no code leaving your machine except registry and advisory lookups.

**Goals**

- Detect outdated and vulnerable dependencies across many local repositories and ecosystems.
- Produce update plans (grouped, policy-filtered), then apply them on isolated git branches or worktrees.
- Verify each update by running the project's build and tests in a sandbox before anything is proposed.
- Optionally open pull requests on GitHub, GitLab, Gitea/Forgejo, or stay fully local.
- Expose all of this as MCP tools, resources, and prompts, so an LLM client can triage, explain, and drive it.
- Read existing `.github/dependabot.yml` files so migration costs nothing.

**Non-goals**

- No hosted or multi-tenant mode, no web UI, no telemetry.
- No replacement for a full SCA or SBOM platform.
- No auto-merging to a default branch in v1.

**Design principles**

- The user's working tree is never touched: all writes happen in git worktrees.
- Read tools are free; every write tool is annotated, auditable, and gated.
- Everything that comes from the network (release notes, changelogs, advisories) is untrusted data.
- One static binary, pure-Go dependencies, no CGO, runs on Linux, macOS, and Windows.

## 2. What the request left out

The biggest gap is that MCP servers are passive and Dependabot is proactive; the rest are decisions this spec makes for you, listed so you can overrule them. Each row names the default the spec assumes.

| # | Gap | Why it matters | Spec default |
| --- | --- | --- | --- |
| 1 | **Trigger model** | An MCP server only answers when a client calls it, so nothing runs "every Monday" by itself. | Two modes: stdio server for interactive use, plus a long-running daemon (localhost HTTP) that owns the scheduler and state. |
| 2 | **Supported ecosystems** | Dependabot covers 30+. Each needs its own parser, resolver, and lockfile writer. | v1: Go modules, npm/pnpm/yarn, Docker, GitHub Actions. v2: pip/uv/poetry, Cargo. |
| 3 | **Code execution risk** | Verifying an update means running install scripts, builds, and tests from third-party code. | Sandboxed runner (bubblewrap/container), lifecycle scripts off, network off after fetch. |
| 4 | **Prompt injection** | Release notes and changelogs flow into the LLM's context and can carry instructions. | Mark as untrusted, truncate, strip markup, never auto-act on their content. |
| 5 | **Cooldown / minimum release age** | Most malicious package versions are caught within days; fresh releases are the riskiest. | Default cooldown of 3 days (7 for major), overridable per package. |
| 6 | **Forge and credentials** | PRs need a token; tokens need safe storage. | OS keyring, fine-grained tokens, `local` forge (branches only) as default. |
| 7 | **Private registries** | `GOPRIVATE`, `.npmrc`, private indexes will otherwise fail silently. | Honor the tools' own config files and env; never copy credentials into state. |
| 8 | **Monorepos and workspaces** | Multiple manifests per repo, `go.work`, npm workspaces, shared lockfiles. | Directory-scoped update targets, as in `dependabot.yml`. |
| 9 | **Transitive and indirect deps** | Many advisories live in dependencies you never import directly. | Report all; update direct deps by default, force-bump indirect ones only for advisories. |
| 10 | **Reachability** | A vulnerability in code you never call is lower priority. | Use `govulncheck` for Go; flag "reachability unknown" elsewhere. |
| 11 | **Release-note summaries** | Humans want "what breaks?", not a diff. | Use MCP sampling to have the client's model summarize, with raw text attached. |
| 12 | **Human-in-the-loop** | Write tools must not run on a model's whim. | Tool annotations plus elicitation for confirmation on every write. |
| 13 | **Undo** | A bad update needs a clean way back. | Every run is a branch; `revert_run` deletes it and its worktree. |
| 14 | **Notifications** | Unattended scheduled runs produce results nobody sees. | Desktop notification and an optional ntfy/webhook target. |
| 15 | **License and health checks** | Updates can change a package's license or pull in abandoned or deprecated code. | Report license changes, deprecation flags, and yanked versions in the plan. |
| 16 | **Rate limits and offline use** | Registry and advisory APIs throttle; laptops go offline. | HTTP cache with ETags, backoff, and an offline mode that serves cached data. |
| 17 | **Config portability** | You likely already have `dependabot.yml` files. | Import them read-only and map to native config. |
| 18 | **Cross-platform and service install** | A daemon needs to start at login. | systemd user unit (your Fedora box), launchd, and a Windows service template. |
| 19 | **Audit log** | You need to know what ran, when, and why. | Append-only JSONL log of every tool call and write. |
| 20 | **MCP SDK choice** | The server's foundation. | The official Go SDK (`github.com/modelcontextprotocol/go-sdk`); verify its current version before starting. |

If you answer only three, make them #1 (daemon or not), #2 (ecosystems), and #6 (local branches only, or real PRs).

## 3. Architecture

One binary, two run modes sharing one core library: `depmcp serve --stdio` for a client that launches it on demand, and `depmcp daemon` for a long-lived process on `127.0.0.1` that serves MCP over Streamable HTTP and also runs the scheduler.

**Process model**

- **stdio mode:** started by the MCP client, reads state from SQLite, exits with the client. No scheduler. stdout carries only protocol frames; all logs go to stderr.
- **daemon mode:** one process owns the scheduler, job queue, and write lock. Clients connect over HTTP with a bearer token.
- **Single-writer rule:** a file lock in the state directory ensures only one process mutates a given repository at a time. A stdio instance that finds the daemon running forwards write calls to it.

**Go package layout**

```text
cmd/depmcp/            main, CLI (serve, daemon, scan, doctor, import-dependabot)
internal/mcpserver/    tool, resource, prompt registration; schemas from Go structs
internal/config/       YAML load, validation, dependabot.yml import
internal/repo/         repository registry, discovery, git worktree management
internal/ecosystem/    Ecosystem interface + gomod/, npm/, docker/, actions/, pip/, cargo/
internal/registry/     package registry clients (proxy.golang.org, npm, Docker, ...) + HTTP cache
internal/advisory/     OSV client, govulncheck runner, severity normalization
internal/policy/       semver rules, cooldown, grouping, ignore rules
internal/planner/      builds an UpdatePlan from inventory + advisories + policy
internal/applier/      writes manifests/lockfiles in a worktree, commits
internal/verify/       sandboxed build/test runner
internal/forge/        Forge interface + github/, gitlab/, gitea/, local/
internal/store/        SQLite (modernc.org/sqlite, no CGO), migrations
internal/scheduler/    cron-style trigger, job queue, backoff
internal/secrets/      OS keyring wrapper
internal/audit/        append-only JSONL audit log
```

**Key dependencies**

| Need | Choice | Note |
| --- | --- | --- |
| MCP | official Go SDK | Provides stdio and Streamable HTTP transports, sampling, elicitation. |
| Go modules | `golang.org/x/mod` | `modfile` and `semver` parse and edit `go.mod` safely. |
| Other semver | `Masterminds/semver/v3` | npm and Cargo ranges differ from Go's; each ecosystem wraps its own rules. |
| Git | shell out to `git` | go-git's worktree support is incomplete; `git worktree` is the right tool. |
| Storage | `modernc.org/sqlite` | Pure Go, keeps the build CGO-free. |
| Config | `gopkg.in/yaml.v3` | Strict decoding, unknown keys are errors. |
| Secrets | `zalando/go-keyring` | Secret Service on Linux, Keychain on macOS, Credential Manager on Windows. |
| Logging | `log/slog` | JSON to stderr, with a request ID per tool call. |

Each ecosystem, forge, and advisory source sits behind a small interface so tests can substitute fakes.

## 4. Ecosystem support

Every package manager is one implementation of a single Go interface, so adding Cargo or Maven later touches no core code.

```go
type Ecosystem interface {
    // Name is the stable identifier used in config, e.g. "gomod", "npm".
    Name() string

    // Detect returns manifest targets found under root (a go.mod, a package.json, ...).
    Detect(ctx context.Context, root string) ([]Target, error)

    // Inventory lists direct and indirect dependencies with current versions.
    Inventory(ctx context.Context, t Target) ([]Dependency, error)

    // Candidates returns available newer versions from the registry.
    Candidates(ctx context.Context, d Dependency) ([]Version, error)

    // Apply rewrites manifest and lockfile inside the given worktree.
    Apply(ctx context.Context, wt Worktree, t Target, ch []Change) (ApplyResult, error)

    // VerifyCommands returns the default build/test commands for the sandbox.
    VerifyCommands(t Target) []Command

    // Compare orders versions under this ecosystem's own semantics.
    Compare(a, b string) int
}
```

**Rollout**

| Phase | Ecosystems | How updates are applied |
| --- | --- | --- |
| v1 | Go modules | `go get pkg@version`, then `go mod tidy`; honors `go.work`, `vendor/`, and `GOPRIVATE`. |
| v1 | npm, pnpm, yarn | Delegate to the detected package manager with `--ignore-scripts`; never hand-edit lockfiles. |
| v1 | Docker | Rewrite `FROM` tags and digests in Dockerfiles and Compose files. |
| v1 | GitHub Actions | Bump `uses:` refs, preferring commit SHA pins with a version comment. |
| v2 | pip, uv, poetry | Update `pyproject.toml` or requirements files, then re-lock with the matching tool. |
| v2 | Cargo | `cargo update -p` plus `Cargo.toml` requirement edits. |
| later | Maven/Gradle, Nix flakes, Terraform, Homebrew | Community-style contributions behind the same interface. |

**Rules shared by all ecosystems**

- Prefer the ecosystem's own tool to edit lockfiles; hand-editing them is the main source of subtle breakage.
- Pre-release versions are skipped unless the current version is itself a pre-release.
- Yanked, retracted, and deprecated versions are never proposed; the plan reports them instead.
- Registry responses are cached on disk with ETags and a per-registry rate limiter.

## 5. MCP surface

The server exposes 18 tools, 6 resources, and 4 prompts. Tools take and return typed JSON (schemas generated from Go structs) and carry MCP annotations so clients can gate the dangerous ones.

**Tools**

| Tool | Purpose | Annotations |
| --- | --- | --- |
| `list_repositories` | Registered repos with last scan time and open update branches. | read-only |
| `add_repository` | Register a path; auto-detect ecosystems and import `dependabot.yml`. | idempotent |
| `remove_repository` | Unregister a repo (does not delete files). | destructive |
| `scan_dependencies` | Inventory direct and indirect dependencies per target. | read-only |
| `check_updates` | Available updates with semver class, age, cooldown status, and advisories. | read-only |
| `check_vulnerabilities` | Advisories affecting current versions, with severity, fixed versions, reachability. | read-only |
| `plan_updates` | Build a grouped `UpdatePlan` under current policy; mutates nothing. Returns `plan_id`. | read-only |
| `get_release_notes` | Fetch and sanitize changelog text between two versions. | read-only, open-world |
| `apply_plan` | Create worktree and branch, apply changes, commit. Needs confirmation. | destructive |
| `verify_run` | Run build and tests for a run inside the sandbox. | open-world |
| `open_pull_request` | Push a verified branch and open a PR through the configured forge. | destructive, open-world |
| `revert_run` | Delete a run's branch and worktree. | destructive |
| `ignore_update` | Add an ignore rule (package, version range, reason, optional expiry). | idempotent |
| `get_config` / `validate_config` | Show effective config; validate a candidate without saving. | read-only |
| `list_runs` / `get_run` | History of runs, their plans, logs, and results. | read-only |
| `run_schedule_now` | Trigger the scheduled job for a repo immediately. | open-world |

**Resources**

- `depmcp://repos` and `depmcp://repos/{id}/dependencies`: current inventory.
- `depmcp://repos/{id}/updates`: latest `check_updates` result, with `resources/updated` notifications when a scheduled scan changes it.
- `depmcp://runs/{id}` and `depmcp://runs/{id}/log`: run record and captured output.
- `depmcp://config`: effective merged configuration, secrets redacted.

**Prompts**

- `triage_vulnerabilities`: rank advisories by severity, reachability, and fix availability, and propose an order.
- `review_updates`: walk a plan group by group, summarize breaking changes, recommend apply or skip.
- `explain_breaking_change`: given a package and version range, read the sanitized notes and list required code changes.
- `weekly_digest`: concise summary of what changed across all repos since the last digest.

**Protocol behaviors**

- **Progress and logging:** long tools send `notifications/progress` and structured log messages.
- **Cancellation:** every tool honors `context` cancellation; a cancelled run leaves a worktree marked `aborted`, never half-committed.
- **Elicitation:** write tools ask the user to confirm with a short diff-style summary; if the client lacks elicitation, they require an explicit `confirm: true` argument.
- **Sampling:** `get_release_notes` can ask the client's model for a breaking-change summary; it is optional, and the raw sanitized text is always returned too.
- **Errors:** tool failures return `isError: true` with a stable error code (`REPO_DIRTY`, `COOLDOWN_ACTIVE`, `SANDBOX_UNAVAILABLE`, ...) and a human-readable hint.

## 6. Update pipeline

Every update flows through the same seven stages, and each stage's output is stored so a run can be inspected or resumed.

1. **Discover.** Walk registered roots, detect manifest targets per ecosystem, and respect `.gitignore` and a configurable exclude list.
2. **Inventory.** Parse manifests and lockfiles into `Dependency{name, current, constraint, direct, target}`.
3. **Resolve candidates.** Query registries, drop pre-releases, yanked, and retracted versions, and classify each candidate as patch, minor, or major under the ecosystem's rules.
4. **Enrich.** Attach advisories (OSV), reachability (`govulncheck` for Go), license changes, deprecation flags, and release age.
5. **Plan.** Apply policy in this order: ignore rules, pinning, cooldown, allowed update types, then grouping. Advisory fixes bypass cooldown and update-type limits unless config says otherwise. The result is an immutable `UpdatePlan` stored under a `plan_id`.
6. **Apply.** Create `git worktree add` on branch `depmcp/<ecosystem>/<group>-<date>`, run the ecosystem's `Apply`, and commit with a conventional message (`build(deps): bump X from a to b`) that lists every change and advisory ID.
7. **Verify.** Run the sandboxed build and tests. Record pass or fail, output, and duration. Only a passing run is eligible for `open_pull_request` unless `allow_unverified` is set.

**Grouping and limits**

- Groups match by name pattern, dependency type (dev or prod), and update type, as in Dependabot's `groups:`.
- A cap on open branches per repo (default 5) prevents flooding; security updates have their own cap.
- A rebase step re-applies a plan on a fresh default branch when the base moves, and supersedes older branches for the same package.

**Failure handling**

- A failing group is bisected: the planner retries with each member alone, so one bad bump does not block the rest.
- Dirty main checkouts are fine (worktrees are separate), but a missing or detached default branch aborts with `NO_DEFAULT_BRANCH`.

## 7. Security advisories

Advisories come from [OSV.dev](https://osv.dev), which aggregates the GitHub Advisory Database, the Go vulnerability database, and ecosystem-specific feeds, so one client covers every ecosystem.

- **Lookup:** batch `POST /v1/querybatch` per inventory, keyed by ecosystem, package, and version; cache results for 6 hours and honor an offline mode that serves stale data with a warning.
- **Normalization:** map every advisory to `{id, aliases, severity (CVSS v3/v4 score and label), affected ranges, fixed versions, published, withdrawn}`; deduplicate by alias (GHSA, CVE, GO-).
- **Reachability (Go):** run `govulncheck` in the sandbox; mark each finding `called`, `imported`, or `unknown`. Other ecosystems report `unknown`.
- **Fix selection:** choose the lowest fixed version that satisfies the constraint and policy; if only a major bump fixes it, flag it for human decision rather than auto-planning it.
- **Security updates vs version updates:** advisory-driven updates are a separate plan category with their own branch cap, labels, and a prominent line in the commit message.
- **Triage state:** each advisory can be `open`, `ignored` (with reason and expiry), or `fixed`; ignoring requires a reason and defaults to a 30-day expiry.
- **Withdrawn advisories:** automatically cleared and logged.

Default severity handling:

| Severity | Cooldown | Update-type limit | Notification |
| --- | --- | --- | --- |
| Critical | bypassed | none | immediate |
| High | bypassed | none | immediate |
| Medium | applies | minor and patch | in digest |
| Low | applies | minor and patch | in digest |

## 8. Configuration

Configuration is layered, later layers winning: built-in defaults, then the global file `$XDG_CONFIG_HOME/depmcp/config.yaml`, then a per-repo `.depmcp.yml` committed in the repository, then tool-call arguments. Decoding is strict; unknown keys are errors, and `validate_config` reports line numbers.

The schema deliberately mirrors `dependabot.yml` keys (`updates`, `directory`, `schedule`, `groups`, `ignore`, `allow`, `cooldown`) so imports are near one-to-one.

```yaml
version: 1

server:
  listen: 127.0.0.1:7391        # daemon mode only
  state_dir: ~/.local/share/depmcp
  offline: false

repositories:
  - path: ~/src/inference-cluster
    forge: local                # local | github | gitlab | gitea
  - path: ~/src/libmdast
    forge: github
    remote: origin

defaults:
  schedule: { interval: weekly, day: monday, time: "06:00", timezone: Europe/Copenhagen }
  cooldown: { default_days: 3, major_days: 7, minor_days: 3, patch_days: 1 }
  max_open_branches: 5
  verify: { enabled: true, timeout: 15m, sandbox: bubblewrap }
  branch_prefix: depmcp
  commit_style: conventional

updates:
  - ecosystem: gomod
    directory: /
    groups:
      golang-x: { patterns: ["golang.org/x/*"] }
    ignore:
      - dependency: github.com/foo/bar
        versions: [">=3.0.0"]
        reason: "v3 drops our Go version"
        until: 2027-03-01
  - ecosystem: npm
    directory: /web
    allow: [{ dependency_type: production }]
    cooldown: { default_days: 5 }

security:
  source: osv
  bypass_cooldown: [critical, high]
  govulncheck: true

notify:
  desktop: true
  ntfy: { topic: depmcp-julian }   # optional
```

**Rules**

- **Schedules:** `daily`, `weekly`, `monthly`, or a cron expression; always evaluated in an explicit time zone.
- **Ignore:** by package, version range, or update type; `reason` is mandatory, `until` is optional, and expired rules are surfaced in the weekly digest.
- **Pinning:** an exact version constraint in the manifest is respected as a pin unless `allow_pinned: true`.
- **Versioning strategy:** `auto` (default), `increase`, `widen`, or `lockfile-only`, mapped per ecosystem.
- **Environment:** secrets are referenced by keyring key (`secret:github-token`), never stored in YAML.
- **Import:** `depmcp import-dependabot <repo>` converts `.github/dependabot.yml` and prints keys it could not map.

## 9. Git and forge integration

All writes happen in a dedicated worktree on a dedicated branch, so your checked-out branch, index, and uncommitted work are never touched. The forge layer is optional: the default `local` forge stops at a verified branch.

**Git behavior**

- Worktrees live under `<state_dir>/worktrees/<repo-id>/<run-id>` and are removed on `revert_run` or after merge detection.
- Branch name: `depmcp/<ecosystem>/<group-or-package>-<YYYYMMDD>`; one branch per group, one commit per package inside it.
- Commit identity comes from the repo's git config, with a `Co-authored-by: depmcp` trailer; commits are signed if the user's config requires it.
- The tool shells out to the user's `git`, inheriting hooks, credential helpers, and signing setup. Hooks are run with `--no-verify` off, so pre-commit failures fail the run visibly.
- Merged or closed upstream branches are detected on each scan and cleaned up.

**Forge interface**

```go
type Forge interface {
    Name() string
    Push(ctx context.Context, repo Repo, branch string) error
    OpenPR(ctx context.Context, repo Repo, pr PullRequest) (PRRef, error)
    UpdatePR(ctx context.Context, ref PRRef, pr PullRequest) error
    ClosePR(ctx context.Context, ref PRRef, reason string) error
    ListManagedPRs(ctx context.Context, repo Repo) ([]PRRef, error)
}
```

| Forge | Auth | Notes |
| --- | --- | --- |
| `local` | none | Branch plus a markdown report under `<state_dir>/reports/`. Default. |
| `github` | fine-grained PAT or GitHub App key in keyring | Labels, reviewers, assignees, draft PRs; ignores forks. |
| `gitlab` | PAT in keyring | Merge requests, self-hosted base URL supported. |
| `gitea` / `forgejo` | token in keyring | Same feature subset as GitHub. |

**Pull request content**

- Title: `build(deps): bump <pkg> from <a> to <b>` or `build(deps): bump the <group> group with N updates`.
- Body: a table of changes, advisory IDs with severity, sanitized release-note excerpts, verification result with log link, and the exact command to revert.
- A footer marks the PR as generated by depmcp, so later runs can recognize and update it. Closed-without-merge PRs create an automatic ignore rule for that version.

## 10. State, scheduling, and daemon mode

All durable state lives in one SQLite file plus a few directories under `$XDG_DATA_HOME/depmcp` (`~/.local/share/depmcp` on Linux), so backup is one copy and reset is one delete.

**State**

| Table | Holds |
| --- | --- |
| `repos` | Registered repositories, forge, last scan, config hash. |
| `dependencies` | Latest inventory snapshot per target. |
| `advisories` | Normalized advisories with triage state and expiry. |
| `plans` | Immutable `UpdatePlan` JSON, keyed by `plan_id`. |
| `runs` | One row per apply, with branch, worktree path, status, verify result, PR reference. |
| `ignores` | Ignore rules, reasons, expiry. |
| `http_cache` | Registry and OSV responses with ETag and expiry. |
| `audit` | Mirror of the JSONL audit log for querying. |

Schema migrations are embedded and forward-only; the binary refuses to start against a newer schema than it knows.

**Run lifecycle**

`planned` → `applying` → `applied` → `verifying` → `verified` or `failed`. A verified run continues `pr_opened` → `merged` or `closed`; any run can end `reverted`. A crash mid-run leaves the run `applying`; on startup the daemon marks such runs `aborted` and offers cleanup.

**Scheduler (daemon only)**

- Cron-style triggers per repo from `schedule:` config, evaluated in the configured time zone.
- A bounded worker pool (default 2) runs jobs; one job per repo at a time.
- Missed runs (laptop asleep) execute once at next wake, not once per missed slot.
- Jitter of up to 10 minutes avoids hammering registries at the top of the hour.
- Scheduled jobs run scan, plan, apply, and verify, but stop short of opening PRs unless `auto_pr: true`; the result appears as a resource update and a notification.
- Backoff with limits on registry errors; a repo with three consecutive failures is paused and reported.

**Daemon operation**

- Binds `127.0.0.1` only; refuses `0.0.0.0` unless `--i-know-what-im-doing` is passed.
- Auth by bearer token generated at first start and stored in the keyring and a mode-0600 file; the client config references it.
- Provided units: systemd user service (`systemctl --user enable --now depmcp`), launchd plist, and a Windows Task Scheduler template.
- `depmcp doctor` checks git, sandbox availability, keyring access, registry reachability, and config validity.

## 11. Security and safety model

The tool runs other people's code on your machine and feeds other people's text to your LLM, so its threat model matters more than its feature list.

| Threat | Mitigation |
| --- | --- |
| Malicious install scripts or tests in a new dependency version | Verify runs in a sandbox (bubblewrap on Linux, `sandbox-exec` or a container on macOS, container on Windows) with no access to home, SSH keys, or the keyring; lifecycle scripts disabled; network allowed only for the initial fetch, then dropped. If no sandbox is available, verify is refused (`SANDBOX_UNAVAILABLE`) unless explicitly overridden. |
| Freshly published malicious versions | Cooldown windows, plus a check that the version's publish time and maintainers did not just change (where the registry exposes it). |
| Prompt injection via release notes, changelogs, advisory text, commit messages | Treat as untrusted: strip HTML and links, truncate, wrap in clearly delimited data blocks, and never let tool results trigger writes; writes need user confirmation regardless of model output. |
| A model or script calling destructive tools by mistake | MCP annotations, elicitation-based confirmation, `confirm` argument fallback, per-repo allow list of writable paths, and a global `read_only: true` switch. |
| Other local users or browser pages reaching the daemon | Loopback only, bearer token, `Origin` header validation, no CORS, Unix-socket option on Linux and macOS. |
| Credential leakage | Secrets only in the OS keyring; redacted from logs, errors, and resources; the sandbox never receives them. |
| Path traversal or symlink escapes | All paths resolved and checked against registered roots; worktrees created only under the state directory. |
| Tampered artifacts | Respect `go.sum`, `GONOSUMDB`/`GOFLAGS=-mod=readonly`, npm integrity hashes, and lockfile checksums; never disable verification. |
| Supply-chain attack on the tool itself | Reproducible builds, signed release artifacts and checksums, minimal dependency set, `govulncheck` on its own CI. |

**Always-on rules**

- No telemetry, no network calls except to configured registries, OSV, and forges.
- An append-only audit log (JSONL, one line per tool call and per write) records who called what, with arguments, results, and git SHAs.
- Every write tool reports exactly what it will change before it changes it.

## 12. Observability, testing, packaging, milestones

**Observability**

- Structured JSON logs through `slog` to stderr (and a rotating file in daemon mode), with `run_id`, `repo_id`, and `tool` fields on every line.
- `depmcp status` and a `depmcp://status` resource show the scheduler queue, last run per repo, cache hit rate, and rate-limit headroom.
- Optional Prometheus-format metrics on a separate loopback port, off by default.

**Testing**

- **Unit:** table-driven tests for semver classification, policy order, grouping, and config import; fuzz tests for every manifest and lockfile parser.
- **Ecosystem golden tests:** fixture repositories per ecosystem; the expected diff after `Apply` is checked in.
- **Fakes:** `httptest` servers for registries, OSV, and forges; a fake `Sandbox` so verify logic is testable without bubblewrap.
- **MCP protocol tests:** in-memory transport pairs exercising every tool, resource, prompt, cancellation, and error code; schema snapshot tests so tool contracts do not drift.
- **Security tests:** prompt-injection corpus in changelog fixtures, path-traversal attempts, and token-redaction checks.
- **CI:** `go test -race ./...`, `golangci-lint`, `govulncheck`, and a nightly end-to-end run against a throwaway git repo with a local registry mirror.

**Packaging**

- Static builds for linux/amd64, linux/arm64, darwin/arm64, and windows/amd64 via GoReleaser, with checksums and cosign signatures.
- Install paths: `go install`, release tarball, and an RPM for Fedora.
- `depmcp mcp-config` prints ready-to-paste client configuration for Claude Desktop, Claude Code, and generic MCP clients (stdio command or HTTP URL plus token).

**Milestones**

1. **M0, skeleton:** MCP server (stdio), config loader, repo registry, SQLite store, audit log. Exit: `list_repositories` and `add_repository` work from a real client.
2. **M1, Go modules read path:** inventory, registry lookup, OSV, `check_updates`, `check_vulnerabilities`. Exit: accurate output on three real repositories.
3. **M2, write path:** planner, worktree applier, `apply_plan`, `revert_run`, confirmation flow. Exit: a Go update branch created and reverted cleanly.
4. **M3, verify and sandbox:** bubblewrap runner, `verify_run`, `govulncheck`. Exit: failing update correctly blocked.
5. **M4, npm, Docker, GitHub Actions:** three more ecosystems behind the same interface.
6. **M5, daemon and scheduler:** HTTP transport, token auth, schedules, notifications, service units.
7. **M6, forges and polish:** GitHub, GitLab, Gitea PRs; `dependabot.yml` import; prompts; documentation.
8. **M7, v2 ecosystems:** pip/uv/poetry and Cargo.
