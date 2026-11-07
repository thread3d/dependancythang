# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions track the
milestones in [the specification](specs/Local%20Dependency%20Updater%20MCP%20Server%20(Go)%20%E2%80%94%20Specification.md).

## [0.9.1] — 2026-11-07

Two bugs the first live run of the v2 ecosystems exposed.

### Fixed
- **crates.io returned nothing.** The registry rejects clients without a
  `User-Agent`, so every Cargo lookup failed silently as "no candidate". The
  shared HTTP cache now identifies itself on every request, which also satisfies
  GitHub's preference.
- **A phantom `name` dependency appeared in Cargo projects.** The dependency
  regex matched `[package]`'s own `name` and `version` keys. Parsing is now
  scoped to dependency tables, so a crate's identity is never mistaken for one of
  its dependencies.

### Verified
Live: `requests 2.25.0 → 2.34.2` (minor, PyPI), `serde 1.0.180 → 1.0.229` (patch,
crates.io), and `flask` correctly reporting no current version for a range
constraint. Inventory on the same repository now lists exactly its three real
dependencies.

## [Unreleased]

### Planned
- Nothing scheduled. The specification's v1 and v2 ecosystems are implemented.

## [0.9.0] — 2026-11-07

M7: the v2 breadth ecosystems — Python, Cargo, and yarn.

### Added
- **Python** (`pip`/`uv`/`poetry`): reads `requirements*.txt` and
  `pyproject.toml`, understanding both PEP 621 dependency arrays and poetry's
  table form. Only an exact `==` pin yields a current version — a range yields
  none, because inventing one would be a lie. Applying rewrites a `==` pin in a
  requirements file directly (that file is a manifest, not a lock) and delegates
  pyproject projects to the tool the repository actually uses (`uv` if `uv.lock`
  is present, `poetry` if `poetry.lock`, otherwise pip).
- **Cargo**: reads `Cargo.toml` and resolves current versions through
  `Cargo.lock`, ignoring the package's own `version` field. Applying delegates to
  `cargo update -p <crate> --precise <version>`, because the lockfile is cargo's
  to write.
- **yarn v1**: reads `yarn.lock` entries, including multi-range headers. A yarn
  berry lockfile is refused rather than misread, and a project carrying
  `package-lock.json` or `pnpm-lock.yaml` is left to that manager.
- **PyPI and crates.io tag sources**, joining npm, Docker Hub, and GitHub in the
  shared ETag cache. A PyPI release with no files is treated as yanked.
- All seven ecosystems are detected, inventoried, planned, applied, and verified
  through the same interface, with no per-ecosystem branching in the write path.

### Fixed
- Poetry's dependency tables were not parsed at all, so a poetry project
  inventoried nothing.
- A range constraint was briefly handed to `pip install`, which would have
  installed packages for a constraint depmcp had deliberately left alone. A
  requirements file is now edited and never delegated.

### Known limitations
- Python, Cargo, and yarn apply steps shell out to the real tools; the tests cover
  the manifest rewriting and the command contract with a fake runner, not a live
  toolchain run.
- yarn berry is out of scope by design, and yarn detection is lockfile-based, so it
  finds resolved packages rather than declared ones.
- Python verification assumes pytest is present; there is no per-project test
  discovery yet.

## [0.8.0] — 2026-11-07

Closes the gap between the design and the code: forge tokens now live in the OS
keyring.

### Added
- **`internal/secrets`**: a keyring store with an explicit `Backend` interface
  (OS keyring in production, in-memory in tests). Entries are namespaced
  `depmcp` / `<forge>:token`, so a user can see which token is which.
- **Token resolution order is environment, then keyring.** The environment stays
  supported for CI and containers without a session bus; the keyring is the
  ordinary path. Nothing is ever written to configuration, the state database, or
  a log.
- **`depmcp secret set|delete|check`.** Tokens are read from stdin, never argv, so
  they do not land in shell history or a process listing, and echo is disabled on
  a terminal.
- **`doctor` probes the keyring by round trip** — a session bus can exist while
  the collection is locked or absent, and the old check only looked for the
  variable.

### Changed
- `Service` owns a `secrets.Store`, so the keyring is a seam that tests can
  replace rather than a global.

### Known limitations
- There is no plaintext fallback by design: on a machine with no keyring the
  environment variable is the only route, and `doctor` says so.

## [0.7.0] — 2026-11-07

The M6 milestone: depmcp can publish, and it can read a dependabot.yml.

### Added
- **Forge layer** (`internal/forge`). `local` is the default and stops at a
  verified branch, writing a markdown report; GitHub, GitLab, and Gitea/Forgejo
  open real pull requests through their REST APIs. Pull request bodies are
  rendered from verified facts only — a change table, advisory ids, the
  verification result, and the exact `depmcp revert` command — and carry a marker
  so later runs recognise and reuse their own requests instead of opening
  duplicates. A pull request on the same branch without the marker is treated as
  someone else's and left alone.
- **`open_pull_request`** (MCP) and `depmcp pr`: refuse any run that has not
  passed verification, with `NOT_VERIFIED`. Pushing is done with the user's own
  git credentials, so no token is held for that step.
- **`import_dependabot`** and `depmcp import-dependabot`: read
  `.github/dependabot.yml` and report the mapping without writing anything.
  Supported ecosystems map one-to-one (`gomod`, `npm`, `pnpm`, `docker`,
  `github_actions`); keys with no depmcp equivalent — `labels`, `reviewers`,
  `assignees`, `open-pull-requests-limit`, `target-branch`, `registries`, and
  others — are listed rather than dropped. Ignore rules arrive with a placeholder
  reason, because dependabot does not record one and depmcp requires it.
- **The four prompts** from §7: `triage_vulnerabilities`, `review_updates`,
  `explain_breaking_change`, and `weekly_digest`. They are written as instructions
  over tool results, so they cannot invent findings, and they say explicitly that
  release notes are untrusted and that an unscored advisory is not a low one.
- Twenty MCP tools and four prompts.

### Fixed
- Forge tokens were about to be asserted in a shared test handler, which would
  have made every tokenless test fail; authentication is now verified in one
  dedicated test.

### Known limitations
- Tokens come from `DEPMCP_GITHUB_TOKEN`, `DEPMCP_GITLAB_TOKEN`, and
  `DEPMCP_GITEA_TOKEN` rather than the OS keyring. The keyring wrapper is
  designed (§3) but not wired, so `doctor` still only reports whether a session
  bus exists.
- `auto_pr` in configuration is still not honoured by the scheduled run; a
  remote run must publish explicitly.
- Prompt text is not yet exercised against a live model.

## [0.6.0] — 2026-11-07

The M5 milestone: depmcp runs on its own, with no daemon and no MCP server in the
loop.

### Added
- **`depmcp run`** — the one-shot scheduled job: plan, apply, verify, notify. It
  stops before opening a pull request unless `auto_pr` is set, because an
  unattended run should not speak to a remote on its own.
- **`depmcp schedule install|print`** generating real OS artifacts instead of a
  goroutine: a systemd user service plus timer (`OnCalendar`, `Persistent=true`
  so a missed slot fires once on wake, `RandomizedDelaySec=10m` so every
  repository does not hit registries at :00), a launchd plist, or a crontab line.
  Cron expressions are refused as `OnCalendar` and routed to the cron artifact.
- **The systemd PATH trap is handled explicitly**: units bake in an
  `Environment=PATH` that includes `~/.local/share/pnpm/bin`, `~/.local/bin`, and
  `~/go/bin`, because a timer does not read a shell profile.
- **Notifications**: a durable JSONL log always written (a headless box has no
  popup), plus `notify-send` and ntfy when configured. A failed notification
  never fails the run.
- **Failure memory without a daemon**: consecutive failures live in the database,
  three suspends the repository, and `depmcp resume` clears the count. `status`
  reports the count and whether a repository is suspended.
- **Worktree supersede**: a day-scoped branch already checked out by an earlier
  run is replaced instead of colliding, which is what a retry or a double-firing
  timer produces.

### Fixed
- Generated units passed a `--config` flag that the `run` command does not
  accept; configuration and state travel as environment variables, which the unit
  already set.

### Verified
A one-shot `depmcp run` with no MCP server and no daemon produced a verified
branch (`[go build ./... go test ./...] passed` in the sandbox) and wrote exactly
one notification. Three failing runs incremented the counter to 3 and suspended
the repository; a fourth refused with `REPO_SUSPENDED`; `depmcp resume` cleared
it, after which a clean run verified again. Only one branch per day existed
throughout, because later runs supersede earlier ones.

### Known limitations
- `auto_pr` is accepted but has no forge to talk to yet (M6).
- Timer artifacts are generated and installed, not enabled: enabling is left to
  the user, who sees the exact `systemctl` command.
- Only the file, desktop, and ntfy channels exist; no webhook payload shape has
  been designed.

## [0.5.0] — 2026-11-07

The M4 ecosystems: npm, pnpm, Docker, and GitHub Actions behind one interface,
plus the interface changes that made that possible.

### Added
- **npm and pnpm** (`internal/ecosystem/nodejs`, one implementation, two names).
  `package.json` is read for runtime, optional, peer, and dev requirements; the
  lockfile supplies the pinned version. All three `package-lock.json` shapes are
  handled — v1 `dependencies`, v2 both sections with `packages` winning, v3
  `packages` only — and nested transitive copies are not mistaken for the
  top-level package. pnpm's `.yaml` is read for v6 and v9, with the peer suffix
  (`4.17.21(react@18)`) stripped. Detection is mutually exclusive, so a project
  is never inventoried under two names, and a stale `package-lock.json` beside a
  `pnpm-lock.yaml` does not make it npm.
- **Docker**: `FROM` lines and Compose `image:` keys, with tag and digest
  updates. Rewrites preserve everything else on the line — the `AS build` alias,
  `--platform` flags, quoting — and never touch a mention outside a manifest
  (a `LABEL` naming `alpine:3.20` stays put). `scratch` is skipped, registry
  ports (`localhost:5000/app:2.1`) are not read as tags, and an unqualified image
  implies `latest`.
- **GitHub Actions**: `uses:` refs, with hardened pinning. A change carrying a
  commit SHA is written as the SHA plus the human-readable tag as a trailing
  comment. Local actions (`./x`) and `docker://` refs are skipped, one entry per
  action regardless of how many jobs use it.
- **Tag sources** (`internal/tags`): registry.npmjs.org, Docker Hub, and the
  GitHub REST API, all through the shared ETag cache. Docker Hub and GitHub
  supply a digest/SHA alongside each tag, which is what the pinning above needs.
- **Ecosystem-agnostic write path**: `Apply` and `ApplyResult` moved into the
  ecosystem package, plans apply group by group with the owning ecosystem, and
  re-validation resolves the right registry per group. A plan spanning a
  Dockerfile and a workflow now applies both in one commit.
- **Project-level verification**: verify collects commands from every ecosystem
  detected in the repository rather than only the changed one, so a workflow-only
  change is still proved by the project's Go or npm build.
- `ClassPin` and `ClassDigest` update classes, so tags and refs are never
  mislabelled as unclassifiable, and both use the patch cooldown.
- Schema: plans and decisions carry a `digest`; dependencies carry `dev`.

### Fixed
- Plan groups were labelled with a single hardcoded ecosystem, and a leftover
  `SetTarget` stamped the first target onto every group — so a two-ecosystem plan
  claimed every change belonged to `gomod` on `docker:/`.
- Plan re-validation resolved the Go module proxy for every change, so an Action
  ref was sent to the Go proxy and rejected as a malformed module path.

### Verified
Golden diffs for every row of the specification's lockfile matrix: `go.mod`,
`package-lock.json` v1/v2/v3, `pnpm-lock.yaml` v6/v9, Dockerfile, Compose, and a
workflow file. Live end-to-end: a repository holding a Dockerfile, a Compose file,
and a workflow produced one commit pinning `actions/checkout` to a SHA with a
`# v7.0.1` comment and both images to their digests, leaving the working tree
clean.

### Known limitations
- yarn v1 and yarn berry are still unimplemented (v1.1 in the plan).
- Docker Hub and GitHub tag lists are read one page deep (100 tags), and GitHub's
  anonymous API is rate limited.
- npm verification needs `node_modules` from the apply step; the sandbox has no
  network and no shared build cache, so a repository without a lockfile cannot be
  verified.

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
