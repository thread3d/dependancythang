# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions track the
milestones in [the specification](specs/Local%20Dependency%20Updater%20MCP%20Server%20(Go)%20%E2%80%94%20Specification.md).

## [Unreleased]

### Planned
- M2: write path — planner output applied to a worktree, `apply_plan`,
  `revert_run`, confirmation flow, TOCTOU re-validation on apply.

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
