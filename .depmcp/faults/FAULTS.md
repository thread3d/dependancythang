# depmcp faults found in itself

Generated from `issues.jsonl` and `resolved.jsonl`; do not edit by hand.
Read the open entries, fix them, then record a resolution with `depmcp faults resolve <id>`.
A fault seen again after its resolution reopens automatically.

1 open, 7 resolved.

## DEP-D516B032 — the actions applier rewrites any ref, so a plan built from the working tree can be applied to a different base and the commit states the wrong from-version

- kind: `friction` · severity: `note` · seen 1 time(s)
- first: 2026-11-08T12:54:13.524752813Z · last: 2026-11-08T12:54:13.524752813Z
- command: `faults add`
- **fix:** Either make the actions applier honour Change.From and report the lines it actually changed, or build the plan from the same fetched default branch apply will use, so Observed and the commit message describe the diff that was really produced.

```
Verification of the PLAN_STALE fix: the plan was built from a working tree where both workflows pinned actions/checkout@v4, while the clone's default branch still had @v7. Apply based the run on the default branch, the applier matched the action by name and rewrote the v7 lines to the v7.0.1 SHA, and the commit message read 'actions/checkout v4 -> v7.0.1 (major)' with Observed reporting v7 as the from-version. The rewritten file is a correct hardened pin, but the record of it is wrong.
```

## Resolved

- DEP-E52DB5F0 — plan_updates panics when the plan has no groups (resolved 2026-11-08T12:53:42.170236767Z: PlanUpdates stores an empty plan with an empty target row through firstTarget instead of indexing plan.Groups[0]; apply_plan then reports NOTHING_TO_APPLY. Tested in internal/app/readpath_empty_test.go)
- DEP-4B802954 — a plan depmcp built itself is rejected as PLAN_STALE because the duplicate-change check ignores the target (resolved 2026-11-08T12:53:42.158070591Z: ScanDependencies keeps one dependency per (target, name) and writes a target's snapshot once, so two workflow files pinning one action no longer become two changes; planner.Validate identifies a change by (target, dependency, version), so a plan for two go.mod files is no longer a duplicate either, while the same change twice for one target is still refused. rMath's plan now has 3 changes instead of 4 and applies; tested in internal/app/readpath_test.go, internal/app/writepath_test.go and internal/planner/planner_test.go)
- DEP-435CC37F — OSV fixed versions without a v prefix were dropped by policy (resolved 2026-11-08T06:51:42.207263797Z: fixed in v0.9.2 by normalizing the v prefix in gomod.Compare and Classify)
- DEP-A569750B — no panic recovery at MCP tool boundaries (resolved 2026-11-08T08:54:00.344768791Z: every tool now registers through addTool, which recovers a panic, records a KindInternal fault with its stack, and returns INTERNAL_ERROR as an isError result; tested in internal/mcpserver/panic_internal_test.go)
- DEP-9D45551D — yarn berry is refused rather than supported (resolved 2026-11-08T08:54:00.380526834Z: v2eco.Berry reads a berry lockfile (__metadata, @npm: descriptors, multi-descriptor keys) with yarn up for apply; yarn v1 now declines berry files so the two never misread each other)
- DEP-9DE3A533 — scheduled runs ignore auto_pr (resolved 2026-11-08T08:54:00.368905871Z: RunOnce now publishes through the configured forge when defaults.auto_pr is true, reporting the PR url or the local report path; tested in internal/app/autopr_test.go)
- DEP-B8A77617 — advisory target version is recorded without the v prefix (resolved 2026-11-08T08:54:00.356896Z: ecosystem.VersionNormalizer added; gomod implements it and PlanUpdates normalizes every decision's target, so an advisory fix reads v0.44.0)
