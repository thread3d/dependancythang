# depmcp faults found in itself

Generated from `issues.jsonl` and `resolved.jsonl`; do not edit by hand.
Read the open entries, fix them, then record a resolution with `depmcp faults resolve <id>`.
A fault seen again after its resolution reopens automatically.

2 open, 8 resolved.

## DEP-445BAE19 — the delegated appliers record the requested change as the observation

- kind: `tool-failure` · severity: `bug` · seen 1 time(s)
- first: 2026-11-08T13:01:27.500456236Z · last: 2026-11-08T13:01:27.500456236Z
- command: `faults add`
- **fix:** Read the manifest or lockfile back after the toolchain runs, the way nodejs and gomod already do, so Observed is a measurement rather than an echo of the request.

```
v2eco Python's delegated path (pip/uv/poetry), and Cargo/Yarn/Berry, set res.Observed[ch.Name] = {ch.From, ch.To} straight from the change instead of reading the lockfile back. gomod and nodejs do read back. Consequence: a toolchain that resolved something other than what was asked is invisible in the run record, and the apply-boundary check added for DEP-D516B032 cannot catch a version disagreement for those ecosystems, because the observation always matches the plan by construction.
```

## DEP-C1D642B4 — apply ignores a group's target, so a plan for a sub-module manifest is applied to the repository root

- kind: `tool-failure` · severity: `bug` · seen 1 time(s)
- first: 2026-11-08T13:01:25.593910655Z · last: 2026-11-08T13:01:25.593910655Z
- command: `faults add`
- **fix:** Give the applier the target it is working on: either pass the target's directory as dir (with an ecosystem-level rule for ecosystems whose Apply already means the whole repository, such as the workflows directory) or add ApplyIn(ctx, root, target, changes) to the interface.

```
applyGroups calls ecosystem.Apply(ctx, run.Worktree, changes) for every group, so dir is always the repository root while gomod.Apply reads filepath.Join(dir, go.mod). A plan built from a monorepo with go.mod in the root and in svc/ therefore runs go get for both groups against the root go.mod: the sub-module change either lands in the wrong file or, now that appliers verify the from-ref, is refused as PLAN_STALE. The same blindness affects docker (a plan for web/Dockerfile) and maven (any second pom). Found while fixing DEP-D516B032; the plan side now allows two targets to share a dependency and version, which makes this reachable more often.
```

## Resolved

- DEP-E52DB5F0 — plan_updates panics when the plan has no groups (resolved 2026-11-08T12:53:42.170236767Z: PlanUpdates stores an empty plan with an empty target row through firstTarget instead of indexing plan.Groups[0]; apply_plan then reports NOTHING_TO_APPLY. Tested in internal/app/readpath_empty_test.go)
- DEP-4B802954 — a plan depmcp built itself is rejected as PLAN_STALE because the duplicate-change check ignores the target (resolved 2026-11-08T12:53:42.158070591Z: ScanDependencies keeps one dependency per (target, name) and writes a target's snapshot once, so two workflow files pinning one action no longer become two changes; planner.Validate identifies a change by (target, dependency, version), so a plan for two go.mod files is no longer a duplicate either, while the same change twice for one target is still refused. rMath's plan now has 3 changes instead of 4 and applies; tested in internal/app/readpath_test.go, internal/app/writepath_test.go and internal/planner/planner_test.go)
- DEP-435CC37F — OSV fixed versions without a v prefix were dropped by policy (resolved 2026-11-08T06:51:42.207263797Z: fixed in v0.9.2 by normalizing the v prefix in gomod.Compare and Classify)
- DEP-A569750B — no panic recovery at MCP tool boundaries (resolved 2026-11-08T08:54:00.344768791Z: every tool now registers through addTool, which recovers a panic, records a KindInternal fault with its stack, and returns INTERNAL_ERROR as an isError result; tested in internal/mcpserver/panic_internal_test.go)
- DEP-9D45551D — yarn berry is refused rather than supported (resolved 2026-11-08T08:54:00.380526834Z: v2eco.Berry reads a berry lockfile (__metadata, @npm: descriptors, multi-descriptor keys) with yarn up for apply; yarn v1 now declines berry files so the two never misread each other)
- DEP-9DE3A533 — scheduled runs ignore auto_pr (resolved 2026-11-08T08:54:00.368905871Z: RunOnce now publishes through the configured forge when defaults.auto_pr is true, reporting the PR url or the local report path; tested in internal/app/autopr_test.go)
- DEP-B8A77617 — advisory target version is recorded without the v prefix (resolved 2026-11-08T08:54:00.356896Z: ecosystem.VersionNormalizer added; gomod implements it and PlanUpdates normalizes every decision's target, so an advisory fix reads v0.44.0)
- DEP-D516B032 — the actions applier rewrites any ref, so a plan built from the working tree can be applied to a different base and the commit states the wrong from-version (resolved 2026-11-08T13:01:23.188500821Z: The hand-editing appliers now apply a planned change only to a line that still holds the plan's from-ref, and refuse the plan when a manifest holds anything else: actions, docker and maven return ecosystem.PlanMismatchError before writing, which the write path maps to PLAN_STALE. A ref that already holds the planned target counts as satisfied, so one plan still covers two workflow files. applyIntoWorktree additionally cross-checks every planned change against the ApplyResult (Observed, or a reason in the new ApplyResult.Skipped), so an applier that reports success without moving anything is refused too; maven reports its property-indirection and managed-version declines in Skipped and the commit says what did not land. Tested in internal/ecosystem/{actions,docker,maven} and internal/app/writepath_test.go; verified end to end against a clone of rMath: a plan stored against v4 refs applied to a branch now holding v7 fails with PLAN_STALE naming each ref, and re-planning then applying pins all three actions.)
