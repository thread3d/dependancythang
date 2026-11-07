# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions track the
milestones in [the specification](specs/Local%20Dependency%20Updater%20MCP%20Server%20(Go)%20%E2%80%94%20Specification.md).

## [Unreleased]

### Planned
- M4: npm/pnpm, Docker, and GitHub Actions ecosystems behind the same interface.

## [0.4.0] — 2026-11-07

The M3 verify stage: an update is now proven before anything is proposed.

### Added
- **Sandboxed verification** (`verify_run`, `depmcp verify`). The branch is built
  and tested inside bubblewrap with no network, a stripped environment, an empty
  home, a read-only root, and exactly one writable directory — the worktree.
- **The Phase B credential boundary is real.** The module cache is mounted
  read-only; a shared writable build cache is deliberately not used, because
  cross-run cache poisoning is a supply-chain path. Each verification gets a
  fresh tmpfs build cache and pays for it in compile time.
- **`SANDBOX_UNAVAILABLE` instead of unconfined execution.** No sandbox means no
  verification, unless `verify.sandbox: none` is configured explicitly — and then
  every result is marked `unconfined`.
- **A failing verify blocks the run**: status `failed`, `VERIFY_FAILED` or
  `VERIFY_TIMEOUT`, first failing command captured with bounded output, and the
  branch left in place for inspection rather than offered.
- `depmcp doctor` now reports whether the configured sandbox can actually start a
  namespace, not merely whether the binary exists.
- Schema migration 4 records `verify_status`, `verify_summary`, `verify_sandbox`,
  and `verified_at` on each run.

### Fixed
- The sandbox mounted its private `/tmp` after the worktree bind, so any worktree
  under `/tmp` was replaced by an empty directory.
- `/etc/alternatives` was not bound, so `/usr/bin/go` — a symlink through the
  alternatives system — dangled inside the sandbox and every command failed with
  `execvp go: No such file or directory`.

### Known limitations
- Only the Go ecosystem supplies verify commands; nothing else can be verified
  yet.
- Container sandboxing (`verify.sandbox: container`) is declared in the
  specification but reports `SANDBOX_UNAVAILABLE` — bubblewrap or nothing.
- `govulncheck` reachability is not yet run inside the sandbox.
- Bisect-by-revert is not automatic: a failing group is reported, not subdivided.

## [0.3.0] — 2026-11-07

The M2 write path: depmcp now applies a plan on a new branch, and can undo it.
Nothing runs the project's tests yet — that is M3.

### Added
- **`apply_plan`**: fetches the default branch, creates a real `git worktree` and
  a `depmcp/<ecosystem>-<date>` branch, runs the toolchain there, and commits.
  The user's working tree, index, and uncommitted work are never touched.
- **TOCTOU re-validation** before any write: a plan whose target version has
  vanished, been yanked, or slipped back inside its cooldown is refused with
  `PLAN_STALE`, `VERSION_YANKED`, or `COOLDOWN_ACTIVE` rather than silently
  retargeted.
- **`revert_run`**: deletes the branch and worktree, subject to the §12 state
  machine — a merged or closed run is terminal and is only reported.
- **`list_runs` / `get_run`**, plus CLI verbs `apply`, `revert`, `runs`.
- **Conventional commit messages** generated from plan data, listing every change,
  its update class, and the advisory ids that drove it.
- **Crash-safe apply**: a failure after the worktree exists removes both the
  worktree and the branch, and records the run as `failed` with the real error.
- **CVSS base scores** (v3.0 and v3.1). Advisories that publish only a vector now
  get a computed severity instead of `unknown`, and each advisory carries a
  numeric score plus the provenance of its label (`database`, `cvss`, `alias:<id>`,
  or `none`).
- **`unknown` severity is now scored, not missing**: it has a defined score and
  rank below `low`, so it participates in sorting and thresholding while never
  triggering a cooldown bypass.
- Schema migration 3 adds the `runs` table.

### Changed
- Advisory severity resolution follows a chain: declared label, then computed
  CVSS, then the aliased record (Go advisories usually alias a GHSA that carries
  severity), then `unknown`.

### Known limitations
- Verification (builds and tests in a sandbox) is not implemented; a run stops at
  `applied`.
- Advisories whose records carry no severity, no CVSS vector, and no informative
  alias remain `unknown` — for example `GO-2026-5024`, whose CVE alias is silent
  too. This is a data limitation, not a parsing gap, and is now visible as
  `severity_source: none`.
- CVSS v4 vectors are not scored; those records fall through to the alias lookup.

## [0.2.0] — 2026-11-07

The M1 read path: depmcp now finds out what is outdated and what is vulnerable.
Nothing is applied yet.

### Added
- **Go module inventory** via `golang.org/x/mod/modfile`, the same parser the
  toolchain uses. Direct and indirect requirements are distinguished, nested
  modules are discovered, and `vendor/` is skipped.
- **Registry client** for `proxy.golang.org` with a shared on-disk HTTP cache:
  ETag revalidation, TTL, and an offline mode that serves stale data rather than
  failing a scan.
- **OSV advisory client.** The batch endpoint returns ids only, so records are
  hydrated individually and cached; results are normalized to id, aliases,
  severity, and fixed versions.
- **Advisory text is treated as hostile input**: markup and markdown links are
  stripped, whitespace collapsed, and detail text truncated before it can reach
  a model's context.
- **Policy engine**, pure and clock-injected: ignore rules (with mandatory
  reason, version ranges, expiry), cooldown per update class, indirect
  dependency suppression, allowed-class limits, advisory bypass by severity, and
  grouping. Order of evaluation follows the specification exactly.
- **Planner** producing immutable, content-addressed plans with per-target
  grouping, so two modules in one repository never collapse together.
- **Five MCP tools added**: `scan_dependencies`, `check_updates`,
  `check_vulnerabilities`, `plan_updates`, `get_plan` — twelve tools total.
- **CLI verbs**: `scan`, `check`, `vulns`, `plan`.
- Schema migration 2 adds inventory snapshots, advisory rows, and plans.

### Changed
- `depmcp version` reports build identity from a dedicated package, injectable
  with `-ldflags -X`.

### Fixed
- A cancelled MCP request surfaces as a tool result with `isError: true`.

### Known limitations
- **Severity is often `unknown`.** Go advisory records frequently carry only a
  CVSS vector, and depmcp does not yet compute base scores from it; the label is
  reported only when the feed supplies one directly.
- Indirect dependencies are inventoried and reported but never proposed, except
  when an advisory forces the bump.
- Advisories and candidates are resolved one dependency at a time; large
  repositories are slow on a cold cache.
- No apply, verify, or forge integration yet.

## [0.1.0] — 2026-11-07

First tagged release: the M0 skeleton. Configuration authority, derived state, and
the MCP surface that reads and edits it. Nothing yet plans, applies, or verifies an
update.

### Added
- **MCP server over stdio** with seven tools: `list_repositories`,
  `add_repository`, `update_repository`, `remove_repository`, `get_config`,
  `validate_config`, `status`. Tool annotations follow the MCP hints, and failures
  carry stable error codes.
- **YAML as the single source of truth.** Tracked repositories and policy live in
  `$XDG_CONFIG_HOME/depmcp/config.yaml` plus an optional per-repository
  `.depmcp.yml`, layered over built-in defaults.
- **Strict configuration decoding** that reports unknown keys with line and column,
  found by walking the YAML node tree alongside the Go type rather than trusting
  the decoder's message.
- **Atomic, comment-preserving edits.** Mutation tools rewrite the YAML node tree,
  keep key order and comments, write temp → fsync → rename, detect a concurrent
  hand-edit by hash, and refuse to write a document that will not load back.
- **Derived state in SQLite** (pure Go, no CGO) with forward-only migrations and a
  reconciler that rebuilds repository state from configuration. A repository
  dropped from config is marked orphaned, never deleted, so branch and run history
  stays attributable.
- **Per-repository locking** via `flock`. There is no daemon and no IPC: the lock
  file is the entire concurrency design, and the OS releases it when a process
  dies.
- **Append-only JSONL audit log**, one line per tool call, mirrored into a
  queryable table.
- **`depmcp doctor`** checking git, sandbox availability, ecosystem CLIs, commit
  signing, and keyring prerequisites.
- **M-1 SDK spike** (`spike/mcp-probe/`) with a wire-level client, settling five
  capability questions about the official Go MCP SDK by running them.

### Fixed
- A cancelled MCP request surfaces as a tool result with `isError: true`, not a
  JSON-RPC error, and carries no structured output — so handlers must persist
  aborted state themselves.

### Known limitations
- Planning, applying, and verifying updates are not implemented; this release is
  infrastructure only.
- Windows is not a target; macOS is untested.
- Prompts and forge integrations arrive in later milestones.
