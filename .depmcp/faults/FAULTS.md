# depmcp faults found in itself

Generated from `issues.jsonl` and `resolved.jsonl`; do not edit by hand.
Read the open entries, fix them, then record a resolution with `depmcp faults resolve <id>`.
A fault seen again after its resolution reopens automatically.

0 open, 5 resolved.

## Resolved

- DEP-435CC37F — OSV fixed versions without a v prefix were dropped by policy (resolved 2026-11-08T06:51:42.207263797Z: fixed in v0.9.2 by normalizing the v prefix in gomod.Compare and Classify)
- DEP-A569750B — no panic recovery at MCP tool boundaries (resolved 2026-11-08T08:54:00.344768791Z: every tool now registers through addTool, which recovers a panic, records a KindInternal fault with its stack, and returns INTERNAL_ERROR as an isError result; tested in internal/mcpserver/panic_internal_test.go)
- DEP-9D45551D — yarn berry is refused rather than supported (resolved 2026-11-08T08:54:00.380526834Z: v2eco.Berry reads a berry lockfile (__metadata, @npm: descriptors, multi-descriptor keys) with yarn up for apply; yarn v1 now declines berry files so the two never misread each other)
- DEP-9DE3A533 — scheduled runs ignore auto_pr (resolved 2026-11-08T08:54:00.368905871Z: RunOnce now publishes through the configured forge when defaults.auto_pr is true, reporting the PR url or the local report path; tested in internal/app/autopr_test.go)
- DEP-B8A77617 — advisory target version is recorded without the v prefix (resolved 2026-11-08T08:54:00.356896Z: ecosystem.VersionNormalizer added; gomod implements it and PlanUpdates normalizes every decision's target, so an advisory fix reads v0.44.0)
