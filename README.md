# DependancyThang — `depmcp`

A local dependency updater exposed over the [Model Context Protocol](https://modelcontextprotocol.io):
Dependabot's workflow, with no hosted service. It finds outdated and vulnerable
dependencies in local repositories, turns them into reviewed update branches, and
runs the project's build and tests before proposing anything.

**Status: v0.11.0 — backlog clear.** depmcp inventories a repository's
dependencies across **Go modules, npm, pnpm, yarn v1 and berry, Docker, GitHub
Actions, Python (pip/uv/poetry), and Cargo**, finds
newer versions and known vulnerabilities, builds a policy-filtered plan, applies
it on a new git branch, and proves the result by building and testing it inside a
sandbox — then runs on a timer with no daemon and no server, and can open pull
requests on GitHub, GitLab, or Gitea once the result is verified. See
[CHANGELOG.md](CHANGELOG.md) for exactly what is in and out. Design: [the specification](specs/Local%20Dependency%20Updater%20MCP%20Server%20(Go)%20%E2%80%94%20Specification.md);
verified SDK behaviour: [M-1 spike findings](spike/mcp-probe/FINDINGS.md).

There is no daemon and no listening socket. `depmcp serve` speaks MCP on
stdin/stdout; scheduling will be an external systemd timer invoking one-shot
commands.

## Build

Requires Go ≥ 1.25 and no C compiler (builds are `CGO_ENABLED=0`).

```bash
go build ./...
go test ./...
go build -ldflags "-X github.com/thread3d/dependancythang/internal/version.Commit=$(git rev-parse --short HEAD) \
  -X github.com/thread3d/dependancythang/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o ~/.local/bin/depmcp ./cmd/depmcp
depmcp version
```

## Use

```bash
depmcp scan ~/src/project  # dependency inventory (JSON)
depmcp check ~/src/project # available updates
depmcp vulns ~/src/project # advisories affecting current versions
depmcp plan ~/src/project  # policy-filtered update plan
depmcp apply <plan-id> --confirm   # apply on a new branch (worktree)
depmcp verify <run-id>     # build and test that branch in a sandbox
depmcp runs                # recent runs
depmcp revert <run-id>     # delete a run's branch and worktree
depmcp run --repo ~/src/project        # one-shot: plan → apply → verify → notify
depmcp schedule install --repo ~/src/project
depmcp schedule print --repo ~/src/project --kind cron
depmcp resume --repo ~/src/project     # clear a suspended repository
depmcp pr <run-id>         # publish a verified branch and open a pull request
depmcp import-dependabot ~/src/project # map an existing dependabot.yml
depmcp doctor              # environment checks
depmcp add ~/src/project   # track a git repository (writes YAML)
depmcp list                # tracked repositories, JSON
depmcp remove ~/src/project
depmcp validate            # validate the configuration
depmcp serve               # MCP server on stdio
```

Register it with an MCP client (example: Claude Code):

```bash
claude mcp add depmcp ~/.local/bin/depmcp serve
```

Tools (22): `list_repositories`, `add_repository`, `update_repository`,
`remove_repository`, `get_config`, `validate_config`, `status`,
`scan_dependencies`, `check_updates`, `check_vulnerabilities`, `plan_updates`,
`get_plan`, `apply_plan`, `verify_run`, `revert_run`, `list_runs`, `get_run`,
`open_pull_request`, `import_dependabot`, `list_faults`, `record_fault`,
`resolve_fault`.

Prompts (4): `triage_vulnerabilities`, `review_updates`,
`explain_breaking_change`, `weekly_digest`.

Forge tokens live in the OS keyring:

```bash
depmcp secret set github     # paste the token; it is never echoed or logged
depmcp secret check          # is the keyring usable?
```

`DEPMCP_GITHUB_TOKEN`, `DEPMCP_GITLAB_TOKEN`, and `DEPMCP_GITEA_TOKEN` still work
and take precedence, for CI and machines without a session bus. Self-hosted
instances set `DEPMCP_FORGE_BASE`.

`apply_plan` confirms through MCP elicitation when the client supports it and
requires `confirm: true` otherwise; the summary it shows is computed from the
stored plan, never from model output.

## Configuration

YAML is the single source of truth: which repositories are tracked and what policy
applies is committed, reviewable text. The SQLite database under the state
directory holds only derived state (scan timestamps, runs, caches, audit index), so
deleting it loses history, never intent.

- Global: `$XDG_CONFIG_HOME/depmcp/config.yaml` (usually `~/.config/depmcp/config.yaml`)
- Per repository: `.depmcp.yml` in the repository root
- Overrides: `DEPMCP_CONFIG`, `DEPMCP_STATE_DIR`

Mutation tools edit the YAML node tree, preserving comments and key order, then
write temp → fsync → rename, and detect concurrent hand-edits by hash.

## Self-defect journal

depmcp records the defects it finds in itself in `<repo>/.depmcp/faults/`, so a
later session can be asked to fix them:

```bash
depmcp faults                      # open entries, bugs first
depmcp faults add "summary" --kind capability-gap --hint "what to do"
depmcp faults resolve DEP-1A2B3C4D --note "fixed in <commit>"
```

`FAULTS.md` is a regenerated digest; `issues.jsonl` and `resolved.jsonl` are the
records. A fault seen again after being resolved reopens automatically, so a
regression cannot hide behind an old fix. See the current backlog in
[.depmcp/faults/FAULTS.md](.depmcp/faults/FAULTS.md).

## Layout

```
cmd/depmcp/        CLI: serve, add, remove, list, doctor, status, validate
internal/version/  build identity, injected with -ldflags -X
internal/app/      wiring, stable error codes, doctor
internal/config/   layered load, strict decode, atomic Node-preserving edits
internal/mcpserver MCP tool registration
internal/store/    SQLite derived state + migrations
internal/repolock/ per-repository flock (the whole concurrency design)
internal/audit/    append-only JSONL audit log
internal/ecosystem/  ecosystem interface + gomod inventory and classification
internal/registry/   Go module proxy client (cached)
internal/ecosystem/  gomod, nodejs (npm/pnpm), docker, actions, v2eco (pip/cargo/yarn)
internal/tags/       npm registry, Docker Hub, GitHub tag sources
internal/execx/      command runner shared by every ecosystem
internal/advisory/   OSV client, normalization, untrusted-text sanitizing
internal/policy/     ignore/cooldown/indirect/class/grouping rules (pure)
internal/planner/    immutable, content-addressed update plans
internal/gitx/       git worktrees, branches, commits (shells out to git)
internal/sandbox/    bubblewrap and unconfined runners (Phase B isolation)
internal/verify/     build/test orchestration and verdicts
internal/schedule/   systemd/launchd/cron artifact generation
internal/notify/     file, desktop, and ntfy notification channels
internal/forge/      local, GitHub, GitLab, and Gitea publishing
internal/dependabot/ .github/dependabot.yml import
internal/faults/     self-defect journal (.depmcp/faults)
internal/secrets/    OS keyring store for forge tokens
internal/httpcache/  shared on-disk HTTP cache with ETags
spike/mcp-probe/   throwaway SDK capability probe from M-1
specs/             the specification
```
