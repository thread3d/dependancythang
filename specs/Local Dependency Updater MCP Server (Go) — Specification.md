# Local Dependency Updater MCP Server (Go) — Specification

Nov 7, 2026 · @Julian Roland Parramore · **Rev 2**

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
- **No long-lived daemon, no listening socket, no HTTP transport in v1.** See §2.

**Design principles**

- The user's working tree is never touched: all writes happen in git worktrees.
- Read tools are free; every write tool is annotated, auditable, and gated.
- Everything that comes from the network (release notes, changelogs, advisories) is untrusted data.
- Human-committable facts live in files a human can read, diff, and review; a database holds only what can be rebuilt.
- One static binary, pure-Go dependencies, no CGO, runs on Linux (v1) and macOS (best effort).

## 2. Decisions settled in Rev 2

These were open questions in Rev 1. They are decided; the rest of the document assumes them.

| # | Question | Rev 1 default | **Rev 2 decision** | Consequence |
| --- | --- | --- | --- | --- |
| 1 | **Trigger model** — MCP servers are passive, Dependabot is proactive | Daemon owning scheduler + stdio server + HTTP forwarding | **No daemon in v1.** MCP stays pure stdio. Scheduling is an external timer (systemd user timer, launchd, or cron) invoking a one-shot `depmcp run` subcommand. State sharing is a per-repo lock file, not a request-forwarding protocol. | Removes Streamable HTTP, bearer tokens, `Origin` validation, CORS, the stdio→daemon relay, and in-process scheduler goroutines. A hypothetical daemon returns in v3 only if a real need appears. |
| 2 | **Code execution vs private registries** — Rev 1's §2 #7 and §11 contradicted each other | "Honor registry config; sandbox never receives credentials" | **Two-phase execution.** Phase A (resolve/apply) runs as the user, with credentials and network, with package-manager lifecycle scripts disabled. Phase B (verify) runs sandboxed, with credentials stripped, network dropped after fetch, and caches pre-populated by Phase A. | The credential boundary is now a phase boundary with a stated residual risk, not an unresolved contradiction. See §4.1 and §12. |
| 3 | **Config file vs SQLite** — two authorities for the same facts | Both existed; neither was declared authoritative | **The YAML config is the single source of truth** for repositories and ignore rules. SQLite holds derived, rebuildable state only (inventory snapshots, plans, runs, advisories, HTTP cache, audit index). Mutating tools edit the YAML atomically and re-read it. | `add_repository`/`ignore_update` become config editors. Deleting the state directory loses history, never intent. See §5. |

## 3. Assumptions you can overrule

Decisions this document makes on your behalf, with no further input. Each row names the default so you can change it in one place.

| # | Assumption | Why it matters | Default |
| --- | --- | --- | --- |
| 1 | **Supported ecosystems** | Dependabot covers 30+. Each needs its own parser, resolver, and lockfile writer. | v1: Go modules, npm, pnpm, Docker, GitHub Actions. v1.1: yarn v1 and yarn berry (separate implementations). v2: pip/uv/poetry, Cargo. |
| 2 | **Prompt injection** | Release notes and changelogs flow into the LLM's context and can carry instructions. | Sanitize before use, mark as untrusted, truncate, strip markup and links, never let tool results trigger writes. |
| 3 | **Cooldown / minimum release age** | Most malicious package versions are caught within days; fresh releases are the riskiest. | patch 1 day, minor 3, major 7; overridable per package, bypassed for critical/high advisories. |
| 4 | **Forge** | PRs need a token; tokens need safe storage. | `local` (branch + markdown report) is the default. Remote forges are opt-in per repository. |
| 5 | **Monorepos and workspaces** | Multiple manifests per repo, `go.work`, npm workspaces, shared lockfiles. | Directory-scoped update targets, as in `dependabot.yml`. |
| 6 | **Transitive and indirect deps** | Many advisories live in dependencies you never import directly. | Report all; update direct deps by default, force-bump indirect ones only for advisories. |
| 7 | **Reachability** | A vulnerability in code you never call is lower priority. | `govulncheck` for Go; "reachability unknown" elsewhere. |
| 8 | **Release-note summaries** | Humans want "what breaks?", not a diff. | MCP sampling, with the raw sanitized text always attached. |
| 9 | **Human-in-the-loop** | Write tools must not run on a model's whim. | Elicitation-based confirmation on every write; `confirm: true` fallback. Confirmation text is computed from plan data, never from model or sampled output. |
| 10 | **Undo** | A bad update needs a clean way back. | Every applied plan is a branch; `revert_run` deletes it and its worktree, except where the run is already terminal-merged (§12). |
| 11 | **Notifications** | Unattended scheduled runs produce results nobody sees. | Desktop notification and an optional ntfy/webhook target, emitted by the one-shot run. |
| 12 | **License and health checks** | Updates can change a package's license or pull in abandoned or deprecated code. | Best-effort: report license changes, deprecation flags, and yanked versions in the plan. License data quality is poor; never gate on it. |
| 13 | **Rate limits and offline use** | Registry and advisory APIs throttle; laptops go offline. | HTTP cache with ETags, backoff, per-registry limiter, and `offline: true` serving cached data with a warning. |
| 14 | **Config portability** | You likely already have `dependabot.yml` files. | Import read-only, map to native config, print unmappable keys. |
| 15 | **Platforms (v1)** | A scheduler needs to start at login; a sandbox needs an OS primitive. | Linux is the target (bubblewrap, systemd timers). macOS is best-effort (`sandbox-exec` is long-deprecated; container runtime required for verify) with a launchd template. Windows is out of v1. |
| 16 | **Audit log** | You need to know what ran, when, and why. | Append-only JSONL log of every tool call and write; SQLite `audit` table is a queryable mirror, the JSONL is the record. |
| 17 | **MCP SDK** | The server's foundation. | The official Go SDK (`github.com/modelcontextprotocol/go-sdk`). Its capability surface is a milestone-zero risk, not an assumption (§14). |

## 4. Architecture

One binary, one long-lived mode (MCP over stdio, started by the client) and a set of one-shot CLI subcommands that share the same core library.

**Process model**

- **`depmcp serve --stdio`:** started by the MCP client, holds a session, exits with the client. stdout carries only protocol frames; all logs go to stderr. No scheduler, no listener.
- **One-shot subcommands:** `depmcp scan`, `plan`, `run`, `apply`, `verify`, `doctor`, `status`, `import-dependabot`, `schedule install|print`. Each opens the state DB, does its work, exits. This is what timers invoke.
- **No IPC.** Two processes never talk to each other; they coordinate through the state DB and a lock file.
- **Single-writer rule:** `flock` on `<state_dir>/locks/<repo-id>.lock` for the duration of any write operation. Contention returns `REPO_BUSY` naming the holder. The OS releases the lock on process death, so there are no stale locks. (Windows, when it arrives, uses `O_EXCL` plus a pid/staleness timeout.)
- **Crash reconciliation:** there is no daemon to notice a crash, so any command that opens the state DB first reconciles runs left in `applying` or `verifying` by a process that no longer holds the repo lock, marking them `aborted` (§12).

**Go package layout**

```text
cmd/depmcp/            main, CLI (serve, scan, plan, run, apply, verify,
                       revert, doctor, status, import-dependabot, schedule)
internal/mcpserver/    tool, resource, prompt registration; schemas from Go structs
internal/config/       YAML load/validate/atomic-write, dependabot.yml import
internal/repo/         repository discovery, git fetch/worktree management, locking
internal/ecosystem/    Ecosystem + Versioner interfaces; gomod/, npm/, pnpm/,
                       docker/, actions/
internal/registry/     package registry clients (proxy.golang.org, npm, Docker, ...) + HTTP cache
internal/advisory/     OSV client, govulncheck runner, severity normalization
internal/policy/       semver rules, cooldown, grouping, ignore rules
internal/planner/      builds an UpdatePlan from inventory + advisories + policy
internal/applier/      Phase A: writes manifests/lockfiles in a worktree, commits
internal/verify/       Phase B: sandboxed build/test runner
internal/forge/        Forge interface + github/, gitlab/, gitea/, local/
internal/store/        SQLite (modernc.org/sqlite, no CGO), migrations
internal/schedule/     timer unit generation (systemd, launchd, crontab)
internal/secrets/      OS keyring wrapper
internal/audit/        append-only JSONL audit log
```

**Key dependencies**

| Need | Choice | Note |
| --- | --- | --- |
| MCP | official Go SDK | stdio transport, sampling, elicitation. Confirm capabilities in M0 (§14). |
| Go modules | `golang.org/x/mod` | `modfile` and `semver` parse and edit `go.mod` safely. |
| npm semver | a dedicated npm-semver port | `Masterminds/semver/v3` is **not** npm-compatible (`||`, hyphen ranges, `x`, prerelease rules differ). Use Masterminds only where its grammar genuinely matches (Cargo, later). |
| Git | shell out to `git` | go-git's worktree support is incomplete; `git worktree` is the right tool. |
| Storage | `modernc.org/sqlite` | Pure Go, keeps the build CGO-free. |
| Config | `gopkg.in/yaml.v3` | `KnownFields(true)` for strict decoding; `yaml.Node` editing to preserve comments. |
| Secrets | `zalando/go-keyring` | Secret Service / Keychain. Has no headless fallback: `doctor` must detect a missing secret service and say so. |
| Logging | `log/slog` | JSON to stderr with a request ID per tool call. Requires Go ≥ 1.21. |

Each ecosystem, forge, and advisory source sits behind a small interface so tests can substitute fakes.

### 4.1. Two-phase execution (the credential boundary)

Verifying an update means running other people's code. Fetching a private dependency means presenting your credentials. Those two requirements must never occupy the same process.

| | **Phase A — resolve & apply** | **Phase B — verify** |
| --- | --- | --- |
| Purpose | Resolve versions, rewrite manifests and lockfiles, commit | Build and test the result |
| Runs as | The user, in the worktree | An unprivileged sandboxed process |
| Credentials | Available (inherited env, `.npmrc`, `netrc`, `GOPRIVATE`, credential helpers) | **Stripped**: no home, no keyring, no netrc, no tokens in env |
| Network | Allowed (registries only) | Populated from Phase A, then dropped |
| Lifecycle scripts | **Disabled** (`--ignore-scripts`, `GOFLAGS=-mod=readonly`) | Disabled by default; enabled only under `verify.allow_install_scripts` |
| Package caches | Populated: `<state_dir>/caches/{gomod,npm,pnpm}` | Mounted **read-only** from the Phase A snapshot |
| Untrusted code execution | None by design — Phase A only runs the package manager itself | Yes, bounded by the sandbox (§12) |

Honest residual risks, to be documented for users:

- `--ignore-scripts` is not universal. Packages that require a native build step will fail in Phase A; the error is `SANDBOX_INSTALL_SCRIPTS_REQUIRED`, and enabling scripts moves that risk into Phase B whether or not the sandbox holds.
- A package manager is itself a large piece of parsing code acting on untrusted registry metadata. Phase A is not a sandbox; it is a *no-arbitrary-code* phase.
- `go build` does not execute dependency code, but `go test` does, and tests are Phase B.

## 5. Configuration authority

Repositories and ignore rules are **declared in YAML** and **derived into state**, never the reverse.

- **Authoritative:** the global `$XDG_CONFIG_HOME/depmcp/config.yaml` plus each repository's committed `.depmcp.yml`. These are the intent: reviewable, diffable, and shareable.
- **Derived:** everything in SQLite. `repo_state` caches a `config_hash` and `last_scan`; `ignores` does not exist as a table. Wiping the state directory loses plans, runs, snapshots, and caches — never which repos you track or what you've ignored.
- **Mutation protocol.** `add_repository`, `update_repository`, `remove_repository`, `ignore_update`, and `unignore_update` all: validate → edit the YAML `yaml.Node` tree (preserving comments and key order for untouched nodes) → write a temp file in the same directory → `fsync` → `rename` → re-read and re-validate. A concurrent hand-edit is detected by comparing the file hash before and after; on mismatch the tool retries once, then returns `CONFIG_CHANGED_EXTERNALLY`.
- **Read-only config.** A committed `.depmcp.yml` in a read-only checkout, or a global config on a read-only filesystem, is a normal condition. Mutating tools return `CONFIG_READONLY` with the exact YAML snippet to paste, rather than silently writing to the wrong layer.
- **Layer resolution.** Tool arguments > `.depmcp.yml` > global `config.yaml` > built-in defaults. `get_config` returns the merged result *and* the source layer for each resolved key, so "why is cooldown 5?" is answerable.
- **Reconciliation.** On every open, `repo_state` rows whose `config_hash` no longer matches are refreshed from config; rows with no config entry are marked `orphaned` (their branches and runs are listed but nothing new is planned for them).

## 6. Ecosystem support

Every package manager is one implementation of a single interface, so adding Cargo or Maven later touches no core code.

Version *identity* is not uniformly semantic (Docker tags and Action refs are not semver), so versions are opaque and each ecosystem owns its ordering:

```go
// Version identity in an ecosystem's own terms.
type VersionKind int // Semantic | Tag | Ref | Digest

type Version struct {
    Raw    string // "1.2.3", "1.2-alpine", "v4", "sha256:9f8e..."
    Kind   VersionKind
    Digest string    // secondary identity for Docker/Actions, "" otherwise
    PublishedAt time.Time // zero if unknown
    Yanked     bool
}

// UpdateClass is patch | minor | major | digest | pin | unknown.
// "unknown" means the ecosystem cannot classify this pair and policy must ask.
type UpdateClass int

type Ecosystem interface {
    Name() string                                              // "gomod", "npm", "docker"
    Detect(ctx context.Context, root string) ([]Target, error)
    Inventory(ctx context.Context, t Target) ([]Dependency, error)

    // Candidates queries the registry. Each returns Version + PublishedAt + Yanked
    // so policy never has to re-derive freshness from a string.
    Candidates(ctx context.Context, d Dependency) ([]Version, error)

    // Apply rewrites manifest and lockfile inside the given worktree.
    // It is the package manager's job, not ours, to compute the final dependency set.
    Apply(ctx context.Context, wt Worktree, t Target, ch []Change) (ApplyResult, error)

    VerifyCommands(t Target) []Command

    // Compare is a total order over this ecosystem's own version kind.
    Compare(a, b Version) int
    // Classify maps a version pair to an update class under this ecosystem's rules.
    Classify(from, to Version) UpdateClass
}

// TagSource is implemented by ecosystems whose versions come from a plain tag
// list rather than a resolver: Docker image tags, GitHub Action refs.
type TagSource interface {
    Tags(ctx context.Context, ref string) ([]Version, error)
}
```

Docker and Actions implement `TagSource` with a heuristic tag order (`1.2-alpine`, `3.14-slim`, `-rc` suffixes, four-digit years) and `Classify` returning `pin` or `digest` rather than pretending tags are semver. Action refs that are branch names (`@main`) or local paths (`uses: ./`) are never proposed for update; they are reported as `unpinnable` in the plan.

**Rollout**

| Phase | Ecosystem | How updates are applied | Notes |
| --- | --- | --- | --- |
| v1 | Go modules | `go get pkg@version`, then `go mod tidy` | Honors `go.work`, `vendor/` (re-runs `go mod vendor`), and `GOPRIVATE`. `go.work` + `go mod tidy` interact: tidy is run per module, and the workspace is re-validated. |
| v1 | npm | `npm install pkg@version --ignore-scripts`, then lockfile is truth | Lockfile format v1/v2/v3 all read; writes go through npm. |
| v1 | pnpm | `pnpm update pkg@version --ignore-scripts` | pnpm-lock.yaml v6/v9. Workspace-aware. |
| v1.1 | yarn v1 | `yarn upgrade pkg@version --ignore-scripts` | Different lockfile; separate implementation. |
| v1.1 | yarn berry | `yarn up`, honors PnP | Different install model entirely; its own package. |
| v1 | Docker | Rewrite `FROM` tags and digests in Dockerfiles and Compose files | Digest + tag together; `TagSource` supplies candidates. |
| v1 | GitHub Actions | Bump `uses:` refs, preferring commit SHA pins with a version comment | Skips `@main`, `uses: ./`, and local actions. |
| v2 | pip, uv, poetry | Update `pyproject.toml` or requirements files, then re-lock | |
| v2 | Cargo | `cargo update -p` plus `Cargo.toml` requirement edits | |
| later | Maven/Gradle, Nix flakes, Terraform, Homebrew | behind the same interface | |

**Rules shared by all ecosystems**

- Prefer the ecosystem's own tool to edit lockfiles; hand-editing them is the main source of subtle breakage.
- Pre-release versions are skipped unless the current version is itself a pre-release.
- Yanked, retracted, and deprecated versions are never proposed; the plan reports them instead.
- Registry responses are cached on disk with ETags and a per-registry rate limiter.
- **The lockfile is the truth, not the request.** After `Apply`, the observed dependency delta is read back from the toolchain and recorded per package. This is what makes §8's commit granularity honest.

**Lockfile format matrix (v1)** — each row is a distinct parser and a distinct set of edge cases, and must have its own fixture:

| Manager | File(s) | Formats | Trap |
| --- | --- | --- | --- |
| Go | `go.mod`, `go.sum`, `vendor/modules.txt`, `go.work` | single | `go mod tidy` may bump indirect modules unrelated to the request |
| npm | `package-lock.json` | v1, v2, v3 | v2 is v1+packages; a mixed-repo can regress format |
| pnpm | `pnpm-lock.yaml` | v6, v9 | workspace root lockfile is shared across packages |
| yarn v1 | `yarn.lock` | v1 | no lockfile-level integrity in older repos |
| yarn berry | `.yarn/cache`, `yarn.lock`, `.pnp.cjs` | berry | PnP has no `node_modules`; many test runners need adjustments |

## 7. MCP surface

The server exposes **22 tools**, **10 resources**, and **4 prompts**. Tools take and return typed JSON (schemas generated from Go structs) and carry MCP annotations so clients can gate the dangerous ones.

**Output discipline (applies to every tool)**

Every list-returning tool accepts `limit` (default 100, max 1000) and an opaque `cursor`, and returns an envelope:

```json
{ "items": [ ... ], "total": 1841, "next_cursor": "b2Zmc2V0OjEwMA==", "truncated": true }
```

Free-text fields (changelog excerpts, logs, notes) are truncated at `max_bytes` (default 8 KiB per field, 64 KiB per tool result) with an explicit `…truncated, read depmcp://runs/{id}/log or call get_run for the rest` marker. An MCP server that returns a whole inventory is an MCP server that breaks the client's context window; this is a hard requirement, not a nicety.

**Tools**

| Tool | Purpose | Annotations (readOnly / destructive / idempotent / openWorld) |
| --- | --- | --- |
| `list_repositories` | Registered repos with last scan time and open branches. | ✓ / ✗ / ✓ / ✗ |
| `add_repository` | Register a path in YAML; auto-detect ecosystems, import `dependabot.yml`. | ✗ / ✗ / ✓ / ✗ |
| `update_repository` | Change a repo's forge, schedule, or target directories in YAML. | ✗ / ✗ / ✓ / ✗ |
| `remove_repository` | Unregister a repo (does not delete files or branches). | ✗ / ✓ / ✓ / ✗ |
| `scan_dependencies` | Inventory direct and indirect dependencies per target. | ✓ / ✗ / ✓ / ✗ |
| `check_updates` | Available updates with update class, age, cooldown status, advisories. | ✓ / ✗ / ✓ / ✓ |
| `check_vulnerabilities` | Advisories affecting current versions, with severity, fixed versions, reachability. Always force-refreshes OSV (§9). | ✓ / ✗ / ✓ / ✓ |
| `plan_updates` | Build a grouped `UpdatePlan` under current policy. Stores a plan record; mutates no repository. Returns `plan_id`. | ✓ / ✗ / ✗ / ✓ |
| `get_plan` | Retrieve a stored plan by id, including deferred groups and their reasons. | ✓ / ✗ / ✓ / ✗ |
| `get_release_notes` | Fetch and sanitize changelog text between two versions. | ✓ / ✗ / ✓ / ✓ |
| `apply_plan` | Create worktree and branch, apply, commit. Re-validates the plan first (§8); Phase A fetches from registries. Needs confirmation. | ✗ / ✗ / ✗ / ✓ |
| `verify_run` | Run build and tests for a run in the sandbox. Executes untrusted code. | ✗ / ✓ / ✗ / ✗ |
| `open_pull_request` | Push a verified branch and open a PR through the configured forge. | ✗ / ✗ / ✗ / ✓ |
| `revert_run` | Delete a run's branch and worktree (subject to the §12 terminal-state rules). | ✗ / ✓ / ✓ / ✗ |
| `ignore_update` | Add an ignore rule to YAML (package, version range, mandatory reason, optional expiry). | ✗ / ✗ / ✓ / ✗ |
| `unignore_update` | Remove an ignore rule from YAML. | ✗ / ✗ / ✓ / ✗ |
| `get_config` | Effective merged config, with the source layer per key; secrets redacted. | ✓ / ✗ / ✓ / ✗ |
| `validate_config` | Validate a candidate config without saving; report line numbers. | ✓ / ✗ / ✓ / ✗ |
| `list_runs` | Run history for one repo, newest first, with status and branch. | ✓ / ✗ / ✓ / ✗ |
| `get_run` | A single run record with plan, status, verify result, and log reference. | ✓ / ✗ / ✓ / ✗ |
| `run_now` | Execute the configured job for one repo immediately: scan → plan → apply → verify, stopping short of PRs unless `auto_pr`. Needs confirmation. | ✗ / ✓ / ✗ / ✓ |
| `status` | Scheduler config, last run per repo, cache hit rate, lock holders, rate-limit headroom. | ✓ / ✗ / ✓ / ✗ |

*Annotation note:* `apply_plan` is deliberately **not** marked `destructive` — it creates a branch and worktree and touches nothing existing. `verify_run` **is**, because it executes third-party code. No MCP hint expresses "runs untrusted code," so annotations cannot carry the safety burden; elicitation is the control, and it must be mandatory regardless of what the model outputs.

**Resources**

- `depmcp://repos`: tracked repositories.
- `depmcp://repos/{id}`: one repository's state, forge, and schedule.
- `depmcp://repos/{id}/dependencies`: current inventory.
- `depmcp://repos/{id}/updates`: latest `check_updates` result.
- `depmcp://repos/{id}/advisories`: latest advisory triage state.
- `depmcp://plans/{plan_id}`: a stored plan.
- `depmcp://runs/{id}`: run record.
- `depmcp://runs/{id}/log`: captured output.
- `depmcp://config`: effective merged configuration, secrets redacted.
- `depmcp://status`: same content as the `status` tool.

**Resource updates.** With no daemon there is no process that can push a notification the moment a timer run finishes. During an active session the stdio server may poll the state DB and emit `notifications/resources/updated` for `repos/{id}` and `runs/{id}` — this is a best-effort convenience, never a guarantee. Anything that must be seen is a tool call.

**Prompts**

- `triage_vulnerabilities`: rank advisories by severity, reachability, and fix availability, and propose an order.
- `review_updates`: walk a plan group by group, summarize breaking changes, recommend apply or skip.
- `explain_breaking_change`: given a package and version range, read the sanitized notes and list required code changes.
- `weekly_digest`: concise summary of what changed across all repos since the last digest.

**Protocol behaviors**

- **Progress and logging:** long tools send `notifications/progress` and structured log messages to stderr. The token comes from `req.Params.GetProgressToken()`; when it is nil (caller opted out) no notifications are emitted. Verified in the M-1 spike.
- **Cancellation:** every tool honors `context` cancellation, and a cancelled run leaves a worktree and run row marked `aborted`, never half-committed. The SDK delivers cancellation as a **tool result with `isError: true` and the bare text `context canceled`** — not a JSON-RPC error, not code `-32800`, and with no structured output. So: aborted state must be persisted by the handler before it returns, and the stable code `REQUEST_CANCELLED` is mapped from `ctx.Err()` by us rather than taken from the SDK.
- **Elicitation:** write tools ask the user to confirm with a short diff-style summary **computed from plan data**. The SDK refuses `Elicit` outright — error `client does not support elicitation` — unless the client advertised the capability at `initialize`. Treat the `confirm: true` argument fallback as the *default* path for any client that does not advertise elicitation, not as a legacy edge case, and keep the fallback payload identical to the interactive one.
- **Sampling:** `get_release_notes` may ask the client's model for a breaking-change summary. Optional; the raw sanitized text is always returned too, and sampling output is never fed back into a confirmation prompt or a write.
- **Errors:** failures return `isError: true` with a stable code and a human-readable hint. The set is closed and documented: `NOT_A_GIT_REPO`, `NO_DEFAULT_BRANCH`, `FETCH_FAILED`, `CONFIG_INVALID`, `CONFIG_READONLY`, `CONFIG_CHANGED_EXTERNALLY`, `REPO_BUSY`, `PLAN_STALE`, `VERSION_YANKED`, `COOLDOWN_ACTIVE`, `CAP_REACHED`, `SANDBOX_UNAVAILABLE`, `SANDBOX_INSTALL_SCRIPTS_REQUIRED`, `VERIFY_FAILED`, `VERIFY_TIMEOUT`, `SIGNING_FAILED`, `AUTH_REQUIRED`, `REGISTRY_UNAVAILABLE`, `OFFLINE_CACHE_MISS`, `FORGE_NOT_CONFIGURED`, `CONFIRMATION_REQUIRED`, `REQUEST_CANCELLED`, `PATH_OUTSIDE_ROOT`.

## 8. Update pipeline

Every update flows through the same seven stages, and each stage's output is stored so a run can be inspected or resumed.

1. **Discover.** Walk registered roots, detect manifest targets per ecosystem, and respect `.gitignore` and a configurable exclude list.
2. **Inventory.** Parse manifests and lockfiles into `Dependency{name, current, constraint, direct, target}`.
3. **Resolve candidates.** Query registries; drop pre-releases, yanked, and retracted versions; classify each candidate as patch, minor, major, digest, or unknown under the ecosystem's own rules.
4. **Enrich.** Attach advisories (OSV), reachability (`govulncheck` for Go), license changes, deprecation flags, and release age. **The resolved candidate list is stored inside the plan**, so a plan applied three days later still means what it meant when it was built.
5. **Plan.** Apply policy in this order: ignore rules, pinning, cooldown, allowed update types, then grouping. Advisory fixes bypass cooldown and update-type limits unless config says otherwise. The result is an immutable `UpdatePlan` stored under a `plan_id`, including groups marked `deferred` with a reason code (`cap_reached`, `would_be_yanked`, `needs_major`).
6. **Apply.** Fetch, create `git worktree add` on branch `depmcp/<ecosystem>/<group>-<date>` from the **fetched default branch**, run the ecosystem's `Apply`, and commit.
7. **Verify.** Run the sandboxed build and tests. Record pass or fail, output, and duration.

**Base ref and freshness**

- Before branching: `git fetch <remote> <default-branch>` (default `origin`/`HEAD`), then branch from `<remote>/<default>`.
- Branching from a stale local `main` produces PRs full of unrelated diffs; this is the single most common way a local updater embarrasses its user.
- `fetch.enabled: false` (or `--offline`) uses the local default branch and records `based_on_stale_ref: true` in the plan and the PR body.
- Fetch failure is `FETCH_FAILED`, not a silent local fallback.

**TOCTOU re-validation at apply**

A stored plan is a hypothesis about the registry as of `created_at`. `apply_plan` re-validates before writing anything, and fails with `PLAN_STALE` (listing the offending packages) if:

- a target version no longer exists or is now yanked/retracted (`VERSION_YANKED`);
- the version's publish time no longer satisfies the cooldown (`COOLDOWN_ACTIVE`);
- the repository's default branch moved in a way that makes the plan conflict;
- the config hash changed (policy edits, new ignore rules).

Stale plans are not auto-refreshed; the caller gets a fresh `plan_id` to review. Silently retargeting a reviewed plan is worse than failing.

**Commit granularity vs package granularity**

`go get` + `go mod tidy` bump indirect modules; `npm install pkg@v` rewrites lockfile entries well beyond that one package. "One commit per package" is therefore an aspiration the toolchain decides, not a promise the spec can make:

- The applier reads the **observed delta** back out of the toolchain after each package change.
- If the delta touches only that package's subtree, it commits that package alone.
- If the delta is broader, it records `unsplittable: true` on the group and commits the group once, with the full change list in the message. The plan says so up front, so nobody is surprised by the diff.

**Grouping and limits**

- Groups match by name pattern, dependency type (dev or prod), and update class, as in Dependabot's `groups:`.
- Caps are **per target, not per repository** (a four-ecosystem monorepo is not five branches' worth of work): `max_open_branches` default 5 per target-directory, with a separate cap for security updates. When a cap is hit, the group is marked `deferred: cap_reached` at **plan** time — a plan is never valid yet unappliable.
- A rebase step re-applies a plan on a fresh default branch when the base moves, and supersedes older branches for the same package.

**Failure handling and bisect**

- A failing group is bisected. If the group has split commits, bisect by `git revert` of individual commits inside the existing branch — cheap, no re-install.
- If the group is `unsplittable`, bisect by re-applying each member alone, bounded by `bisect_max_attempts` (default 8). A group larger than the bound is reported as `bisect_incomplete` rather than run forever.
- Dirty main checkouts are fine (worktrees are separate). A missing, detached, or unsettable default branch aborts with `NO_DEFAULT_BRANCH`.

## 9. Security advisories

Advisories come from [OSV.dev](https://osv.dev), which aggregates the GitHub Advisory Database, the Go vulnerability database, and ecosystem-specific feeds, so one client covers every ecosystem.

- **Lookup:** batch `POST /v1/querybatch` per inventory, keyed by ecosystem, package, and version. Responses are cached under a synthesized key (the endpoint is POST, so there is no ETag to rely on); TTL 6 hours for scheduled scans.
- **Forced refresh:** `check_vulnerabilities` called by a user bypasses the cache and re-queries. A critical advisory published an hour ago being invisible to the person who just asked is the one failure this tool cannot afford. Offline mode serves stale data with an explicit warning.
- **Normalization:** map every advisory to `{id, aliases, severity (CVSS v3/v4 score and label), affected ranges, fixed versions, published, withdrawn}`; deduplicate by alias (GHSA, CVE, GO-).
- **Reachability (Go):** run `govulncheck` in the Phase B sandbox, using a pre-fetched vulnerability DB copy; mark each finding `called`, `imported`, or `unknown`. Other ecosystems report `unknown`.
- **Fix selection:** choose the lowest fixed version that satisfies the constraint and policy; if only a major bump fixes it, flag it for human decision rather than auto-planning it.
- **Security updates vs version updates:** advisory-driven updates are a separate plan category with their own branch cap, labels, and a prominent line in the commit message.
- **Triage state:** each advisory can be `open`, `ignored` (with reason and expiry, stored in YAML per §5), or `fixed`. Ignoring requires a reason and defaults to a 30-day expiry.
- **Withdrawn advisories:** automatically cleared and logged.

Default severity handling:

| Severity | Cooldown | Update-class limit | Notification |
| --- | --- | --- | --- |
| Critical | bypassed | none | immediate |
| High | bypassed | none | immediate |
| Medium | applies | minor and patch | in digest |
| Low | applies | minor and patch | in digest |

## 10. Configuration file

Configuration is layered, later layers winning: built-in defaults, then `$XDG_CONFIG_HOME/depmcp/config.yaml`, then a per-repo `.depmcp.yml` committed in the repository, then tool-call arguments. Decoding is strict; unknown keys are errors, and `validate_config` reports line numbers.

The schema deliberately mirrors `dependabot.yml` keys (`updates`, `directory`, `schedule`, `groups`, `ignore`, `allow`, `cooldown`) so imports are near one-to-one. `repositories` and `ignore` are the authoritative declarations described in §5.

```yaml
version: 1

server:
  state_dir: ~/.local/share/depmcp
  offline: false
  read_only: false              # global kill switch for every write tool

fetch:
  enabled: true                 # fetch before branching; false = based_on_stale_ref
  remote: origin

repositories:
  - path: ~/src/inference-cluster
    forge: local                # local | github | gitlab | gitea
  - path: ~/src/libmdast
    forge: github
    remote: origin

defaults:
  schedule: { interval: weekly, day: monday, time: "06:00", timezone: Europe/Copenhagen }
  cooldown: { patch_days: 1, minor_days: 3, major_days: 7 }
  max_open_branches: 5          # per update target, not per repository
  verify: { enabled: true, timeout: 15m, sandbox: bubblewrap, allow_install_scripts: false, strict_cache: false }
  bisect_max_attempts: 8
  branch_prefix: depmcp
  commit_style: conventional
  auto_pr: false                # one-shot runs stop at a verified branch unless true

updates:
  - ecosystem: gomod
    directory: /
    groups:
      golang-x: { patterns: ["golang.org/x/*"] }
    ignore:
      - dependency: github.com/foo/bar
        versions: [">=3.0.0"]
        reason: "v3 drops our Go version"
        until: "2027-03-01"
  - ecosystem: npm
    directory: /web
    allow: [{ dependency_type: production }]
    cooldown: { minor_days: 5 }

security:
  source: osv
  bypass_cooldown: [critical, high]
  govulncheck: true

notify:
  desktop: true
  ntfy: { topic: depmcp-julian }   # optional
```

**Rules**

- **Schedules:** `daily`, `weekly`, `monthly`, or a cron expression; always evaluated in an explicit time zone. Scheduling itself is external (§12).
- **Ignore:** by package, version range, or update class; `reason` is mandatory, `until` is optional, and expired rules are surfaced in the weekly digest.
- **Pinning:** an exact version constraint in the manifest is respected as a pin unless `allow_pinned: true`.
- **Versioning strategy:** `auto` (default), `increase`, `widen`, or `lockfile-only`, mapped per ecosystem.
- **Secrets:** referenced by keyring key (`secret:github-token`), never stored in YAML, never written to the state DB.
- **Import:** `depmcp import-dependabot <repo>` converts `.github/dependabot.yml` and prints keys it could not map.

## 11. Git and forge integration

All writes happen in a dedicated worktree on a dedicated branch, so your checked-out branch, index, and uncommitted work are never touched. The forge layer is optional: the default `local` forge stops at a verified branch.

**Git behavior**

- Worktrees live under `<state_dir>/worktrees/<repo-id>/<run-id>` and are removed on `revert_run` or after merge detection.
- Branch name: `depmcp/<ecosystem>/<group-or-package>-<YYYYMMDD>`; one branch per group, one commit per package where the toolchain allows it (§8).
- Branches are based on the freshly fetched default branch (§8).
- Commit identity comes from the repo's git config, with a `Co-authored-by: depmcp` trailer.
- **Hooks are honored** — depmcp never passes `--no-verify`, so a pre-commit failure fails the run visibly.
- **Signing is checked, not assumed.** If `commit.gpgsign` is on, a non-interactive run cannot answer a pinentry prompt and will hang. `doctor` verifies signing works in a non-interactive context; a run that would need signing and cannot get it fails fast with `SIGNING_FAILED` instead of blocking.
- Merged or closed upstream branches are detected on each scan and cleaned up. With the `local` forge, cleanup happens on the next `run`/`scan` after the branch is gone from the remote-less default branch.

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
- Body: a table of changes, advisory IDs with severity, sanitized release-note excerpts, verification result with log link, whether the branch was based on a stale ref, and the exact command to revert.
- A footer marks the PR as generated by depmcp, so later runs can recognize and update it. Closed-without-merge PRs create a proposed ignore rule (written to YAML only on confirmation).

## 12. State, locking, and scheduling

All durable state lives in one SQLite file plus a few directories under `$XDG_DATA_HOME/depmcp` (`~/.local/share/depmcp` on Linux), so backup is one copy and reset is one delete. Per §5, **none of it is authoritative** — deleting it loses history and caches, not intent.

**State (all derived)**

| Table | Holds |
| --- | --- |
| `repo_state` | Cached view of config: `config_hash`, last scan, orphan flag. Rebuilt from YAML at any time. |
| `dependencies` | Latest inventory snapshot per target. |
| `advisories` | Normalized advisories with triage state; ignore rules themselves live in YAML. |
| `plans` | Immutable `UpdatePlan` JSON, keyed by `plan_id`. |
| `runs` | One row per apply, with branch, worktree path, status, verify result, PR reference. |
| `http_cache` | Registry and OSV responses with key and expiry. |
| `audit` | Queryable mirror of the JSONL audit log; the JSONL file is the record. |

Schema migrations are embedded and forward-only; the binary refuses to start against a newer schema than it knows.

**Locking**

- One `flock` per repository for the duration of any write (`plan` and read-only tools take no lock).
- Contention returns `REPO_BUSY` with the holding pid and operation.
- Because timers and an interactive session can overlap, this lock is the entire concurrency design — there is no queue, no forwarding, and no daemon.

**Run lifecycle**

Statuses: `planned`, `applying`, `applied`, `verifying`, `verified`, `pr_opened`, `merged`, `closed`, `failed`, `aborted`, `reverted`.

| From | Allowed to | Notes |
| --- | --- | --- |
| `planned` | `applying`, `abandoned` | |
| `applying` | `applied`, `failed`, `aborted` | Crash here is reconciled to `aborted` on next open. |
| `applied` | `verifying`, `reverted`, `failed` | |
| `verifying` | `verified`, `failed`, `aborted` | Timeout is `VERIFY_TIMEOUT`, not `failed`. |
| `verified` | `pr_opened`, `reverted` | |
| `pr_opened` | `merged`, `closed`, `reverted` | Revert deletes branch, never the remote PR. |
| `merged`, `closed` | — | **Terminal.** `revert_run` on these cleans the worktree only and says so. |
| `failed`, `aborted` | `reverted` (cleanup), `planned` (retry) | |
| `reverted` | — | Terminal. |

Rev 1 said "any run can end `reverted`", which was wrong: reverting a merged run would try to delete a branch whose content is already on the default branch. The table above is the contract.

**Scheduling without a daemon**

- `depmcp schedule install` writes a **systemd user timer** (`~/.config/systemd/user/depmcp-<repo>.timer`) that runs `depmcp run --repo <id>` at the configured time, with `RandomizedDelaySec=10m` for jitter and **`Persistent=true`** so a missed slot (laptop asleep) fires once at next wake — not once per missed slot. This is the whole of Rev 1's "missed runs" requirement, already solved by systemd.
- `depmcp schedule print` emits the equivalent launchd plist or crontab line for manual installation; cron users are told to prefer `anacron`-style semantics if they care about missed runs.
- `depmcp run` is a one-shot: scan → plan → apply → verify, then notify. It stops before opening PRs unless `auto_pr: true`.
- **Backoff without a daemon:** consecutive failures are counted in `repo_state`. At three, the timer unit's `OnFailure` hook (or the run itself) reports it and `schedule install --disabled` is suggested; `status` shows the count. No paused-in-memory state, because there is no memory.
- Concurrency is bounded by the fact that a timer run and an interactive run both honour the repo lock; the loser gets `REPO_BUSY` and exits non-zero so `journalctl` shows it.

**Operator surface**

- `depmcp doctor` checks: git present and usable, default branch resolvable, fetch permitted, sandbox availability (bubblewrap / container), non-interactive commit signing, keyring availability, registry reachability, config validity, and state DB writability.
- `depmcp status` and the `status` tool show last run per repo, failure counts, cache hit rate, lock holders, and rate-limit headroom.

## 13. Security and safety model

The tool runs other people's code on your machine and feeds other people's text to your LLM, so its threat model matters more than its feature list.

| Threat | Mitigation |
| --- | --- |
| Malicious install scripts or tests in a new dependency version | Lifecycle scripts are disabled in both phases by default. Phase A never runs dependency code. Phase B runs in a sandbox (bubblewrap on Linux; `sandbox-exec` or a container on macOS) with no home, SSH keys, or keyring, and network dropped after fetch. If no sandbox is available, verify is refused (`SANDBOX_UNAVAILABLE`) unless explicitly overridden. |
| Private-registry credentials leaking into untrusted code | Two-phase boundary (§4.1): credentials exist only in Phase A, which runs no dependency code; Phase B gets a read-only cache and no credentials. |
| Freshly published malicious versions | Cooldown windows, plus a check that the version's publish time and maintainers did not just change where the registry exposes it. |
| Prompt injection via release notes, changelogs, advisory text, commit messages | Treat as untrusted: strip HTML and links, truncate, wrap in delimited data blocks. Confirmation payloads and write decisions are computed from plan data, never from model or sampled output. |
| Injection through the confirmation dialog | The elicitation summary is rendered from the stored `UpdatePlan` fields only; it has no free-text field that an LLM or changelog can author. |
| A model or script calling destructive tools by mistake | MCP annotations, mandatory elicitation, `confirm` fallback, per-repo path allow list, and a global `read_only: true` switch. Annotations are advisory; elicitation is the enforcement. |
| Two processes racing on one repository | Per-repo `flock`, no daemon, no IPC, `REPO_BUSY` on contention. There is no network listener to attack. |
| Stale or retargeted plans | Plans carry their resolved candidate list and config hash; `apply_plan` re-validates and fails closed with `PLAN_STALE`. |
| Credential leakage in logs or state | Secrets only in the OS keyring; redacted from logs, errors, resources, and the SQLite file; never passed to the sandbox. |
| Path traversal or symlink escapes | All paths resolved and checked against registered roots; worktrees created only under the state directory. |
| Shared package cache as a supply-chain path | Phase A populates the cache; Phase B mounts it read-only. `verify.strict_cache: true` copies per run instead of sharing. |
| Tampered artifacts | Respect `go.sum`, `GOSUMDB`, `GOFLAGS=-mod=readonly`, npm integrity hashes, and lockfile checksums; never disable verification. |
| Supply-chain attack on the tool itself | Reproducible builds, signed release artifacts and checksums, minimal dependency set, `govulncheck` on its own CI. |

**Always-on rules**

- No telemetry, no network calls except to configured registries, OSV, and forges.
- An append-only audit log (JSONL, one line per tool call and per write) records what was called, with arguments, results, and git SHAs.
- Every write tool reports exactly what it will change before it changes it.

## 14. Observability, testing, packaging, milestones

**Build requirements**

- Official Go toolchain **≥ 1.25** — the hard floor is the MCP SDK (`github.com/modelcontextprotocol/go-sdk` v1.8.0 declares `go 1.25.0`, verified); `log/slog` needs 1.21. Pin `go 1.25` or higher in `go.mod` and verify with `go version` before M0. A distro `gccgo` cannot build this and must not be used: on Fedora it can shadow the real toolchain through `/etc/alternatives/go`, which reports a years-old version while a current `golang-bin` sits unused at `/usr/lib/golang/bin/go`.
- Builds are CGO-free (`CGO_ENABLED=0`), so no C compiler is required at build time and the release binary is statically linked.
- Dev tooling may require a newer Go than the distro ships (GoReleaser v2.20 needs ≥ 1.27.3). Keep `GOTOOLCHAIN=local` for the project build so it stays pinned to the packaged toolchain, and use `GOTOOLCHAIN=auto` only for one-off `go install` of tools.
- Linux: `git`, `bubblewrap`, and the ecosystem CLIs actually used (`go`, `npm`, `pnpm`, `podman`/`docker`).

**Observability**

- Structured JSON logs through `slog` to stderr, with `run_id`, `repo_id`, and `tool` fields on every line. One-shot commands log to stderr and are captured by `journalctl` via the timer.
- `depmcp status` and the `status` tool show last run per repo, failure counts, cache hit rate, lock holders, and rate-limit headroom.
- Optional Prometheus-format metrics written to a file for a node exporter to scrape — **never** a listening port, since that would reintroduce the attack surface §2 removes.

**Testing**

- **Unit:** table-driven tests for version classification (including non-semver tags), policy order, grouping, caps-at-plan-time, and config import/mutation; fuzz tests for every manifest and lockfile parser.
- **Config authority tests:** mutating tools preserve comments and key order; read-only files return `CONFIG_READONLY`; a hand-edit mid-write returns `CONFIG_CHANGED_EXTERNALLY`; wiping the DB leaves repositories and ignores intact.
- **Ecosystem golden tests:** fixture repositories per ecosystem and per lockfile format (§6 matrix); the expected diff after `Apply` is checked in.
- **TOCTOU tests:** a plan applied after a simulated yank, cooldown change, or config edit must fail with the right code.
- **Bisect tests:** both paths — `git revert` of a split commit, and bounded re-apply for an `unsplittable` group.
- **Fakes:** `httptest` servers for registries, OSV, and forges; a fake `Sandbox` so verify logic is testable without bubblewrap.
- **MCP protocol tests:** in-memory transport pairs exercising every tool, resource, prompt, cancellation, and error code; schema snapshot tests so tool contracts do not drift; pagination envelope tests.
- **Real-sandbox test:** at least one CI job runs the actual bubblewrap path, so `SANDBOX_UNAVAILABLE` is not the only branch ever executed.
- **Security tests:** prompt-injection corpus in changelog fixtures, path-traversal attempts, and token-redaction checks.
- **CI:** `go test -race ./...`, `golangci-lint`, `govulncheck`, and a nightly end-to-end run against a throwaway git repo with a local registry mirror.

**Packaging**

- Static builds for linux/amd64, linux/arm64, and darwin/arm64 via GoReleaser, with checksums and cosign signatures. Windows is a v2 target.
- Install paths: `go install`, release tarball, and an RPM for Fedora.
- `depmcp mcp-config` prints ready-to-paste client configuration (stdio command) for Claude Desktop, Claude Code, and generic MCP clients.
- `depmcp schedule install` writes the timer; `depmcp doctor` validates the installation end to end.

**Milestones**

0. **M-1, SDK risk spike — DONE** (see `spike/mcp-probe/FINDINGS.md`). A throwaway stdio server plus a wire-level client settled all five questions against `go-sdk` v1.8.0 on Go 1.26.10, protocol 2025-06-18. Results: elicitation round-trips **but only when the client advertises the capability**; sampling works and is *not* capability-gated (so sampled text is untrusted by default); `notifications/progress` arrives mid-call and requires a caller-supplied token; schemas come free from struct tags including `outputSchema`; cancellation stops the tool but surfaces as an `isError` tool result, which is why §7 now requires handlers to persist `aborted` themselves and map `REQUEST_CANCELLED`. Prompts remain unexercised until M6.
1. **M0, skeleton — DONE.** stdio MCP server, YAML as sole authority (layered load, strict decode with line-numbered unknown keys, atomic Node-preserving edits), repository registry, per-repo `flock`, SQLite derived store with forward-only migrations, JSONL audit log, and `doctor`. Module path fixed at `github.com/thread3d/dependancythang`. Exit criterion demonstrated end-to-end: `add_repository` and `list_repositories` work from an MCP client over stdio, and deleting the entire state directory loses no configuration (the DB is rebuilt from YAML on next open). Deferred to later milestones as planned: everything that plans, applies, or verifies.
2. **M1, Go modules read path — DONE.** Inventory (`golang.org/x/mod/modfile`), cached `proxy.golang.org` client, OSV advisories with per-record hydration and hostile-text sanitizing, `scan_dependencies`, `check_updates`, `check_vulnerabilities`, `plan_updates`, `get_plan`, and the policy/planner golden tests (cooldown arithmetic per class, ignore rules with expiry, indirect suppression, advisory bypass by severity, grouping, target-aware plan identity). Exit criterion met on three real repositories — this one, `markdown-wysiwyg-editor-go`, and `FreeHire/freehire` — with correct classes and candidates from live registry data. Two limitations recorded rather than hidden: severity stays `unknown` for advisories that publish only a CVSS vector, and indirect dependencies are reported but not proposed.
3. **M2, write path — DONE.** `apply_plan` fetches the default branch, creates a worktree and `depmcp/<ecosystem>-<date>` branch, applies through the toolchain (`go get` + `go mod tidy`, never a hand-edited lockfile), and commits a conventional message carrying every change and advisory id. `revert_run` deletes branch and worktree subject to the §12 terminal-state rules. Re-validation before any write refuses stale, yanked, or still-cooling versions. Exit demonstrated twice: against a scratch repository with the real toolchain and live registry (`golang.org/x/text v0.14.0 → v0.43.0`, commit listing `GO-2026-5970` and `GO-2026-6629`, working tree untouched, clean revert), and against a deliberately failing apply, which removed its branch and worktree and recorded the run as `failed`. Bisect-by-revert belongs to M3, where a failing verify is what starts it.
4. **M3, verify and the two-phase boundary — DONE** for the bubblewrap half. `verify_run` builds and tests a branch inside bubblewrap with no network, a cleared environment, an empty home, a read-only root, a read-only module cache, and a fresh tmpfs build cache; `SANDBOX_UNAVAILABLE` refuses rather than running untrusted code unconfined, and `doctor` proves the sandbox can actually start a namespace. The credential half of the exit criterion holds structurally: Phase B receives only the six variables in `verifyEnv`, never the process environment, and the module cache it reads is mounted read-only. Exit demonstrated both ways on real repositories: a clean update reached `verified` through `go build` and `go test`, while an update whose tests fail reached `failed` with `VERIFY_FAILED`, the failing command captured, and its branch left for inspection rather than offered. Deferred and recorded: `govulncheck` reachability inside the sandbox, container sandboxing (declared in config, reports `SANDBOX_UNAVAILABLE`), and automatic bisect-by-revert.
5. **M4, npm, pnpm, Docker, GitHub Actions — DONE.** Four ecosystems behind one interface, plus the interface changes they forced: `Apply`/`ApplyResult` moved into the ecosystem package, plans apply group by group with the owning ecosystem, re-validation resolves a registry per group, and verification collects commands from every ecosystem in the repository rather than only the changed one. npm covers `package-lock.json` v1/v2/v3 (nested transitive copies excluded) and pnpm covers `pnpm-lock.yaml` v6/v9 with the peer suffix stripped; detection is mutually exclusive. Docker covers `FROM` and Compose `image:` with tag and digest updates that preserve `AS` aliases, `--platform`, and quoting, and never rewrite a mention outside a manifest. Actions covers `uses:` with hardened pinning — a SHA plus the tag as a trailing comment. Exit criterion met: golden diffs for every row of the §6 v1 matrix (`go.mod`, three package-lock shapes, two pnpm shapes, Dockerfile, Compose, workflow), plus a live run producing one commit that pinned an Action to a SHA and two images to their digests in a clean working tree. Deferred: yarn v1 and berry (v1.1), deeper tag pagination, and container-based sandboxing.
6. **M5, scheduling without a daemon — DONE.** `depmcp run` performs plan → apply → verify → notify in one process and stops before a pull request unless `auto_pr` is set. `schedule install|print` generates a systemd user service plus timer (`Persistent=true` for missed slots, `RandomizedDelaySec=10m` for jitter, and an explicit `Environment=PATH` carrying the pnpm location a timer would otherwise not see), a launchd plist, or a crontab line; cron expressions are refused as `OnCalendar` and routed to cron. Notifications are a durable JSONL log plus optional desktop and ntfy, and a failed notification never fails the run. Failure memory lives in the database — three consecutive failures suspend a repository and `resume` clears it — because a daemon would have kept that in memory. Exit criterion met: a one-shot run with no MCP server and no daemon produced a verified branch and exactly one notification; three failing runs suspended the repository, a fourth refused with `REPO_SUSPENDED`, and `resume` restored it. One bug fixed on the way: generated units passed a `--config` flag the command does not accept, and a day-scoped branch already checked out by an earlier run now supersedes instead of colliding.
7. **M6, forges and polish — DONE.** `internal/forge` implements the §11 interface for `local` (the default: a verified branch plus a markdown report), GitHub, GitLab, and Gitea/Forgejo; pull request bodies are rendered from verified facts only and carry the marker later runs use to avoid duplicates, while an unmarked request on the same branch is left alone. `open_pull_request` refuses any run that has not passed verification (`NOT_VERIFIED`), and pushes with the user's git credentials rather than a held token. `import_dependabot` reads `.github/dependabot.yml` and reports the mapping without writing, listing every key that has no depmcp equivalent. The four prompts from §7 are registered and are written as instructions over tool results. Not yet done from this milestone's wording: tokens come from environment variables rather than the OS keyring, `auto_pr` is still not honoured by scheduled runs, and the prompts have not been exercised against a live model.
8. **M7, v2 breadth.** pip/uv/poetry, Cargo, yarn v1/berry; Windows if it matters by then.

## 15. Revision history

- **Rev 2 (this document).** Settled three architectural questions: the v1 daemon is cut in favour of external timers and a one-shot CLI; credentials and untrusted code are separated into two phases; the YAML config is declared the sole authority and SQLite is reduced to derived state. Also folded in: explicit TOCTOU re-validation, fetch-before-branch, commit granularity that admits what the toolchain does, bisect by `git revert`, cap semantics evaluated at plan time, pagination on every list tool, forced OSV refresh on explicit calls, opaque versions with `TagSource` for Docker/Actions, a corrected run state machine, signing preflight, Windows and yarn demoted, a milestone-zero SDK spike, and the misspelled-module-path warning.
- **Rev 1 (commit `5d111f4`).** Initial specification.
