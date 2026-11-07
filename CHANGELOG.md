# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions track the
milestones in [the specification](specs/Local%20Dependency%20Updater%20MCP%20Server%20(Go)%20%E2%80%94%20Specification.md).

## [Unreleased]

### Planned
- M1: Go-modules read path — inventory, registry lookup, advisories, plans.

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
